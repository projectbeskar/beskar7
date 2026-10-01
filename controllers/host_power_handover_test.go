/*
Copyright 2026 The Beskar7 Authors.

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
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stmcginnis/gofish/schemas"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
)

// A host released in the middle of a run is still running the inspector, which
// parks after a failure and has nothing that acts on the ACPI power button a
// graceful shutdown presses — so it stays on. The next claim then has to boot
// it into a fresh inspector; leaving an already-on host alone left it parked in
// the old one until the machine failed with InspectionTimedOut.
var _ = Describe("Host power across a release and the next claim", func() {
	const machineName = "power-machine"

	var (
		testNs *corev1.Namespace
		mockRf *internalredfish.MockClient
		r      *Beskar7MachineReconciler
		key    types.NamespacedName
	)

	BeforeEach(func() {
		testNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "host-power-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(testNs.Name))).To(Succeed())
		host := claimedPhysicalHost(testNs.Name, "power-host", machineName)
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		key = client.ObjectKeyFromObject(host)

		mockRf = internalredfish.NewMockClient()
		r = &Beskar7MachineReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Log:    ctrl.Log.WithName("host-power-test"),
			RedfishClientFactory: func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
				return mockRf, nil
			},
			BootstrapURLBase: "https://test.svc:8082",
		}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	getHost := func() *infrav1.PhysicalHost {
		h := &infrav1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, key, h)).To(Succeed())
		return h
	}
	setState := func(state string) {
		h := getHost()
		h.Status.State = state
		Expect(k8sClient.Status().Update(ctx, h)).To(Succeed())
	}

	Context("claiming a host", func() {
		trigger := func() error {
			machine := &infrav1.Beskar7Machine{ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: testNs.Name}}
			_, err := r.triggerInspection(ctx, r.Log, machine, getHost())
			return err
		}

		It("restarts a host that is already on, so it boots the inspector", func() {
			setState(infrav1.StateInUse)
			mockRf.PowerState = schemas.OnPowerState

			Expect(trigger()).To(Succeed())

			Expect(mockRf.SetBootSourcePXECalled).To(BeTrue())
			Expect(mockRf.ResetCalled).To(BeTrue(),
				"a host that is on is running something else (a parked inspector, the previous OS); only a restart boots it into the PXE override")
			Expect(mockRf.SetPowerStateCalled).To(BeFalse())
		})

		It("does not restart a host whose inspect request is pending, even after a controller restart", func() {
			// The host stays InUse until the PhysicalHost reconciler applies the
			// request, so the machine can pass through here again meanwhile; a
			// restarted controller has only the request to go by.
			setState(infrav1.StateInUse)
			mockRf.PowerState = schemas.OnPowerState
			Expect(trigger()).To(Succeed())
			Expect(mockRf.ResetCalled).To(BeTrue())
			Expect(getHost().Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, "inspect"))

			r = &Beskar7MachineReconciler{
				Client: r.Client, Scheme: r.Scheme, Log: r.Log,
				RedfishClientFactory: r.RedfishClientFactory, BootstrapURLBase: r.BootstrapURLBase,
			}
			mockRf.ResetCalled = false
			Expect(trigger()).To(Succeed())

			Expect(mockRf.ResetCalled).To(BeFalse(), "a second restart would interrupt the inspector booting")
			Expect(mockRf.SetPowerStateCalled).To(BeFalse())
		})

		It("does not restart a host it booted for this claim before the request is visible", func() {
			// A pass can read the host from a cache that has not yet seen the
			// request the pass before it sent (the lab showed three passes in one
			// second); the controller's own record of the boot must hold then.
			setState(infrav1.StateInUse)
			mockRf.PowerState = schemas.OnPowerState
			Expect(trigger()).To(Succeed())
			Expect(mockRf.ResetCalled).To(BeTrue())

			stale := getHost()
			delete(stale.Annotations, InspectionRequestAnnotation)
			mockRf.ResetCalled = false
			machine := &infrav1.Beskar7Machine{ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: testNs.Name}}
			_, err := r.triggerInspection(ctx, r.Log, machine, stale)
			Expect(err).NotTo(HaveOccurred())

			Expect(mockRf.ResetCalled).To(BeFalse(), "a second restart would interrupt the inspector booting")
		})

		It("restarts the host again for a new claim, even by a machine of the same name", func() {
			setState(infrav1.StateInUse)
			mockRf.PowerState = schemas.OnPowerState
			Expect(trigger()).To(Succeed())

			// The machine is recreated under the same name and claims the same
			// host, which is still on: a new claim, told apart by its UID.
			h := getHost()
			delete(h.Annotations, InspectionRequestAnnotation)
			Expect(k8sClient.Update(ctx, h)).To(Succeed())
			mockRf.ResetCalled = false
			successor := &infrav1.Beskar7Machine{ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: testNs.Name, UID: "successor-uid"}}
			_, err := r.triggerInspection(ctx, r.Log, successor, getHost())
			Expect(err).NotTo(HaveOccurred())

			Expect(mockRf.ResetCalled).To(BeTrue())
		})

		It("rides out a routine conflict on the inspection request instead of failing the pass", func() {
			// The PhysicalHost reconciler mirrors the credentials minted above into
			// the host's status, so the host routinely changes under this pass.
			// Failing the pass on that conflict sent the claim round again, and the
			// next pass restarted the host a second time (seen in the lab).
			setState(infrav1.StateInUse)
			mockRf.PowerState = schemas.OnPowerState
			base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			var bumped atomic.Bool
			r.Client = interceptor.NewClient(base, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if _, isHost := obj.(*infrav1.PhysicalHost); isHost && bumped.CompareAndSwap(false, true) {
						h := getHost()
						h.Status.ObservedPowerState = string(schemas.OnPowerState)
						Expect(k8sClient.Status().Update(ctx, h)).To(Succeed())
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			})

			Expect(trigger()).To(Succeed())

			Expect(bumped.Load()).To(BeTrue(), "the host must change under the pass for this spec to mean anything")
			Expect(getHost().Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, "inspect"))
		})

		It("powers on a host that is off, without a restart", func() {
			setState(infrav1.StateInUse)
			mockRf.PowerState = schemas.OffPowerState

			Expect(trigger()).To(Succeed())

			Expect(mockRf.SetPowerStateCalled).To(BeTrue())
			Expect(mockRf.PowerState).To(Equal(schemas.OnPowerState))
			Expect(mockRf.ResetCalled).To(BeFalse())
		})

		It("mints the host's credentials before it boots the host", func() {
			// Nothing after the power action may fail and send the claim round
			// again, or a retry would restart a host that is already booting.
			setState(infrav1.StateInUse)
			mockRf.PowerState = schemas.OnPowerState
			mockRf.ShouldFail["Reset"] = errors.New("BMC busy")

			Expect(trigger()).NotTo(Succeed())

			s := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNs.Name, Name: bootstrapTokenSecretName(key.Name)}, s)).To(Succeed())
			creds := readBootstrapCredentials(s)
			Expect(creds.consumer).To(Equal(machineName))
			Expect(creds.tokenValid(time.Now())).To(BeTrue())
			Expect(getHost().Annotations).NotTo(HaveKey(InspectionRequestAnnotation),
				"the host must not be marked Inspecting before it has been booted")
		})
	})

	Context("releasing a host", func() {
		release := func() {
			b7m := &infrav1.Beskar7Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:       machineName,
					Namespace:  testNs.Name,
					Finalizers: []string{Beskar7MachineFinalizer},
				},
				Spec: infrav1.Beskar7MachineSpec{
					InspectionImageURL: "http://boot-server/inspect.ipxe",
					TargetImageURL:     "http://boot-server/kairos.raw",
					TargetImageDigest:  bootTestDigest,
				},
			}
			Expect(k8sClient.Create(ctx, b7m)).To(Succeed())
			DeferCleanup(func() {
				Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(b7m), b7m)).To(Succeed())
				base := b7m.DeepCopy()
				b7m.Finalizers = nil
				Expect(k8sClient.Patch(ctx, b7m, client.MergeFrom(base))).To(Succeed())
			})

			_, err := r.reconcileDelete(ctx, r.Log, b7m)
			Expect(err).NotTo(HaveOccurred())
			Expect(getHost().Spec.ConsumerRef).To(BeNil())
		}

		DescribeTable("forces off a host released mid-run, which a graceful shutdown would leave on",
			func(state string) {
				setState(state)
				mockRf.PowerState = schemas.OnPowerState

				release()

				Expect(mockRf.ForcePowerOffCalled).To(BeTrue())
				Expect(mockRf.SetPowerStateCalled).To(BeFalse())
				Expect(mockRf.ClearBootSourceOverrideCalled).To(BeTrue())
			},
			Entry("Inspecting", infrav1.StateInspecting),
			Entry("Deploying", infrav1.StateDeploying),
			Entry("Error", infrav1.StateError),
		)

		It("shuts a provisioned host down gracefully", func() {
			setState(infrav1.StateReady)
			mockRf.PowerState = schemas.OnPowerState

			release()

			Expect(mockRf.SetPowerStateCalled).To(BeTrue())
			Expect(mockRf.PowerState).To(Equal(schemas.OffPowerState))
			Expect(mockRf.ForcePowerOffCalled).To(BeFalse())
		})
	})
})
