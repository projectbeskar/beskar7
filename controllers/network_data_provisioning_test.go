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
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

const (
	networkDataRunCluster = "netdata-cluster"
	networkDataRunMachine = "netdata-machine"
	networkDataRunHost    = "netdata-host"
	networkDataRunSecret  = "netdata-host-network"
	// networkDataRunWake is well under the machine's one-minute no-host
	// requeue, so a claim inside it proves the host event woke the machine.
	networkDataRunWake = 20 * time.Second
	// networkDataRunHold is how long a refused host must stay unclaimed or
	// unbooted.
	networkDataRunHold = 3 * time.Second
)

// Both reconcilers run under one manager, so NetworkDataReady is set by the
// real PhysicalHost reconciler and the machine reacts to it through its
// watches, not to a condition the test writes.
var _ = Describe("Network data gates the claim and the boot under a running manager", func() {
	var (
		testNs    *corev1.Namespace
		mgrCancel context.CancelFunc
	)

	BeforeEach(func() {
		testNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "network-data-run-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(testNs.Name))).To(Succeed())

		skipNameValidation := true
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 k8sClient.Scheme(),
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{testNs.Name: {}}},
			Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
		})
		Expect(err).NotTo(HaveOccurred())

		bmc := (&bmcGate{reachable: true}).factory()
		Expect((&PhysicalHostReconciler{
			Client:               mgr.GetClient(),
			Scheme:               mgr.GetScheme(),
			Log:                  ctrl.Log.WithName("network-data-run-host"),
			Recorder:             record.NewFakeRecorder(100),
			RedfishClientFactory: bmc,
			BootstrapURLBase:     "https://example.com:8082",
		}).SetupWithManager(mgr)).To(Succeed())
		Expect((&Beskar7MachineReconciler{
			Client:               mgr.GetClient(),
			Scheme:               mgr.GetScheme(),
			Log:                  ctrl.Log.WithName("network-data-run-machine"),
			RedfishClientFactory: bmc,
		}).SetupWithManager(mgr)).To(Succeed())

		var mgrCtx context.Context
		mgrCtx, mgrCancel = context.WithCancel(ctx)
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())
	})

	AfterEach(func() {
		mgrCancel()
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	hostKey := func() client.ObjectKey { return client.ObjectKey{Namespace: testNs.Name, Name: networkDataRunHost} }
	machineKey := func() client.ObjectKey { return client.ObjectKey{Namespace: testNs.Name, Name: networkDataRunMachine} }

	// createNetworkDataSecret creates the host's network data Secret, opted in
	// for annotatedHost.
	createNetworkDataSecret := func(annotatedHost string) {
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: networkDataRunSecret, Namespace: testNs.Name,
				Annotations: map[string]string{NetworkDataHostAnnotation: annotatedHost},
			},
			Data: map[string][]byte{networkDataSecretKey: []byte("network:\n  version: 2\n")},
		})).To(Succeed())
	}

	fixNetworkDataSecret := func() {
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: testNs.Name, Name: networkDataRunSecret}, secret)).To(Succeed())
		base := secret.DeepCopy()
		secret.Annotations[NetworkDataHostAnnotation] = networkDataRunHost
		Expect(k8sClient.Patch(ctx, secret, client.MergeFrom(base))).To(Succeed())
	}

	createHost := func(consumer string) {
		host := &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: networkDataRunHost, Namespace: testNs.Name},
			Spec: infrav1.PhysicalHostSpec{
				RedfishConnection: infrav1.RedfishConnection{
					Address:              "https://mock-redfish.example.invalid:8443",
					CredentialsSecretRef: "bmc-credentials",
				},
				NetworkDataSecretRef: networkDataRunSecret,
			},
		}
		if consumer != "" {
			host = claimedPhysicalHost(testNs.Name, networkDataRunHost, consumer)
			host.Spec.NetworkDataSecretRef = networkDataRunSecret
		}
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
	}

	// createMachine creates the CAPI objects and the Beskar7Machine that
	// provisions the Machine.
	createMachine := func() {
		ns := testNs.Name
		Expect(k8sClient.Create(ctx, &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: networkDataRunCluster, Namespace: ns},
			Spec:       clusterv1.ClusterSpec{Paused: ptr.To(false)},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "netdata-bootstrap", Namespace: ns},
			Data:       map[string][]byte{"value": []byte("#cloud-config\n")},
		})).To(Succeed())
		machine := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: networkDataRunMachine, Namespace: ns,
				Labels: map[string]string{clusterv1.ClusterNameLabel: networkDataRunCluster},
			},
			Spec: clusterv1.MachineSpec{
				ClusterName: networkDataRunCluster,
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: infrav1.GroupVersion.Group, Kind: "Beskar7Machine", Name: networkDataRunMachine,
				},
				Bootstrap: clusterv1.Bootstrap{DataSecretName: ptr.To("netdata-bootstrap")},
			},
		}
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		Expect(k8sClient.Create(ctx, &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: networkDataRunMachine, Namespace: ns,
				Labels: map[string]string{clusterv1.ClusterNameLabel: networkDataRunCluster},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine",
					Name: machine.Name, UID: machine.UID,
				}},
			},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot/inspect.ipxe",
				TargetImageURL:     "http://boot/kairos.raw",
				TargetImageDigest:  bootTestDigest,
			},
		})).To(Succeed())
	}

	getHost := func(g Gomega) *infrav1.PhysicalHost {
		h := &infrav1.PhysicalHost{}
		g.Expect(k8sClient.Get(ctx, hostKey(), h)).To(Succeed())
		return h
	}
	getMachine := func(g Gomega) *infrav1.Beskar7Machine {
		m := &infrav1.Beskar7Machine{}
		g.Expect(k8sClient.Get(ctx, machineKey(), m)).To(Succeed())
		return m
	}
	hostRefused := func(g Gomega) {
		cond := conditions.Get(getHost(g), infrav1.NetworkDataReadyCondition)
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal(infrav1.NetworkDataNotAuthorizedReason))
	}
	hostBooted := func(g Gomega) {
		g.Expect(getHost(g).Status.State).To(Equal(infrav1.StateInspecting))
	}

	It("skips a host whose network data is refused, and claims and boots it once the Secret is fixed", func() {
		createNetworkDataSecret("another-host")
		createHost("")
		Eventually(func(g Gomega) {
			hostRefused(g)
			g.Expect(getHost(g).Status.State).To(Equal(infrav1.StateAvailable))
		}, 30*time.Second, 100*time.Millisecond).Should(Succeed())

		createMachine()
		Eventually(func(g Gomega) {
			g.Expect(conditions.GetReason(getMachine(g), infrav1.PhysicalHostAssociatedCondition)).
				To(Equal(infrav1.WaitingForPhysicalHostReason))
		}, 30*time.Second, 100*time.Millisecond).Should(Succeed())
		Consistently(func(g Gomega) {
			g.Expect(getHost(g).Spec.ConsumerRef).To(BeNil(), "a host whose network data is refused must not be claimed")
		}, networkDataRunHold, 100*time.Millisecond).Should(Succeed())

		By("fixing the Secret's opt-in annotation")
		fixNetworkDataSecret()
		Eventually(hostBooted, networkDataRunWake, 100*time.Millisecond).Should(Succeed(),
			"the host's NetworkDataReady turning True must wake the waiting machine")
		Expect(getHost(Default).Spec.ConsumerRef.Name).To(Equal(networkDataRunMachine))
	})

	It("does not boot a claimed host whose network data is refused, and boots it once the Secret is fixed", func() {
		createNetworkDataSecret("another-host")
		createHost(networkDataRunMachine)
		createMachine()

		Eventually(func(g Gomega) {
			hostRefused(g)
			g.Expect(conditions.GetReason(getMachine(g), infrav1.InfrastructureReadyCondition)).
				To(Equal(infrav1.WaitingForNetworkDataReason))
		}, 30*time.Second, 100*time.Millisecond).Should(Succeed())
		Consistently(func(g Gomega) {
			g.Expect(getHost(g).Status.State).NotTo(Equal(infrav1.StateInspecting), "a host whose network data is refused must not be booted")
		}, networkDataRunHold, 100*time.Millisecond).Should(Succeed())

		By("fixing the Secret's opt-in annotation")
		fixNetworkDataSecret()
		Eventually(hostBooted, networkDataRunWake, 100*time.Millisecond).Should(Succeed())
	})
})
