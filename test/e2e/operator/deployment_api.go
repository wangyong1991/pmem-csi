/*
Copyright 2019 Intel Corporation.

SPDX-License-Identifier: Apache-2.0
*/

package operator

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	alphaapi "github.com/intel/pmem-csi/pkg/apis/pmemcsi/v1alpha1"
	api "github.com/intel/pmem-csi/pkg/apis/pmemcsi/v1beta1"
	"github.com/intel/pmem-csi/pkg/exec"
	"github.com/intel/pmem-csi/pkg/k8sutil"
	"github.com/intel/pmem-csi/pkg/pmem-csi-operator/controller/deployment/testcases"
	"github.com/intel/pmem-csi/pkg/version"
	"github.com/intel/pmem-csi/test/e2e/deploy"
	"github.com/intel/pmem-csi/test/e2e/operator/validate"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	storagev1beta1 "k8s.io/api/storage/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epv "k8s.io/kubernetes/test/e2e/framework/pv"
	runtime "sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

// We use intentionally use this non-existing driver image
// because these tests do not actually need a running driver.
const dummyImage = "unexisting/pmem-csi-driver"

func getDeployment(name string) api.Deployment {
	return api.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: api.DeploymentSpec{
			Image: dummyImage,
		},
	}
}

var _ = deploy.DescribeForSome("API", func(d *deploy.Deployment) bool {
	// Run these tests for all bare operator deployments, i.e.
	// those which did not already install the driver.
	return d.HasOperator && !d.HasDriver
}, func(d *deploy.Deployment) {
	var (
		c          *deploy.Cluster
		ctx        context.Context
		cancel     func()
		client     runtime.Client
		k8sver     version.Version
		evWatcher  watch.Interface
		evCaptured map[types.UID]map[string]struct{}
	)

	f := framework.NewDefaultFramework("operator")
	// test/e2e/deploy.go is using default namespace for deploying operator.
	// So we could skip default namespace creation/deletion steps
	f.SkipNamespaceCreation = true

	BeforeEach(func() {
		Expect(f).ShouldNot(BeNil(), "framework init")
		cluster, err := deploy.NewCluster(f.ClientSet, f.DynamicClient)
		Expect(err).ShouldNot(HaveOccurred(), "new cluster")
		c = cluster

		client, err = runtime.New(f.ClientConfig(), runtime.Options{})
		Expect(err).ShouldNot(HaveOccurred(), "new operator runtime client")

		ver, err := k8sutil.GetKubernetesVersion(f.ClientConfig())
		Expect(err).ShouldNot(HaveOccurred(), "get Kubernetes version")
		k8sver = *ver

		evWatcher, err = f.ClientSet.CoreV1().Events("").Watch(context.TODO(), metav1.ListOptions{})
		Expect(err).ShouldNot(HaveOccurred(), "get events watcher")
		evCaptured = map[types.UID]map[string]struct{}{}
		go func() {
			for watchEvent := range evWatcher.ResultChan() {
				ev, ok := watchEvent.Object.(*corev1.Event)
				if !ok || ev.Source.Component != "pmem-csi-operator" {
					continue
				}
				if _, ok := evCaptured[ev.InvolvedObject.UID]; !ok {
					evCaptured[ev.InvolvedObject.UID] = map[string]struct{}{}
				}
				evCaptured[ev.InvolvedObject.UID][ev.Reason] = struct{}{}
			}
		}()

		// All tests are expected to complete in 5 minutes.
		// We need to set up the global variables indirectly to avoid a warning about cancel not being called.
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Minute)
		ctx, cancel = ctx2, cancel2
	})

	AfterEach(func() {
		evWatcher.Stop()
		evCaptured = nil
		cancel()
	})

	validateDriver := func(deployment api.Deployment, what ...interface{}) {
		framework.Logf("waiting for expected driver deployment %s", deployment.Name)
		if what == nil {
			what = []interface{}{"validate driver"}
		}

		// We cannot check for unexpected object modifications
		// by the operator during E2E testing because the app
		// controllers themselves will also modify the same
		// objects with status changes. We can only test
		// that during unit testing.
		initialCreation := false

		framework.ExpectNoErrorWithOffset(1, validate.DriverDeploymentEventually(ctx, client, k8sver, d.Namespace, deployment, initialCreation), what...)
		framework.Logf("got expected driver deployment %s", deployment.Name)
	}

	validateConditions := func(depName string, expected map[api.DeploymentConditionType]corev1.ConditionStatus, what ...interface{}) {
		if what == nil {
			what = []interface{}{"validate driver(%s) status conditions", depName}
		}
		dep := deploy.GetDeploymentCR(f, depName)
		actual := dep.Status.Conditions
		ExpectWithOffset(1, len(actual)).Should(BeEquivalentTo(len(expected)), what...)
		for _, c := range actual {
			ExpectWithOffset(2, expected[c.Type]).Should(BeEquivalentTo(c.Status))
		}
	}

	validateEvents := func(dep *api.Deployment, expectedEvents []string, what ...interface{}) {
		if what == nil {
			what = []interface{}{"validate events"}
		}
		expected := map[string]struct{}{}
		for _, r := range expectedEvents {
			expected[r] = struct{}{}
		}
		Eventually(func() bool {
			reasons := map[string]struct{}{}
			ok := false
			if reasons, ok = evCaptured[dep.UID]; !ok {
				// No event captured for this object
				return false
			}
			for r := range reasons {
				if _, ok := expected[r]; ok {
					delete(expected, r)
				}
			}
			return len(expected) == 0
		}, 2*time.Minute, time.Second, what, ": ", expected)
	}

	ensureObjectRecovered := func(obj apiruntime.Object) {
		meta, err := meta.Accessor(obj)
		Expect(err).ShouldNot(HaveOccurred(), "get meta object")
		framework.Logf("Waiting for deleted object recovered %T/%s", obj, meta.GetName())
		key := runtime.ObjectKey{Name: meta.GetName(), Namespace: meta.GetNamespace()}
		Eventually(func() error {
			return client.Get(context.TODO(), key, obj)
		}, "2m", "1s").ShouldNot(HaveOccurred(), "failed to recover object")
		framework.Logf("Object %T/%s recovered", obj, meta.GetName())
	}

	Context("deployment", func() {

		tests := map[string]api.Deployment{
			"with defaults": getDeployment("test-deployment-with-defaults"),
			"with explicit values": {
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-deployment-with-explicit",
				},
				Spec: api.DeploymentSpec{
					DeviceMode: api.DeviceModeDirect,
					PullPolicy: corev1.PullNever,
					Image:      dummyImage,
					ControllerDriverResources: &corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("200m"),
							corev1.ResourceMemory: resource.MustParse("100Mi"),
						},
					},
					NodeDriverResources: &corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("500Mi"),
						},
					},
					ProvisionerResources: &corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("210m"),
							corev1.ResourceMemory: resource.MustParse("110Mi"),
						},
					},
					NodeRegistrarResources: &corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("300m"),
							corev1.ResourceMemory: resource.MustParse("100Mi"),
						},
					},
				},
			},
		}

		for name, deployment := range tests {
			deployment := deployment
			It(name, func() {
				deployment = deploy.CreateDeploymentCR(f, deployment)
				defer deploy.DeleteDeploymentCR(f, deployment.Name)
				validateDriver(deployment)
				validateConditions(deployment.Name, map[api.DeploymentConditionType]corev1.ConditionStatus{
					api.CertsReady:     corev1.ConditionTrue,
					api.DriverDeployed: corev1.ConditionTrue,
				})
				validateEvents(&deployment, []string{api.EventReasonNew, api.EventReasonRunning})
			})
		}

		It("get deployment shall list expected fields", func() {
			lblKey := "storage"
			lblValue := "unknown-node"
			deployment := getDeployment("test-get-deployment-fields")
			// Only values that are visible in Deployment CR are shown in `kubectl get`
			// but, not the default values chosen by the operator.
			// So provide the values that are expected to list.
			deployment.Spec.DeviceMode = api.DeviceModeDirect
			deployment.Spec.PullPolicy = corev1.PullNever
			deployment.Spec.Image = dummyImage
			deployment.Spec.NodeSelector = map[string]string{
				lblKey: lblValue,
			}

			deployment = deploy.CreateDeploymentCR(f, deployment)
			defer deploy.DeleteDeploymentCR(f, deployment.Name)
			validateDriver(deployment, "validate driver")

			d := deploy.GetDeploymentCR(f, deployment.Name)

			// Run in-cluster kubectl from master node
			ssh := os.Getenv("REPO_ROOT") + "/_work/" + os.Getenv("CLUSTER") + "/ssh.0"
			out, err := exec.RunCommand(ssh, "kubectl", "get", "deployments.pmem-csi.intel.com", "--no-headers")
			Expect(err).ShouldNot(HaveOccurred(), "kubectl get: %v", out)
			Expect(out).Should(MatchRegexp(`%s\s+%s\s+.*"?%s"?:"?%s"?.*\s+%s\s+%s\s+[0-9]+(s|m)`,
				d.Name, d.Spec.DeviceMode, lblKey, lblValue, d.Spec.Image, d.Status.Phase), "fields mismatch")
		})

		It("driver image shall default to operator image", func() {
			deployment := getDeployment("test-deployment-driver-image")
			deployment.Spec.Image = ""
			deployment.Spec.PMEMPercentage = 50

			deployment = deploy.CreateDeploymentCR(f, deployment)
			defer deploy.DeleteDeploymentCR(f, deployment.Name)

			operatorPod := deploy.WaitForOperator(c, d.Namespace)

			// operator image should be the driver image
			deployment.Spec.Image = operatorPod.Spec.Containers[0].Image
			validateDriver(deployment)
		})

		It("shall be able to edit running deployment", func() {
			deployment := getDeployment("test-deployment-update")

			deployment = deploy.CreateDeploymentCR(f, deployment)
			defer deploy.DeleteDeploymentCR(f, deployment.Name)
			validateDriver(deployment, "validate driver before editing")

			// We have to get a fresh copy before updating it because the
			// operator should have modified the status.
			deployment = deploy.GetDeploymentCR(f, deployment.Name)

			// Update fields.
			spec := &deployment.Spec
			spec.LogLevel++
			spec.Image = "test-driver-image"
			spec.PullPolicy = corev1.PullNever
			spec.ProvisionerImage = "test-provisioner"
			spec.ControllerDriverResources = &corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("100Mi"),
				},
			}
			spec.NodeDriverResources = &corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("500Mi"),
				},
			}
			spec.ProvisionerResources = &corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("300m"),
					corev1.ResourceMemory: resource.MustParse("300Mi"),
				},
			}
			spec.NodeRegistrarResources = &corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("200Mi"),
				},
			}
			testcases.SetTLSOrDie(spec)

			deployment = deploy.UpdateDeploymentCR(f, deployment)

			validateDriver(deployment, "validate driver after editing")
		})

		It("shall allow multiple deployments", func() {
			deployment1 := getDeployment("test-deployment-1")
			deployment2 := getDeployment("test-deployment-2")

			deployment1 = deploy.CreateDeploymentCR(f, deployment1)
			defer deploy.DeleteDeploymentCR(f, deployment1.Name)
			validateDriver(deployment1, "validate driver #1")
			validateEvents(&deployment1, []string{api.EventReasonNew, api.EventReasonRunning})

			deployment2 = deploy.CreateDeploymentCR(f, deployment2)
			defer deploy.DeleteDeploymentCR(f, deployment2.Name)
			validateDriver(deployment2, true /* TODO 2 */, "validate driver #2")
			validateEvents(&deployment2, []string{api.EventReasonNew, api.EventReasonRunning})
		})

		It("shall support dots in the name", func() {
			deployment1 := getDeployment("test.deployment.example.org")

			deployment1 = deploy.CreateDeploymentCR(f, deployment1)
			defer deploy.DeleteDeploymentCR(f, deployment1.Name)
			validateDriver(deployment1, "validate driver")
		})

		It("shall be able to use custom CA certificates", func() {
			deployment := getDeployment("test-deployment-with-certificates")
			testcases.SetTLSOrDie(&deployment.Spec)

			deployment = deploy.CreateDeploymentCR(f, deployment)
			defer deploy.DeleteDeploymentCR(f, deployment.Name)
			validateDriver(deployment, true)
			validateConditions(deployment.Name, map[api.DeploymentConditionType]corev1.ConditionStatus{
				api.CertsReady:     corev1.ConditionTrue,
				api.CertsVerified:  corev1.ConditionTrue,
				api.DriverDeployed: corev1.ConditionTrue,
			})
			validateEvents(&deployment, []string{api.EventReasonNew, api.EventReasonRunning})
		})

		It("driver deployment shall be running even after operator exit", func() {
			deployment := getDeployment("test-deployment-operator-exit")

			deployment = deploy.CreateDeploymentCR(f, deployment)

			defer deploy.DeleteDeploymentCR(f, deployment.Name)
			validateDriver(deployment, true)
			validateConditions(deployment.Name, map[api.DeploymentConditionType]corev1.ConditionStatus{
				api.CertsReady:     corev1.ConditionTrue,
				api.DriverDeployed: corev1.ConditionTrue,
			})

			// Stop the operator
			stopOperator(c, d)
			// restore the deployment so that next test should  not effect
			defer startOperator(c, d)

			// Ensure that the driver is running consistently
			resourceVersions := map[string]string{}
			Consistently(func() error {
				final, err := validate.DriverDeployment(client, k8sver, d.Namespace, deployment, resourceVersions)
				if final {
					framework.Failf("final error during driver validation after restarting: %v", err)
				}
				return err
			}, "1m", "20s").ShouldNot(HaveOccurred(), "driver validation failure after restarting")
		})

		It("shall recover from conflicts", func() {
			deployment := getDeployment("test-recover-from-conflicts")
			sec := &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Secret",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      deployment.GetHyphenedName() + "-registry-secrets",
					Namespace: d.Namespace,
				},
				Type: corev1.SecretTypeTLS,
				Data: map[string][]byte{
					"ca.crt":  []byte("fake ca"),
					"tls.key": []byte("fake key"),
					"tls.crt": []byte("fake crt"),
				},
			}
			deleteSecret := func(name string) {
				Eventually(func() error {
					err := f.ClientSet.CoreV1().Secrets(d.Namespace).Delete(context.Background(), name, metav1.DeleteOptions{})
					deploy.LogError(err, "Delete secret error: %v, will retry...", err)
					if errors.IsNotFound(err) {
						return nil
					}
					return err
				}, "3m", "1s").ShouldNot(HaveOccurred(), "delete secret %q", name)
			}
			Eventually(func() error {
				_, err := f.ClientSet.CoreV1().Secrets(d.Namespace).Create(context.Background(), sec, metav1.CreateOptions{})
				deploy.LogError(err, "create secret error: %v, will retry...", err)
				return err
			}, "3m", "1s").ShouldNot(HaveOccurred(), "create secret %q", sec.Name)
			defer deleteSecret(sec.Name)

			deployment = deploy.CreateDeploymentCR(f, deployment)
			defer deploy.DeleteDeploymentCR(f, deployment.Name)

			// The deployment should fail to create required secret(s) as it already
			// exists and is owned by others.
			Eventually(func() bool {
				out := deploy.GetDeploymentCR(f, deployment.Name)
				return out.Status.Phase == api.DeploymentPhaseFailed
			}, "3m", "1s").Should(BeTrue(), "deployment should fail %q", deployment.Name)
			validateEvents(&deployment, []string{api.EventReasonNew, api.EventReasonFailed})

			// Deleting the existing secret should make the deployment succeed.
			deleteSecret(sec.Name)
			validateDriver(deployment, true)
			validateEvents(&deployment, []string{api.EventReasonNew, api.EventReasonRunning})
		})
	})

	Context("switch device mode", func() {
		postSwitchFuncs := map[string]func(from, to api.DeviceMode, depName string, pvc *corev1.PersistentVolumeClaim){
			"delete volume": func(from, to api.DeviceMode, depName string, pvc *corev1.PersistentVolumeClaim) {
				// Delete Volume created in `from` device mode
				deletePVC(f, pvc.Namespace, pvc.Name)
			},
			"use volume": func(from, to api.DeviceMode, depName string, pvc *corev1.PersistentVolumeClaim) {
				// Switch back to original device mode
				switchDeploymentMode(c, f, depName, d.Namespace, from)

				// Now try using the volume
				app := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "switch-mode-app",
						Namespace: corev1.NamespaceDefault,
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:            "test-driver",
								Image:           os.Getenv("PMEM_CSI_IMAGE"),
								ImagePullPolicy: corev1.PullIfNotPresent,
								Command:         []string{"sleep", "180"},
							},
						},
						Volumes: []corev1.Volume{
							{
								Name: "pmem-volume",
								VolumeSource: corev1.VolumeSource{
									PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
										ClaimName: pvc.Name,
									},
								},
							},
						},
					},
				}

				By(fmt.Sprintf("Starting application pod '%s'", app.Name))
				Eventually(func() error {
					_, err := f.ClientSet.CoreV1().Pods(app.Namespace).Create(context.Background(), app, metav1.CreateOptions{})
					deploy.LogError(err, "create pod %q error: %v, will retry...", app.Name, err)
					return err
				}, "3m", "1s").ShouldNot(HaveOccurred(), "create pod %q", app.Name)

				defer func() {
					By(fmt.Sprintf("Stopping application pod '%s'", app.Name))
					Eventually(func() error {
						err := f.ClientSet.CoreV1().Pods(app.Namespace).Delete(context.Background(), app.Name, metav1.DeleteOptions{})
						if err != nil && errors.IsNotFound(err) {
							return nil
						}
						deploy.LogError(err, "delete pod %q error: %v, will retry...", app.Name, err)
						return err
					}, "3m", "1s").ShouldNot(HaveOccurred(), "delete pod %q", app.Name)
				}()

				By(fmt.Sprintf("Ensure application pod '%s' is running", app.Name))
				Eventually(func() error {
					pod, err := f.ClientSet.CoreV1().Pods(app.Namespace).Get(context.Background(), app.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					if pod.Status.Phase != corev1.PodRunning {
						return fmt.Errorf("%s: status %v", pod.Name, pod.Status.Phase)
					}
					return nil
				}, "3m", "1s").ShouldNot(HaveOccurred(), "pod read %q", app.Name)
			},
		}

		defineSwitchModeTests := func(ctx string, from, to api.DeviceMode) {
			for name, postSwitch := range postSwitchFuncs {
				Context(ctx, func() {
					name := name
					postSwitch := postSwitch
					It(name, func() {
						driverName := ctx + "-" + strings.Replace(name, " ", "-", -1)
						deployment := api.Deployment{
							ObjectMeta: metav1.ObjectMeta{
								Name: driverName,
							},
							Spec: api.DeploymentSpec{
								DeviceMode:     from,
								PMEMPercentage: 50,
								NodeSelector: map[string]string{
									// Provided by NFD.
									"feature.node.kubernetes.io/memory-nv.dax": "true",
								},
							},
						}

						deployment = deploy.CreateDeploymentCR(f, deployment)
						defer deploy.DeleteDeploymentCR(f, deployment.Name)
						deploy.WaitForPMEMDriver(c, deployment.Name,
							&deploy.Deployment{
								Namespace: d.Namespace,
							})
						validateDriver(deployment, true)

						// NOTE(avalluri): As the current operator does not support deploying
						// the driver in 'testing' mode, we cannot directely access CSI
						// interface of it. Hence, using SC/PVC for creating volumes.
						//
						// Once we add "-testing" support we could simplify the code
						// by using controller's CSI interface to create/delete/publish
						// the test volume.

						sc := createStorageClass(f, "switch-mode-sc", driverName)
						defer deleteStorageClass(f, sc.Name)

						pvc := createPVC(f, corev1.NamespaceDefault, "switch-mode-pvc", sc.Name)
						defer deletePVC(f, pvc.Namespace, pvc.Name)

						// Wait till a volume get provisioned for this claim
						err := e2epv.WaitForPersistentVolumeClaimPhase(corev1.ClaimBound, f.ClientSet, pvc.Namespace, pvc.Name, framework.Poll, framework.ClaimProvisionTimeout)
						Expect(err).NotTo(HaveOccurred(), "Persistent volume claim bound failure")

						pvc, err = f.ClientSet.CoreV1().PersistentVolumeClaims(pvc.Namespace).Get(context.Background(), pvc.Name, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred(), "failed to get updated volume claim: %q", pvc.Name)
						framework.Logf("PVC '%s', Volume Ref: %s", pvc.Name, pvc.Spec.VolumeName)

						// Switch device mode
						deployment = switchDeploymentMode(c, f, deployment.Name, d.Namespace, to)

						postSwitch(from, to, driverName, pvc)

						deletePVC(f, pvc.Namespace, pvc.Name)
					})
				})
			}
		}

		defineSwitchModeTests("lvm-to-direct", api.DeviceModeLVM, api.DeviceModeDirect)
		defineSwitchModeTests("direct-to-lvm", api.DeviceModeDirect, api.DeviceModeLVM)
	})

	Context("updating", func() {
		for _, testcase := range testcases.UpdateTests() {
			testcase := testcase
			Context(testcase.Name, func() {
				testIt := func(restart bool) {
					deployment := *testcase.Deployment.DeepCopyObject().(*api.Deployment)

					// Use fake images to prevent pods from actually starting.
					deployment.Spec.Image = dummyImage
					deployment.Spec.NodeRegistrarImage = dummyImage
					deployment.Spec.ProvisionerImage = dummyImage

					deployment = deploy.CreateDeploymentCR(f, deployment)
					defer deploy.DeleteDeploymentCR(f, deployment.Name)
					validateDriver(deployment, "validate driver before update")

					// We have to get a fresh copy before updating it because the
					// operator should have modified the status, and only the status.
					modifiedDeployment := deploy.GetDeploymentCR(f, deployment.Name)
					Expect(modifiedDeployment.Spec).To(Equal(deployment.Spec), "spec unmodified")
					Expect(modifiedDeployment.Status.Phase).To(Equal(api.DeploymentPhaseRunning), "deployment phase")

					restored := false
					if restart {
						stopOperator(c, d)
						defer func() {
							if !restored {
								startOperator(c, d)
							}
						}()
					}

					testcase.Mutate(&modifiedDeployment)
					deployment = deploy.UpdateDeploymentCR(f, modifiedDeployment)

					if restart {
						startOperator(c, d)
						restored = true
					}

					validateDriver(modifiedDeployment, "validate driver after update and restart")
				}

				It("while running", func() {
					testIt(false)
				})

				It("while stopped", func() {
					testIt(true)
				})
			})
		}
	})

	Context("recover", func() {
		Context("deleted sub-resources", func() {
			tests := map[string]func(*api.Deployment) apiruntime.Object{
				"registry secret": func(dep *api.Deployment) apiruntime.Object {
					return &corev1.Secret{
						ObjectMeta: metav1.ObjectMeta{
							Name: dep.RegistrySecretName(), Namespace: d.Namespace,
						},
					}
				},
				"node secret": func(dep *api.Deployment) apiruntime.Object {
					return &corev1.Secret{
						ObjectMeta: metav1.ObjectMeta{Name: dep.NodeSecretName(), Namespace: d.Namespace},
					}
				},
				"service account": func(dep *api.Deployment) apiruntime.Object {
					return &corev1.ServiceAccount{
						ObjectMeta: metav1.ObjectMeta{Name: dep.ServiceAccountName(), Namespace: d.Namespace},
					}
				},
				"controller service": func(dep *api.Deployment) apiruntime.Object {
					return &corev1.Service{
						ObjectMeta: metav1.ObjectMeta{Name: dep.ControllerServiceName(), Namespace: d.Namespace},
					}
				},
				"metrics service": func(dep *api.Deployment) apiruntime.Object {
					return &corev1.Service{
						ObjectMeta: metav1.ObjectMeta{Name: dep.MetricsServiceName(), Namespace: d.Namespace},
					}
				},
				"provisioner role": func(dep *api.Deployment) apiruntime.Object {
					return &rbacv1.Role{
						ObjectMeta: metav1.ObjectMeta{Name: dep.ProvisionerRoleName(), Namespace: d.Namespace},
					}
				},
				"provisioner role binding": func(dep *api.Deployment) apiruntime.Object {
					return &rbacv1.RoleBinding{
						ObjectMeta: metav1.ObjectMeta{Name: dep.ProvisionerRoleBindingName(), Namespace: d.Namespace},
					}
				},
				"provisioner cluster role": func(dep *api.Deployment) apiruntime.Object {
					return &rbacv1.ClusterRole{
						ObjectMeta: metav1.ObjectMeta{Name: dep.ProvisionerClusterRoleName()},
					}
				},
				"provisioner cluster role binding": func(dep *api.Deployment) apiruntime.Object {
					return &rbacv1.ClusterRoleBinding{
						ObjectMeta: metav1.ObjectMeta{Name: dep.ProvisionerClusterRoleBindingName()},
					}
				},
				"csi driver": func(dep *api.Deployment) apiruntime.Object {
					return &storagev1beta1.CSIDriver{
						ObjectMeta: metav1.ObjectMeta{Name: dep.GetName()},
					}
				},
				"controller driver": func(dep *api.Deployment) apiruntime.Object {
					return &appsv1.StatefulSet{
						ObjectMeta: metav1.ObjectMeta{Name: dep.ControllerDriverName(), Namespace: d.Namespace},
					}
				},
				"node driver": func(dep *api.Deployment) apiruntime.Object {
					return &appsv1.DaemonSet{
						ObjectMeta: metav1.ObjectMeta{Name: dep.NodeDriverName(), Namespace: d.Namespace},
					}
				},
			}

			delete := func(obj apiruntime.Object) {
				meta, err := meta.Accessor(obj)
				Expect(err).ShouldNot(HaveOccurred(), "get meta object")
				Eventually(func() error {
					err := client.Delete(context.TODO(), obj)
					if err == nil || errors.IsNotFound(err) {
						return nil
					}
					return err
				}, "3m", "1s").ShouldNot(HaveOccurred(), "delete object '%T/%s", obj, meta.GetName())
				framework.Logf("Deleted object %T/%s", obj, meta.GetName())
			}
			for name, getter := range tests {
				name, getter := name, getter
				It(name, func() {
					dep := getDeployment("recover-" + strings.ReplaceAll(name, " ", "-"))
					deployment := deploy.CreateDeploymentCR(f, dep)
					defer deploy.DeleteDeploymentCR(f, dep.Name)
					validateDriver(deployment)

					obj := getter(&dep)
					delete(obj)
					ensureObjectRecovered(obj)
					validateDriver(deployment, "restore deleted registry secret")
				})
			}
		})

		Context("conflicting update", func() {
			tests := map[string]func(dep *api.Deployment) apiruntime.Object{
				"controller": func(dep *api.Deployment) apiruntime.Object {
					obj := &appsv1.StatefulSet{}
					key := runtime.ObjectKey{Name: dep.ControllerDriverName(), Namespace: d.Namespace}
					EventuallyWithOffset(1, func() error {
						return client.Get(context.TODO(), key, obj)
					}, "2m", "1s").ShouldNot(HaveOccurred(), "get stateful set")

					for i, container := range obj.Spec.Template.Spec.Containers {
						if container.Name == "pmem-driver" {
							obj.Spec.Template.Spec.Containers[i].Command = []string{"malformed", "options"}
							break
						}
					}
					return obj
				},
				"node driver": func(dep *api.Deployment) apiruntime.Object {
					obj := &appsv1.DaemonSet{}
					key := runtime.ObjectKey{Name: dep.NodeDriverName(), Namespace: d.Namespace}
					EventuallyWithOffset(1, func() error {
						return client.Get(context.TODO(), key, obj)
					}, "2m", "1s").ShouldNot(HaveOccurred(), "get daemon set")

					for i, container := range obj.Spec.Template.Spec.Containers {
						if container.Name == "pmem-driver" {
							obj.Spec.Template.Spec.Containers[i].Command = []string{"malformed", "options"}
							break
						}
					}
					return obj
				},
				"metrics service": func(dep *api.Deployment) apiruntime.Object {
					obj := &corev1.Service{}
					key := runtime.ObjectKey{Name: dep.MetricsServiceName(), Namespace: d.Namespace}
					EventuallyWithOffset(1, func() error {
						return client.Get(context.TODO(), key, obj)
					}, "2m", "1s").ShouldNot(HaveOccurred(), "get metrics service set")
					obj.Spec.Ports = []corev1.ServicePort{
						{
							Port: 1111,
							TargetPort: intstr.IntOrString{
								IntVal: 1111,
							},
						},
					}
					return obj
				},
				"controller service": func(dep *api.Deployment) apiruntime.Object {
					obj := &corev1.Service{}
					key := runtime.ObjectKey{Name: dep.ControllerServiceName(), Namespace: d.Namespace}
					EventuallyWithOffset(1, func() error {
						return client.Get(context.TODO(), key, obj)
					}, "2m", "1s").ShouldNot(HaveOccurred(), "get metrics service set")

					obj.Spec.Ports = []corev1.ServicePort{
						{
							Port: 1111,
							TargetPort: intstr.IntOrString{
								IntVal: 1111,
							},
						},
					}
					return obj
				},
			}
			for name, mutate := range tests {
				name, mutate := name, mutate
				It(name, func() {
					dep := getDeployment("recover-" + strings.ReplaceAll(name, " ", "-"))
					deployment := deploy.CreateDeploymentCR(f, dep)
					defer deploy.DeleteDeploymentCR(f, dep.Name)
					validateDriver(deployment)

					obj := mutate(&deployment)
					Eventually(func() error {
						err := client.Update(context.TODO(), obj)
						if err != nil && errors.IsConflict(err) {
							obj = mutate(&deployment)
						}
						return err
					}, "2m", "1s").ShouldNot(HaveOccurred(), "update: %s", name)

					validateDriver(deployment, fmt.Sprintf("recovered %s", name))
				})
			}
		})
	})

	Context("conversion", func() {
		getAlphaDeployment := func(name string) alphaapi.Deployment {
			return alphaapi.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name: name,
				},
				Spec: alphaapi.DeploymentSpec{
					Image: dummyImage,
				},
			}
		}
		It("with default values", func() {
			alphaDep := getAlphaDeployment("alpha-default-values")
			deploy.CreateAlphaDeploymentCR(f, alphaDep)

			deployment := deploy.GetDeploymentCR(f, alphaDep.Name)

			defer deploy.DeleteDeploymentCR(f, deployment.Name)

			validateDriver(deployment, true)

			// Expect to ruturn alpha object converting from stored beta CR
			alphaCR := deploy.GetAlphaDeploymentCR(f, deployment.Name)
			betaCR := deploy.GetDeploymentCR(f, deployment.Name)
			Expect(alphaCR).ShouldNot(BeNil(), "get alpha CR")
			Expect(alphaCR.Spec).Should(BeEquivalentTo(alphaDep.Spec), "alpha CR spec mismatch")
			Expect(alphaCR.Status).Should(BeEquivalentTo(betaCR.Status), "alpha CR status mismatch")
		})
		It("with explicit values", func() {
			alphaDep := getAlphaDeployment("alpha-explicit-values")
			alphaDep.Spec.NodeResources = &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("10m"),
					corev1.ResourceMemory: resource.MustParse("25Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("50Mi"),
				},
			}
			alphaDep.Spec.ControllerResources = &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("20m"),
					corev1.ResourceMemory: resource.MustParse("50Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("100Mi"),
				},
			}
			deploy.CreateAlphaDeploymentCR(f, alphaDep)

			deployment := deploy.GetDeploymentCR(f, alphaDep.Name)

			if d.HasOLM {
				// OLM not yet support conversion webhooks, hence
				// explicit conversion of version incompatible fields
				// is not supported and they get ignored in default
				// conversion provided by the API server.
				alphaDep.Spec.NodeResources = nil
				alphaDep.Spec.ControllerResources = nil
			}
			Expect(deployment.Spec.NodeDriverResources).Should(BeEquivalentTo(alphaDep.Spec.NodeResources), "node driver resources")
			Expect(deployment.Spec.ControllerDriverResources).Should(BeEquivalentTo(alphaDep.Spec.ControllerResources), "controller driver resources")

			defer deploy.DeleteDeploymentCR(f, deployment.Name)

			validateDriver(deployment, true)

			// Expect to ruturn alpha object converting from stored beta CR
			alphaCR := deploy.GetAlphaDeploymentCR(f, deployment.Name)
			betaCR := deploy.GetDeploymentCR(f, deployment.Name)
			Expect(alphaCR).ShouldNot(BeNil(), "get alpha CR")
			Expect(alphaCR.Spec).Should(BeEquivalentTo(alphaDep.Spec), "alpha CR spec mismatch")
			Expect(alphaCR.Status).Should(BeEquivalentTo(betaCR.Status), "alpha CR status mismatch")
		})
	})
})

func validateDeploymentFailure(f *framework.Framework, name string) {
	Eventually(func() bool {
		dep, err := f.DynamicClient.Resource(deploy.DeploymentResource).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			return false
		}

		deployment := deploy.DeploymentFromUnstructured(dep)
		framework.Logf("Deployment %q is in %q phase", deployment.Name, deployment.Status.Phase)
		return deployment.Status.Phase == api.DeploymentPhaseFailed
	}, "3m", "5s").Should(BeTrue(), "deployment %q not running", name)
}

// stopOperator ensures operator deployment replica counter == 0 and the
// operator pod gets deleted
func stopOperator(c *deploy.Cluster, d *deploy.Deployment) error {
	framework.Logf("Decrease operator deployment replicas to 0")
	Eventually(func() bool {
		dep, err := c.ClientSet().AppsV1().Deployments(d.Namespace).Get(context.Background(), "pmem-csi-operator", metav1.GetOptions{})
		if err != nil && !errors.IsNotFound(err) {
			return false
		}

		if *dep.Spec.Replicas == 0 {
			return true
		}

		*dep.Spec.Replicas = 0
		_, err = c.ClientSet().AppsV1().Deployments(dep.Namespace).Update(context.Background(), dep, metav1.UpdateOptions{})
		deploy.LogError(err, "failed update operator's replica counter: %v", err)
		return false
	}, "3m", "1s").Should(BeTrue(), "set operator deployment replicas to 0")

	framework.Logf("Ensure the operator pod got deleted.")

	Eventually(func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, err := c.GetAppInstance(ctx, "pmem-csi-operator", "", d.Namespace)
		deploy.LogError(err, "get operator error: %v, will retry...", err)
		return err != nil && strings.HasPrefix(err.Error(), "no app")
	}, "3m", "1s").Should(BeTrue(), "delete operator pod")

	framework.Logf("Operator pod got deleted!")

	return nil
}

// startOperator ensures the operator deployment counter == 1 and the operator pod
// is in running state
func startOperator(c *deploy.Cluster, d *deploy.Deployment) {
	Eventually(func() bool {
		dep, err := c.ClientSet().AppsV1().Deployments(d.Namespace).Get(context.Background(), "pmem-csi-operator", metav1.GetOptions{})

		deploy.LogError(err, "Failed to get error: %v", err)
		if err != nil {
			return false
		}

		if *dep.Spec.Replicas == 1 {
			return true
		}

		*dep.Spec.Replicas = 1
		_, err = c.ClientSet().AppsV1().Deployments(dep.Namespace).Update(context.Background(), dep, metav1.UpdateOptions{})
		deploy.LogError(err, "failed update operator's replication counter: %v", err)

		return false
	}, "3m", "1s").Should(BeTrue(), "increase operator deployment replicas to 1")

	framework.Logf("Ensure operator pod is ready.")
	deploy.WaitForOperator(c, d.Namespace)
	framework.Logf("Operator is restored!")
}

func switchDeploymentMode(c *deploy.Cluster, f *framework.Framework, depName, ns string, mode api.DeviceMode) api.Deployment {
	podNames := []string{}

	for i := 1; i < c.NumNodes(); i++ {
		Eventually(func() error {
			pod, err := c.GetAppInstance(context.Background(), depName+"-node", c.NodeIP(i), ns)
			if err != nil {
				return err
			}
			podNames = append(podNames, pod.Name)
			return nil
		}, "3m", "1s").ShouldNot(HaveOccurred(), "Get daemonset pods")
	}
	By(fmt.Sprintf("Switching driver mode to '%s'", mode))
	deployment := deploy.GetDeploymentCR(f, depName)
	deployment.Spec.DeviceMode = mode
	deployment = deploy.UpdateDeploymentCR(f, deployment)

	// Wait till all the existing daemonset pods restarted
	for _, pod := range podNames {
		Eventually(func() bool {
			_, err := f.ClientSet.CoreV1().Pods(ns).Get(context.Background(), pod, metav1.GetOptions{})
			if err != nil && errors.IsNotFound(err) {
				return true
			}
			deploy.LogError(err, "Failed to fetch daemon set: %v", err)
			return false
		}, "3m", "1s").Should(BeTrue(), "Pod restart '%s'", pod)
	}

	deploy.WaitForPMEMDriver(c, depName,
		&deploy.Deployment{
			Namespace: ns,
		})

	return deployment
}

func createStorageClass(f *framework.Framework, name, provisioner string) *storagev1.StorageClass {
	reclaim := corev1.PersistentVolumeReclaimDelete
	immediate := storagev1.VolumeBindingImmediate

	// Create storage class
	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Provisioner:       provisioner,
		ReclaimPolicy:     &reclaim,
		VolumeBindingMode: &immediate,
		Parameters: map[string]string{
			"eraseafter": "false",
		},
	}

	EventuallyWithOffset(1, func() error {
		_, err := f.ClientSet.StorageV1().StorageClasses().Create(context.Background(), sc, metav1.CreateOptions{})
		if err == nil || errors.IsAlreadyExists(err) {
			return nil
		}
		deploy.LogError(err, "create storage class error: %v, will retry...", err)
		return err
	}, "3m", "1s").ShouldNot(HaveOccurred(), "create storage class %q", sc.Name)
	framework.Logf("Created storage class %q", sc.Name)

	return sc
}

func deleteStorageClass(f *framework.Framework, name string) {
	EventuallyWithOffset(1, func() error {
		framework.Logf("deleting storage class %q", name)
		err := f.ClientSet.StorageV1().StorageClasses().Delete(context.Background(), name, metav1.DeleteOptions{})
		if err != nil && errors.IsNotFound(err) {
			return nil
		}
		deploy.LogError(err, "delete storage class error: %v, will retry...", err)
		return err
	}, "3m", "1s").ShouldNot(HaveOccurred(), "delete storage class %q", name)
}

func createPVC(f *framework.Framework, namespace, name, storageClassName string) *corev1.PersistentVolumeClaim {
	// Create a volume
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "switch-mode-pvc",
			Namespace: namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &storageClassName,
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("2Gi"),
				},
			},
		},
	}
	EventuallyWithOffset(1, func() error {
		_, err := f.ClientSet.CoreV1().PersistentVolumeClaims(pvc.Namespace).Create(context.Background(), pvc, metav1.CreateOptions{})
		deploy.LogError(err, "create pvc %q error: %v, will retry...", pvc.Name, err)
		return err
	}, "3m", "1s").ShouldNot(HaveOccurred(), "create pvc %q", pvc.Name)
	framework.Logf("Created pvc %q", pvc.Name)

	return pvc
}

func deletePVC(f *framework.Framework, namespace, name string) {
	framework.Logf("Deleting PVC %q", name)
	pvc, err := f.ClientSet.CoreV1().PersistentVolumeClaims(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		ExpectWithOffset(1, errors.IsNotFound(err)).Should(BeTrue(), "Get PVC '%s'", name)
	}

	pvName := pvc.Spec.VolumeName
	framework.Logf("Pv %q bound for PVC %q", pvName, name)

	EventuallyWithOffset(1, func() error {
		err := f.ClientSet.CoreV1().PersistentVolumeClaims(namespace).Delete(context.Background(), name, metav1.DeleteOptions{})
		if err != nil && errors.IsNotFound(err) {
			return nil
		}
		deploy.LogError(err, "delete pvc error: %v, will retry...", err)
		return err
	}, "3m", "1s").ShouldNot(HaveOccurred(), "delete pvc %q", name)
	framework.Logf("PVC deleted %q", name)

	if pvName == "" {
		return
	}
	// Wait till the underlined volume get deleted
	// as we use the reclaim policy delete
	framework.Logf("Waiting for PV %q get deleted", pvName)
	EventuallyWithOffset(1, func() bool {
		_, err := f.ClientSet.CoreV1().PersistentVolumes().Get(context.Background(), pvName, metav1.GetOptions{})
		deploy.LogError(err, "Get PV '%s' error: %v, will retry...", pvName, err)
		if err != nil && errors.IsNotFound(err) {
			return true
		}
		return false
	}, "3m", "1s").Should(BeTrue(), "Get PV '%s'", pvName)
}
