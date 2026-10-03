/*
Copyright 2024 The Beskar7 Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/internal/auth"
)

// D-036. D-031 cut the bearer token's life to five minutes once the host is
// Ready, and did nothing when the run failed: a host whose Beskar7Machine had
// failed terminally, and whose consumerRef still named that machine, kept a
// token that authenticated callbacks and fetched the host's bootstrap data for
// the rest of its mint lifetime, up to an hour. The Beskar7Machine controller
// now gives a failed machine's token the same grace, because the inspector may
// still be retrying its /provision-failed report.
//
// The cut is not made for a host in an Error about its BMC. PROV-1 applies a
// /provisioned report to a host whose BMC failed while it deployed, and the
// report authenticates with this token; cutting it would strand a deployment
// that finished.
//
// These specs drive the real Beskar7MachineReconciler.Reconcile against
// envtest: a machine fails through the controller's own state machine, and the
// pass after that is the one that cuts.
var _ = Describe("A failed run's callback token is cut like a Ready one (D-036)", func() {
	const (
		machineName    = "failed-run-machine"
		hostName       = "failed-run-host"
		dataSecretName = "failed-run-bootstrap-data"
	)

	var (
		ns         *corev1.Namespace
		hostKey    client.ObjectKey
		machineKey client.ObjectKey
		r          *Beskar7MachineReconciler
	)

	newReconciler := func(c client.Client) *Beskar7MachineReconciler {
		return &Beskar7MachineReconciler{
			Client: c, Scheme: k8sClient.Scheme(),
			Log:                  ctrl.Log.WithName("failed-run-token"),
			RedfishClientFactory: reachableBMC(),
			BootstrapURLBase:     "https://callback.example.com:8082",
		}
	}

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "failed-run-token-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: dataSecretName, Namespace: ns.Name},
			Data:       map[string][]byte{bootstrapDataSecretKey: []byte("#cloud-config\n")},
		})).To(Succeed())

		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "failed-run-cluster", Namespace: ns.Name},
			Spec:       clusterv1.ClusterSpec{Paused: ptr.To(false)},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		capiMachine := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: machineName, Namespace: ns.Name,
				Labels: map[string]string{clusterv1.ClusterNameLabel: cluster.Name},
			},
			Spec: clusterv1.MachineSpec{
				ClusterName: cluster.Name,
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: infrav1.GroupVersion.Group, Kind: "Beskar7Machine", Name: machineName,
				},
				Bootstrap: clusterv1.Bootstrap{DataSecretName: ptr.To(dataSecretName)},
			},
		}
		Expect(k8sClient.Create(ctx, capiMachine)).To(Succeed())
		Expect(k8sClient.Create(ctx, &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: machineName, Namespace: ns.Name,
				Labels: map[string]string{clusterv1.ClusterNameLabel: cluster.Name},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine",
					Name: capiMachine.Name, UID: capiMachine.UID,
				}},
			},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL: "https://boot.example.com/inspect",
				TargetImageURL:     "https://boot.example.com/kairos.raw",
				TargetImageDigest:  bootTestDigest,
			},
		})).To(Succeed())

		hostKey = client.ObjectKey{Namespace: ns.Name, Name: hostName}
		machineKey = client.ObjectKey{Namespace: ns.Name, Name: machineName}
		r = newReconciler(k8sClient)
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	getMachine := func() *infrav1.Beskar7Machine {
		machine := &infrav1.Beskar7Machine{}
		Expect(k8sClient.Get(ctx, machineKey, machine)).To(Succeed())
		return machine
	}
	// mint stores credentials for the machine the way triggerInspection would.
	mint := func() bootstrapCredentials {
		Expect(r.ensureBootstrapCredentials(ctx, r.Log, getMachine(), getPhysicalHost(hostKey), time.Now())).To(Succeed())
		return readBootstrapCredentials(getCredentialSecret(hostKey))
	}
	reconcileMachine := func() error {
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: machineKey})
		return err
	}
	// failMachine reconciles until the machine's own state machine has failed
	// it, and stops there: the pass that marks the failure is not the one that
	// cuts the token.
	failMachine := func() *infrav1.Beskar7Machine {
		for range 6 {
			Expect(reconcileMachine()).To(Succeed())
			if machine := getMachine(); isTerminallyFailed(machine) {
				return machine
			}
		}
		Fail("the Beskar7Machine did not fail within 6 reconciles")
		return nil
	}
	setHostError := func(message string) {
		host := getPhysicalHost(hostKey)
		host.Status.State = infrav1.StateError
		host.Status.Ready = false
		host.Status.ErrorMessage = message
		Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
	}
	// runFailedHost creates the host of a run the inspector reported failed:
	// Error, with the message the /provision-failed handler writes.
	runFailedHost := func() {
		provisioningHost(ns.Name, hostName, machineName, infrav1.StateDeploying, nil)
		setHostError(provisionFailedReasonPrefix + "image write failed")
		Expect(provisioningRunFailed(getPhysicalHost(hostKey))).To(BeTrue())
	}
	// boundTo returns Secret data binding a token with the given remaining life
	// and a nonce to the Beskar7Machine named consumer, with the UID recorded.
	boundTo := func(consumer, uid, token string, remaining time.Duration) map[string][]byte {
		data := boundCredentialData(consumer, token, remaining, "bound-nonce", 10*time.Minute)
		if uid != "" {
			data[bootstrapConsumerUIDSecretKey] = []byte(uid)
		}
		return data
	}
	expectCut := func(minted bootstrapCredentials) bootstrapCredentials {
		cut := readBootstrapCredentials(getCredentialSecret(hostKey))
		Expect(cut.tokenExpiresAt).To(BeTemporally("~", time.Now().Add(auth.TokenReadyGrace), 10*time.Second),
			"the grace covers the inspector's retries, and no more")
		Expect(cut.token).To(Equal(minted.token), "the token itself is not replaced")
		Expect(cut.tokenIssuedAt).To(Equal(minted.tokenIssuedAt))
		Expect(cut.nonce).To(Equal(minted.nonce))
		Expect(cut.nonceExpiresAt).To(Equal(minted.nonceExpiresAt))
		Expect(cut.consumer).To(Equal(minted.consumer))
		Expect(cut.consumerUID).To(Equal(minted.consumerUID))
		return cut
	}

	DescribeTable("a machine that has failed terminally has its token cut to the grace, and keeps it",
		func(prepare func(), expectedReason string) {
			prepare()
			minted := mint()
			Expect(minted.tokenExpiresAt).To(BeTemporally(">", time.Now().Add(50*time.Minute)))

			failed := failMachine()
			Expect(conditions.GetReason(failed, infrav1.InfrastructureReadyCondition)).To(Equal(expectedReason))

			Expect(reconcileMachine()).To(Succeed())
			expectCut(minted)
			Expect(isTerminallyFailed(getMachine())).To(BeTrue(), "cutting the token never un-fails the machine")
		},
		Entry("the inspector reported the deployment failed", func() { runFailedHost() }, infrav1.DeploymentFailedReason),
		Entry("the inspection failed on the host", func() {
			provisioningHost(ns.Name, hostName, machineName, infrav1.StateInspecting, nil)
			host := getPhysicalHost(hostKey)
			host.Status.InspectionPhase = infrav1.InspectionPhaseFailed
			Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
		}, infrav1.InspectionFailedReason),
		Entry("the inspection timed out", func() {
			r.InspectionTimeout = 30 * time.Second
			provisioningHost(ns.Name, hostName, machineName, infrav1.StateInspecting, nil)
		}, infrav1.InspectionTimedOutReason),
		Entry("the deployment timed out", func() {
			r.DeploymentTimeout = 30 * time.Second
			provisioningHost(ns.Name, hostName, machineName, infrav1.StateDeploying, nil)
		}, DeploymentTimedOutReason),
	)

	Context("when the inspector retries a report after the run failed", func() {
		It("is still authenticated within the grace, and gets a 401 once it has passed", func() {
			runFailedHost()
			minted := mint()
			failMachine()
			Expect(reconcileMachine()).To(Succeed())
			expectCut(minted)

			srv := httptest.NewServer(func() http.Handler { mux, _ := buildProvisionFailedMux(); return mux }())
			defer srv.Close()
			url := srv.URL + "/api/v1/provision-failed/" + hostKey.Namespace + "/" + hostKey.Name

			code, _ := callbackRequest(http.MethodPost, url, minted.token)
			Expect(code).To(Equal(http.StatusAccepted), "a retry whose first 202 was lost must still land")

			By("the grace running out")
			secret := getCredentialSecret(hostKey)
			secret.Data[bootstrapTokenExpiresAtSecretKey] = credentialTime(time.Now().Add(-time.Minute))
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())
			code, _ = callbackRequest(http.MethodPost, url, minted.token)
			Expect(code).To(Equal(http.StatusUnauthorized))
		})

		It("is mirrored into the host's status by the PhysicalHost reconciler, which the machine controller never writes", func() {
			runFailedHost()
			minted := mint()
			failMachine()
			Expect(reconcileMachine()).To(Succeed())
			cut := expectCut(minted)

			hostReconciler := &PhysicalHostReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				Log:                  ctrl.Log.WithName("failed-run-token-host"),
				Recorder:             record.NewFakeRecorder(10),
				RedfishClientFactory: reachableBMC(),
			}
			host := settlePhysicalHost(hostReconciler, hostKey)
			Expect(host.Status.Bootstrap).NotTo(BeNil())
			Expect(host.Status.Bootstrap.ExpiresAt).NotTo(BeNil())
			Expect(host.Status.Bootstrap.ExpiresAt.Time).To(BeTemporally("~", cut.tokenExpiresAt, time.Second))
		})
	})

	Context("when the host is in an Error about its BMC (PROV-1)", func() {
		It("keeps the token so /provisioned still lands, and cuts it once the host has left the Error", func() {
			interruptedDeployHost(ns.Name, hostName, machineName, nil)
			minted := mint()

			By("the machine failing: a BMC failure that is not an outage is terminal for it")
			failed := failMachine()
			Expect(conditions.GetReason(failed, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.PhysicalHostErrorReason))
			before := getCredentialSecret(hostKey)
			for range 3 {
				Expect(reconcileMachine()).To(Succeed())
			}
			after := getCredentialSecret(hostKey)
			Expect(after.ResourceVersion).To(Equal(before.ResourceVersion), "no cut while the BMC Error holds the host")
			Expect(readBootstrapCredentials(after).tokenExpiresAt).To(BeTemporally(">", time.Now().Add(50*time.Minute)))

			By("the inspector posting /provisioned with its token")
			srv := httptest.NewServer(func() http.Handler { mux, _ := buildProvisionedMux(); return mux }())
			defer srv.Close()
			code, _ := callbackRequest(http.MethodPost,
				srv.URL+"/api/v1/provisioned/"+hostKey.Namespace+"/"+hostKey.Name, minted.token)
			Expect(code).To(Equal(http.StatusAccepted))
			Expect(getPhysicalHost(hostKey).Annotations).To(HaveKeyWithValue(ProvisionedRequestAnnotation, "provisioned"),
				"the report lands: the BMC failure says nothing about the deployment")

			By("the host applying the report")
			hostReconciler := &PhysicalHostReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				Log:                  ctrl.Log.WithName("failed-run-token-host"),
				Recorder:             record.NewFakeRecorder(10),
				RedfishClientFactory: reachableBMC(),
			}
			Expect(settlePhysicalHost(hostReconciler, hostKey).Status.State).To(Equal(infrav1.StateReady))

			By("the failed machine seeing the host out of its Error")
			Expect(reconcileMachine()).To(Succeed())
			expectCut(minted)
		})

		It("does not touch the token of a machine that only waits out a BMC outage", func() {
			interruptedDeployHost(ns.Name, hostName, machineName, nil)
			host := getPhysicalHost(hostKey)
			setFalse(host, infrav1.RedfishConnectionReadyCondition, infrav1.BMCUnreachableReason, "BMC unreachable (connection refused)")
			Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
			mint()
			before := getCredentialSecret(hostKey)

			for range 4 {
				Expect(reconcileMachine()).To(Succeed())
			}
			Expect(isTerminallyFailed(getMachine())).To(BeFalse(), "an outage is not a terminal failure")
			Expect(getCredentialSecret(hostKey).ResourceVersion).To(Equal(before.ResourceVersion))
		})
	})

	Context("when the cut would not be this machine's to make or would not shorten anything", func() {
		DescribeTable("the Secret is left as it is",
			func(data func(machine *infrav1.Beskar7Machine) map[string][]byte) {
				runFailedHost()
				failMachine()
				putCredentialSecret(hostKey, data(getMachine()))
				before := getCredentialSecret(hostKey)

				for range 2 {
					Expect(reconcileMachine()).To(Succeed())
				}
				after := getCredentialSecret(hostKey)
				Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
				Expect(after.Data).To(Equal(before.Data))
			},
			Entry("a token that expires before the grace is not extended to it", func(m *infrav1.Beskar7Machine) map[string][]byte {
				return boundTo(m.Name, string(m.UID), "short-token", 2*time.Minute)
			}),
			Entry("a token whose expiry is missing, which never verifies, is not given one", func(m *infrav1.Beskar7Machine) map[string][]byte {
				data := boundTo(m.Name, string(m.UID), "no-expiry-token", time.Hour)
				delete(data, bootstrapTokenExpiresAtSecretKey)
				return data
			}),
			Entry("credentials with a nonce and no token", func(m *infrav1.Beskar7Machine) map[string][]byte {
				return boundTo(m.Name, string(m.UID), "", 0)
			}),
			Entry("credentials bound to another machine", func(m *infrav1.Beskar7Machine) map[string][]byte {
				return boundTo("another-machine", string(m.UID), "foreign-token", time.Hour)
			}),
			Entry("credentials bound to an earlier machine of the same name", func(m *infrav1.Beskar7Machine) map[string][]byte {
				return boundTo(m.Name, "uid-of-an-earlier-machine", "earlier-token", time.Hour)
			}),
			Entry("credentials that record no machine UID", func(m *infrav1.Beskar7Machine) map[string][]byte {
				return boundTo(m.Name, "", "unattributed-token", time.Hour)
			}),
		)

		It("leaves a Secret the host does not own alone, whoever it names", func() {
			runFailedHost()
			failMachine()
			machine := getMachine()
			Expect(k8sClient.Create(ctx, unownedCredentialSecret(hostKey.Namespace, hostKey.Name,
				boundTo(machine.Name, string(machine.UID), "squatter-token", time.Hour)))).To(Succeed())
			before := getCredentialSecret(hostKey)

			for range 2 {
				Expect(reconcileMachine()).To(Succeed())
			}
			after := getCredentialSecret(hostKey)
			Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
			Expect(after.OwnerReferences).To(BeEmpty(), "never adopted")
		})

		It("has nothing to cut when the failed machine holds no host", func() {
			machine := getMachine()
			setFalse(machine, infrav1.InfrastructureReadyCondition, infrav1.InvalidHostSelectorReason, "no host")
			machine.Status.Phase = ptr.To(infrav1.PhaseFailed)
			Expect(k8sClient.Status().Update(ctx, machine)).To(Succeed())

			Expect(reconcileMachine()).To(Succeed())
			Expect(isTerminallyFailed(getMachine())).To(BeTrue())
		})
	})

	Context("when the machine stays failed over many reconciles", func() {
		It("writes the Secret once, and never moves the expiry", func() {
			runFailedHost()
			minted := mint()
			failMachine()
			Expect(reconcileMachine()).To(Succeed())
			expectCut(minted)
			cut := getCredentialSecret(hostKey)

			for range 4 {
				Expect(reconcileMachine()).To(Succeed())
			}
			again := getCredentialSecret(hostKey)
			Expect(again.ResourceVersion).To(Equal(cut.ResourceVersion))
			Expect(again.Data).To(Equal(cut.Data))
		})

		It("cuts a failure recorded before the cut existed on the first pass that sees it", func() {
			runFailedHost()
			minted := mint()
			failed := failMachine()
			Expect(readBootstrapCredentials(getCredentialSecret(hostKey)).tokenExpiresAt).To(
				BeTemporally(">", time.Now().Add(50*time.Minute)), "an upgraded manager meets a failed machine with its mint-time token")
			Expect(isTerminallyFailed(failed)).To(BeTrue())

			Expect(reconcileMachine()).To(Succeed())
			expectCut(minted)
		})
	})

	Context("when the Secret write fails", func() {
		It("requeues with the error, leaves the machine failed, and cuts on the retry", func() {
			runFailedHost()
			minted := mint()
			failMachine()

			base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			var refused atomic.Bool
			r = newReconciler(interceptor.NewClient(base, interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if _, isSecret := obj.(*corev1.Secret); isSecret && refused.CompareAndSwap(false, true) {
						return errors.New("injected: the Secret write did not land")
					}
					return c.Update(ctx, obj, opts...)
				},
			}))

			Expect(reconcileMachine()).NotTo(Succeed(), "an error requeues the machine with the reconcile's backoff")
			Expect(refused.Load()).To(BeTrue(), "the cut tried to write the Secret")
			failed := getMachine()
			Expect(isTerminallyFailed(failed)).To(BeTrue(), "a failed write does not un-fail the machine")
			Expect(conditions.GetReason(failed, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.DeploymentFailedReason))
			Expect(readBootstrapCredentials(getCredentialSecret(hostKey)).tokenExpiresAt).To(
				BeTemporally(">", time.Now().Add(50*time.Minute)), "nothing was written")

			Expect(reconcileMachine()).To(Succeed())
			expectCut(minted)
			Expect(isTerminallyFailed(getMachine())).To(BeTrue())
		})
	})

	Context("when the host is claimed again after the cut", func() {
		It("never hands the cut token out again, to the same machine or to a new claim", func() {
			runFailedHost()
			minted := mint()
			failMachine()
			Expect(reconcileMachine()).To(Succeed())
			cut := expectCut(minted)

			machine := getMachine()
			Expect(bootstrapTokenReusable(cut, machine.Name, time.Now(), r.bootstrapTokenMinRemaining())).To(BeFalse(),
				"/boot renders the token into a kernel cmdline: five minutes cannot cover a nonce plus an inspection")

			By("the same machine minting again: the token is replaced, the unconsumed nonce is not")
			Expect(r.ensureBootstrapCredentials(ctx, r.Log, machine, getPhysicalHost(hostKey), time.Now())).To(Succeed())
			again := readBootstrapCredentials(getCredentialSecret(hostKey))
			Expect(again.token).NotTo(Equal(minted.token))
			Expect(again.tokenExpiresAt).To(BeTemporally("~", time.Now().Add(auth.TokenLifetime), 10*time.Second))
			Expect(again.nonce).To(Equal(minted.nonce))

			By("a new machine claiming the host")
			second := &infrav1.Beskar7Machine{ObjectMeta: metav1.ObjectMeta{
				Name: "second-failed-run-machine", Namespace: ns.Name, UID: "second-failed-run-machine-uid",
			}}
			Expect(r.ensureBootstrapCredentials(ctx, r.Log, second, getPhysicalHost(hostKey), time.Now())).To(Succeed())
			fresh := readBootstrapCredentials(getCredentialSecret(hostKey))
			Expect(fresh.consumer).To(Equal(second.Name))
			Expect(fresh.consumerUID).To(Equal(string(second.UID)))
			Expect(fresh.token).NotTo(Equal(again.token))
			Expect(fresh.nonce).NotTo(Equal(again.nonce))
			Expect(fresh.tokenExpiresAt).To(BeTemporally("~", time.Now().Add(auth.TokenLifetime), 10*time.Second))
		})
	})
})
