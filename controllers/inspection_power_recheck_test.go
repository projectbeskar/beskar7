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

	"github.com/stmcginnis/gofish/redfish"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
)

// A host that is powered off cannot be running the inspector. That state is
// reachable without anything being broken: when a host is re-claimed straight
// after a release, the power-on decision can be made from a reading taken while
// the previous consumer's shutdown is still in flight — the read returns On, the
// power-on is skipped, and the host powers itself off moments later. Measured on
// bare metal (2026-09-14): reads said On at 08:44:46-47, the serial console
// printed "reboot: Power down" at 08:44:49, and the machine then sat through the
// full 10-minute inspection timeout before being failed terminally.
var _ = Describe("Beskar7Machine inspection when the host is powered off", func() {
	const pastTheRecheckDelay = InspectionPowerRecheckDelay + time.Minute

	var (
		testNs *corev1.Namespace
		mockRf *internalredfish.MockClient
		r      *Beskar7MachineReconciler
		host   *infrav1.PhysicalHost
		b7m    *infrav1.Beskar7Machine
	)

	BeforeEach(func() {
		testNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "insp-power-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "bmc-creds", Namespace: testNs.Name},
			Data:       map[string][]byte{"username": []byte("admin"), "password": []byte("pw")},
		})).To(Succeed())

		mockRf = internalredfish.NewMockClient()
		r = &Beskar7MachineReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Log:    ctrl.Log.WithName("insp-power-test"),
			RedfishClientFactory: func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
				return mockRf, nil
			},
			BootstrapURLBase: "https://example.com:8082",
		}

		b7m = &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "insp-power-machine", Namespace: testNs.Name},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot/inspect.ipxe",
				TargetImageURL:     "http://boot/kairos.raw",
				TargetImageDigest:  bootTestDigest,
			},
		}
		Expect(k8sClient.Create(ctx, b7m)).To(Succeed())

		host = &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "insp-power-host", Namespace: testNs.Name},
			Spec: infrav1.PhysicalHostSpec{
				RedfishConnection: infrav1.RedfishConnection{
					Address:              "https://192.168.2.240",
					CredentialsSecretRef: "bmc-creds",
				},
				ConsumerRef: &corev1.ObjectReference{
					Kind: "Beskar7Machine", Name: b7m.Name, Namespace: testNs.Name,
					APIVersion: infrav1.GroupVersion.String(),
				},
			},
		}
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	// inspecting puts the host in the state handleInspectingHost monitors, with
	// the inspection started `age` ago and the given cached power reading.
	inspecting := func(age time.Duration, observed redfish.PowerState) {
		stamp := metav1.NewTime(time.Now().Add(-age))
		host.Status.State = infrav1.StateInspecting
		host.Status.InspectionPhase = infrav1.InspectionPhasePending
		host.Status.InspectionTimestamp = &stamp
		host.Status.ObservedPowerState = string(observed)
		Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
	}

	It("powers the host back on instead of waiting out the inspection timeout", func() {
		inspecting(pastTheRecheckDelay, redfish.OffPowerState)
		mockRf.PowerState = redfish.OffPowerState

		result, err := r.handleInspectingHost(ctx, r.Log, b7m, host)
		Expect(err).NotTo(HaveOccurred())

		Expect(mockRf.SetPowerStateCalled).To(BeTrue(),
			"a host that is off cannot be running the inspector; it must be powered back on")
		Expect(mockRf.PowerState).To(Equal(redfish.OnPowerState))

		By("leaving the machine still inspecting rather than failing it")
		Expect(isTerminallyFailed(b7m)).To(BeFalse())
		Expect(result.RequeueAfter).NotTo(BeZero(), "it must keep monitoring")
	})

	It("does not touch a host that is running normally", func() {
		inspecting(pastTheRecheckDelay, redfish.OnPowerState)
		mockRf.PowerState = redfish.OnPowerState

		_, err := r.handleInspectingHost(ctx, r.Log, b7m, host)
		Expect(err).NotTo(HaveOccurred())
		Expect(mockRf.SetPowerStateCalled).To(BeFalse(),
			"the cached reading says the host is up, so nothing should be sent to the BMC")
		Expect(mockRf.GetPowerStateCalled).To(BeFalse())
	})

	It("trusts the BMC over a stale cached reading", func() {
		// The other reconciler maintains ObservedPowerState on its own cadence,
		// so it can still say Off just after a successful power-on. Confirming
		// over Redfish is what stops that becoming a spurious power cycle.
		inspecting(pastTheRecheckDelay, redfish.OffPowerState)
		mockRf.PowerState = redfish.OnPowerState

		_, err := r.handleInspectingHost(ctx, r.Log, b7m, host)
		Expect(err).NotTo(HaveOccurred())
		Expect(mockRf.GetPowerStateCalled).To(BeTrue(), "it should check before acting")
		Expect(mockRf.SetPowerStateCalled).To(BeFalse(), "the host is already on; nothing to correct")
	})

	It("gives a freshly started inspection time to settle before believing an Off", func() {
		inspecting(10*time.Second, redfish.OffPowerState)
		mockRf.PowerState = redfish.OffPowerState

		_, err := r.handleInspectingHost(ctx, r.Log, b7m, host)
		Expect(err).NotTo(HaveOccurred())
		Expect(mockRf.SetPowerStateCalled).To(BeFalse(),
			"a stale Off right after the power-on must not trigger a power cycle")
	})

	It("still fails the machine when the host never comes up at all", func() {
		// The power-on is a recovery attempt, not a replacement for the timeout.
		inspecting(r.inspectionTimeout()+time.Minute, redfish.OffPowerState)
		mockRf.PowerState = redfish.OffPowerState

		_, err := r.handleInspectingHost(ctx, r.Log, b7m, host)
		Expect(err).NotTo(HaveOccurred())
		Expect(mockRf.SetPowerStateCalled).To(BeTrue(), "it should still try to recover the host")
		Expect(isTerminallyFailed(b7m)).To(BeTrue(), "but the timeout is still the backstop")
	})
})
