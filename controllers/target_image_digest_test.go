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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
)

// snapshot returns the log entries recorded so far. The manager's reconcilers
// append from their own goroutines while a spec reads.
func (s *logCaptureSink) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.entries...)
}

// expectNoDigestLeak asserts that nothing the checksum server sent is on the
// machine or in the logs: not in any condition, not in status, not in a log line.
func expectNoDigestLeak(b7m *infrav1.Beskar7Machine, sink *logCaptureSink) {
	GinkgoHelper()
	status, err := json.Marshal(b7m.Status)
	Expect(err).NotTo(HaveOccurred())
	Expect(string(status)).NotTo(ContainSubstring(digestTestLeakMarker), "a response body, header or entry reached status")
	for _, entry := range sink.snapshot() {
		Expect(entry).NotTo(ContainSubstring(digestTestLeakMarker), "a response body, header or entry reached a log line")
	}
}

// ── what a machine does with a digest that does not resolve ──────────────────

// These drive reconcileNormal directly against a fake client with the
// status.state index the manager registers, so the pass the machine is in, and
// the host it must not claim, are certain rather than a matter of timing.
var _ = Describe("Beskar7Machine naming its image digest by URL (D-038)", func() {
	machineKey := types.NamespacedName{Namespace: "default", Name: "url-machine"}
	owner := &clusterv1.Machine{Spec: clusterv1.MachineSpec{ClusterName: "fake-cluster"}}

	hostIndex := func(obj client.Object) []string {
		h, ok := obj.(*infrav1.PhysicalHost)
		if !ok {
			return nil
		}
		return []string{string(h.Status.State)}
	}
	availableHost := func(name string) *infrav1.PhysicalHost {
		return &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: infrav1.PhysicalHostSpec{
				RedfishConnection: infrav1.RedfishConnection{Address: "https://192.0.2.10", CredentialsSecretRef: "irrelevant"},
			},
			Status: infrav1.PhysicalHostStatus{State: infrav1.StateAvailable},
		}
	}
	newClientWith := func(objs ...client.Object) client.Client {
		b := fake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithIndex(&infrav1.PhysicalHost{}, PhysicalHostStateIndex, hostIndex).
			WithStatusSubresource(&infrav1.PhysicalHost{})
		c := b.Build()
		for _, o := range objs {
			var status infrav1.PhysicalHostStatus
			h, isHost := o.(*infrav1.PhysicalHost)
			if isHost {
				status = h.Status
			}
			Expect(c.Create(ctx, o)).To(Succeed())
			if isHost {
				h.Status = status
				Expect(c.Status().Update(ctx, h)).To(Succeed())
			}
		}
		return c
	}
	newMachine := func(digestURL string) *infrav1.Beskar7Machine {
		return &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: machineKey.Name, Namespace: machineKey.Namespace, UID: "url-machine-uid", Finalizers: []string{Beskar7MachineFinalizer}},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL:   "http://boot/inspect.ipxe",
				TargetImageURL:       digestTestImageURL,
				TargetImageDigestURL: digestURL,
			},
		}
	}
	newReconciler := func(c client.Client, sink *logCaptureSink, s *checksumServer) *Beskar7MachineReconciler {
		r := &Beskar7MachineReconciler{Client: c, Scheme: scheme.Scheme, Log: logWithSink(sink)}
		if s != nil {
			r.ChecksumRootCAs = s.pool
		}
		return r
	}
	consumerOf := func(c client.Client, name string) *corev1.ObjectReference {
		h := &infrav1.PhysicalHost{}
		Expect(c.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, h)).To(Succeed())
		return h.Spec.ConsumerRef
	}
	expireBackoff := func(r *Beskar7MachineReconciler) {
		v, ok := r.digestRetries.Load(machineKey)
		Expect(ok).To(BeTrue(), "there should be a backoff record to expire")
		retry, ok := v.(digestRetry)
		Expect(ok).To(BeTrue())
		retry.next = time.Now().Add(-time.Second)
		r.digestRetries.Store(machineKey, retry)
	}
	sums := func(hex string) http.Handler { return servingText(gnuLine(hex, digestTestImageName)) }

	oversized := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "# ", digestTestLeakMarker, "\n")
		_, _ = fmt.Fprint(w, strings.Repeat("#"+strings.Repeat("x", 78)+"\n", digestFetchMaxBytes/79+2))
	})
	redirectsElsewhere := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example.invalid/SHA256SUMS?token="+digestTestLeakMarker, http.StatusFound)
	})
	redirectsForever := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/again?token="+digestTestLeakMarker, http.StatusFound)
	})
	failsWith := func(code int) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Error-Detail", digestTestLeakMarker)
			http.Error(w, digestTestLeakMarker, code)
		})
	}

	DescribeTable("a digest that does not resolve holds the machine at WaitingForTargetImageDigest, claims nothing, and retries later",
		func(handler http.Handler, trusted bool, wantReason string) {
			s := newChecksumServer(handler)
			DeferCleanup(s.Close)
			sink := &logCaptureSink{}
			c := newClientWith(availableHost("free-host"))
			r := newReconciler(c, sink, nil)
			if trusted {
				r.ChecksumRootCAs = s.pool
			}
			b7m := newMachine(s.URL + "/SHA256SUMS?token=" + digestTestLeakMarker)

			result, err := r.reconcileNormal(ctx, r.Log, b7m, owner)

			Expect(err).NotTo(HaveOccurred(), "an unresolved digest is a wait, not an error")
			Expect(result.RequeueAfter).To(BeNumerically(">=", 30*time.Second))
			Expect(result.RequeueAfter).To(BeNumerically("<=", 5*time.Minute))
			Expect(isTerminallyFailed(b7m)).To(BeFalse(), "the operator can fix the file or the URL")
			cond := conditions.Get(b7m, infrav1.InfrastructureReadyCondition)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(infrav1.WaitingForTargetImageDigestReason))
			Expect(cond.Message).To(ContainSubstring(wantReason))
			Expect(cond.Message).To(ContainSubstring(strings.TrimPrefix(s.URL, "https://")), "the message names the host")
			Expect(cond.Message).NotTo(ContainSubstring("token="), "nor the query")
			Expect(b7m.Status.TargetImageDigest).To(BeEmpty())
			Expect(b7m.Status.TargetImageDigestURL).To(BeEmpty())
			Expect(conditions.Has(b7m, infrav1.PhysicalHostAssociatedCondition)).To(BeFalse(), "the claim step was never reached")
			Expect(consumerOf(c, "free-host")).To(BeNil(), "a digest that does not resolve must not hold hardware")
			expectNoDigestLeak(b7m, sink)
		},
		Entry("no entry for the image in the file",
			servingText("# "+digestTestLeakMarker+"\n"+gnuLine(digestTestHexB, "other.raw")), true, "no entry for"),
		Entry("the entry is in a directory", servingText(gnuLine(digestTestHexA, "dir/"+digestTestImageName)+"# "+digestTestLeakMarker), true, "no entry for"),
		Entry("two entries that disagree",
			servingText("# "+digestTestLeakMarker+"\n"+gnuLine(digestTestHexA, digestTestImageName)+gnuLine(digestTestHexB, digestTestImageName)), true,
			"conflicting entries for"),
		Entry("a digest that is not hex",
			servingText(gnuLine(digestTestLeakMarker, digestTestImageName)), true, "is not a SHA-256 digest"),
		Entry("a digest of the wrong length",
			servingText(gnuLine(digestTestHexA+digestTestLeakMarker, digestTestImageName)), true, "is not a SHA-256 digest"),
		Entry("a body over 64 KiB", oversized, true, "checksum file too large"),
		Entry("HTTP 404", failsWith(http.StatusNotFound), true, "HTTP 404"),
		Entry("HTTP 500", failsWith(http.StatusInternalServerError), true, "HTTP 500"),
		Entry("a certificate the manager does not trust", sums(digestTestHexA), false, "TLS certificate not trusted"),
		Entry("a redirect to another host", redirectsElsewhere, true, "redirect to another host refused"),
		Entry("redirects that never end", redirectsForever, true, "more than 3 redirects"),
	)

	DescribeTable("pins the digest of the image whatever the layout of the file",
		func(handler http.Handler, path string) {
			s := newChecksumServer(handler)
			DeferCleanup(s.Close)
			c := newClientWith(availableHost("free-host"))
			r := newReconciler(c, &logCaptureSink{}, s)
			url := s.URL + path
			b7m := newMachine(url)

			result, err := r.reconcileNormal(ctx, r.Log, b7m, owner)

			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(requeueShortly))
			Expect(b7m.Status.TargetImageDigest).To(Equal("sha256:" + digestTestHexA))
			Expect(b7m.Status.TargetImageDigestURL).To(Equal(url))
			Expect(consumerOf(c, "free-host")).To(BeNil(), "claimed on the pass after the pin")
		},
		Entry("GNU lines, text mode", servingText(gnuLine(digestTestHexB, "other.raw")+gnuLine(digestTestHexA, digestTestImageName)), "/SHA256SUMS"),
		Entry("GNU lines, binary mode", servingText(digestTestHexA+" *"+digestTestImageName+"\n"), "/SHA256SUMS"),
		Entry("BSD lines", servingText("SHA256 (other.raw) = "+digestTestHexB+"\nSHA256 ("+digestTestImageName+") = "+digestTestHexA+"\n"), "/SHA256SUMS"),
		Entry("a file that is one bare digest", servingText("  "+digestTestHexA+"\n\n"), "/ubuntu.sha256"),
		Entry("capitals, comments and blank lines", servingText("# release v1\n\n"+gnuLine(strings.ToUpper(digestTestHexA), "./"+digestTestImageName)), "/SHA256SUMS"),
		Entry("the same file behind a redirect to the same host",
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/old/SHA256SUMS" {
					http.Redirect(w, r, "/v1/SHA256SUMS", http.StatusMovedPermanently)
					return
				}
				_, _ = fmt.Fprint(w, gnuLine(digestTestHexA, digestTestImageName))
			}), "/old/SHA256SUMS"),
	)

	It("does not ask the server again inside the backoff, and asks again, later, once it has passed", func() {
		s := newChecksumServer(failsWith(http.StatusServiceUnavailable))
		DeferCleanup(s.Close)
		sink := &logCaptureSink{}
		r := newReconciler(newClientWith(availableHost("free-host")), sink, s)
		b7m := newMachine(s.URL + "/SHA256SUMS")

		first, err := r.reconcileNormal(ctx, r.Log, b7m, owner)
		Expect(err).NotTo(HaveOccurred())
		Expect(first.RequeueAfter).To(BeNumerically("~", 30*time.Second, time.Second))
		Expect(s.hits.Load()).To(Equal(int32(1)))
		message := conditions.Get(b7m, infrav1.InfrastructureReadyCondition).Message

		By("reconciling again straight away, as an event on the machine does")
		again, err := r.reconcileNormal(ctx, r.Log, b7m, owner)
		Expect(err).NotTo(HaveOccurred())
		Expect(again.RequeueAfter).To(BeNumerically("<=", first.RequeueAfter))
		Expect(again.RequeueAfter).To(BeNumerically(">", 25*time.Second), "what is left of the window")
		Expect(s.hits.Load()).To(Equal(int32(1)), "the server is not asked inside the window")
		Expect(conditions.Get(b7m, infrav1.InfrastructureReadyCondition).Message).To(Equal(message), "the wait is reported unchanged")

		By("letting the window pass")
		expireBackoff(r)
		second, err := r.reconcileNormal(ctx, r.Log, b7m, owner)
		Expect(err).NotTo(HaveOccurred())
		Expect(s.hits.Load()).To(Equal(int32(2)))
		Expect(second.RequeueAfter).To(BeNumerically("~", time.Minute, time.Second), "the wait doubles")

		By("changing the URL, which starts the backoff over")
		b7m.Spec.TargetImageDigestURL = s.URL + "/other/SHA256SUMS"
		third, err := r.reconcileNormal(ctx, r.Log, b7m, owner)
		Expect(err).NotTo(HaveOccurred())
		Expect(s.hits.Load()).To(Equal(int32(3)))
		Expect(third.RequeueAfter).To(BeNumerically("~", 30*time.Second, time.Second))
	})

	It("resolves once the file is fixed, pins the digest in a pass of its own, and only then claims a host", func() {
		var body atomic.Value
		body.Store("# nothing yet\n")
		s := newChecksumServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, body.Load())
		}))
		DeferCleanup(s.Close)
		sink := &logCaptureSink{}
		c := newClientWith(availableHost("free-host"))
		r := newReconciler(c, sink, s)
		url := s.URL + "/v1/SHA256SUMS"
		b7m := newMachine(url)

		_, err := r.reconcileNormal(ctx, r.Log, b7m, owner)
		Expect(err).NotTo(HaveOccurred())
		Expect(conditions.GetReason(b7m, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.WaitingForTargetImageDigestReason))

		By("fixing the file")
		body.Store(gnuLine(digestTestHexB, "other.raw") + gnuLine(strings.ToUpper(digestTestHexA), digestTestImageName))
		expireBackoff(r)

		result, err := r.reconcileNormal(ctx, r.Log, b7m, owner)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(requeueShortly))
		Expect(b7m.Status.TargetImageDigest).To(Equal("sha256:" + digestTestHexA))
		Expect(b7m.Status.TargetImageDigestURL).To(Equal(url))
		Expect(conditions.Has(b7m, infrav1.InfrastructureReadyCondition)).To(BeFalse(),
			"the wait does not outlive itself: a missing InfrastructureReady is left out of the Ready summary")
		Expect(consumerOf(c, "free-host")).To(BeNil(), "the pin is persisted before any host is claimed")
		_, retried := r.digestRetries.Load(machineKey)
		Expect(retried).To(BeFalse(), "the backoff record is dropped")
		_, remembered := r.digestPins.Load(machineKey)
		Expect(remembered).To(BeTrue(), "and the resolution is remembered until status shows it")

		By("the next pass claims")
		result, err = r.reconcileNormal(ctx, r.Log, b7m, owner)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(5 * time.Second))
		Expect(consumerOf(c, "free-host")).NotTo(BeNil())
		Expect(conditions.IsTrue(b7m, infrav1.PhysicalHostAssociatedCondition)).To(BeTrue())
		Expect(conditions.GetReason(b7m, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.PhysicalHostNotReadyReason))
		Expect(b7m.Status.TargetImageDigest).To(Equal("sha256:" + digestTestHexA))
		Expect(s.hits.Load()).To(Equal(int32(2)), "one failed attempt, one that resolved: never again")

		By("the file changing after the claim")
		body.Store(gnuLine(digestTestHexC, digestTestImageName))
		for range 3 {
			_, err = r.reconcileNormal(ctx, r.Log, b7m, owner)
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(b7m.Status.TargetImageDigest).To(Equal("sha256:"+digestTestHexA), "a run in flight never changes digest")
		Expect(s.hits.Load()).To(Equal(int32(2)))
	})

	It("reuses the digest it just resolved when a pass reads a status that predates the pin", func() {
		var body atomic.Value
		body.Store(gnuLine(digestTestHexA, digestTestImageName))
		s := newChecksumServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, body.Load())
		}))
		DeferCleanup(s.Close)
		c := newClientWith(availableHost("free-host"))
		r := newReconciler(c, &logCaptureSink{}, s)
		url := s.URL + "/SHA256SUMS"

		first := newMachine(url)
		_, proceed, err := r.ensureTargetImageDigest(ctx, r.Log, first)
		Expect(err).NotTo(HaveOccurred())
		Expect(proceed).To(BeFalse())
		Expect(first.Status.TargetImageDigest).To(Equal("sha256:" + digestTestHexA))

		By("a pass working from the cache, which has not seen the status the first pass wrote")
		body.Store(gnuLine(digestTestHexB, digestTestImageName))
		stale := newMachine(url)
		result, proceed, err := r.ensureTargetImageDigest(ctx, r.Log, stale)
		Expect(err).NotTo(HaveOccurred())
		Expect(proceed).To(BeFalse())
		Expect(result.RequeueAfter).To(Equal(requeueShortly))
		Expect(stale.Status.TargetImageDigest).To(Equal("sha256:"+digestTestHexA), "the digest it pinned, not the file's new one")
		Expect(stale.Status.TargetImageDigestURL).To(Equal(url))
		Expect(s.hits.Load()).To(Equal(int32(1)))

		By("a machine of the same name but another UID, which is another machine")
		recreated := newMachine(url)
		recreated.UID = "another-uid"
		_, _, err = r.ensureTargetImageDigest(ctx, r.Log, recreated)
		Expect(err).NotTo(HaveOccurred())
		Expect(recreated.Status.TargetImageDigest).To(Equal("sha256:" + digestTestHexB))
		Expect(s.hits.Load()).To(Equal(int32(2)))

		By("a pin that status has caught up with is dropped from memory")
		caughtUp := newMachine(url)
		caughtUp.UID = "another-uid"
		caughtUp.Status.TargetImageDigest, caughtUp.Status.TargetImageDigestURL = "sha256:"+digestTestHexB, url
		_, proceed, err = r.ensureTargetImageDigest(ctx, r.Log, caughtUp)
		Expect(err).NotTo(HaveOccurred())
		Expect(proceed).To(BeTrue())
		_, remembered := r.digestPins.Load(machineKey)
		Expect(remembered).To(BeFalse())
	})

	It("fetches nothing when the spec names the digest, and drops a pin left from before", func() {
		s := newChecksumServer(sums(digestTestHexA))
		DeferCleanup(s.Close)
		c := newClientWith(availableHost("free-host"))
		r := newReconciler(c, &logCaptureSink{}, s)
		b7m := newMachine("")
		b7m.Spec.TargetImageDigest = "sha256:" + digestTestHexC
		b7m.Status.TargetImageDigest = "sha256:" + digestTestHexA
		b7m.Status.TargetImageDigestURL = s.URL + "/SHA256SUMS"

		result, err := r.reconcileNormal(ctx, r.Log, b7m, owner)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(5*time.Second), "straight to the claim")
		Expect(consumerOf(c, "free-host")).NotTo(BeNil())
		Expect(s.hits.Load()).To(BeZero(), "no fetch")
		Expect(b7m.Status.TargetImageDigest).To(BeEmpty(), "status never shows a digest /boot does not use")
		Expect(b7m.Status.TargetImageDigestURL).To(BeEmpty())
		Expect(effectiveTargetImageDigest(b7m)).To(Equal("sha256:" + digestTestHexC))
	})

	It("does not read the file again once it is pinned from this URL", func() {
		s := newChecksumServer(sums(digestTestHexB))
		DeferCleanup(s.Close)
		c := newClientWith(availableHost("free-host"))
		r := newReconciler(c, &logCaptureSink{}, s)
		b7m := newMachine(s.URL + "/SHA256SUMS")
		b7m.Status.TargetImageDigest = "sha256:" + digestTestHexA
		b7m.Status.TargetImageDigestURL = b7m.Spec.TargetImageDigestURL

		_, err := r.reconcileNormal(ctx, r.Log, b7m, owner)
		Expect(err).NotTo(HaveOccurred())
		Expect(s.hits.Load()).To(BeZero())
		Expect(b7m.Status.TargetImageDigest).To(Equal("sha256:" + digestTestHexA))
		Expect(consumerOf(c, "free-host")).NotTo(BeNil(), "and goes on to claim")
	})

	It("re-resolves when the URL changes before any host is claimed", func() {
		var requested []string
		var mu sync.Mutex
		s := newChecksumServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			requested = append(requested, r.URL.Path)
			mu.Unlock()
			_, _ = fmt.Fprint(w, gnuLine(digestTestHexB, digestTestImageName))
		}))
		DeferCleanup(s.Close)
		c := newClientWith(availableHost("free-host"))
		r := newReconciler(c, &logCaptureSink{}, s)
		b7m := newMachine(s.URL + "/v2/SHA256SUMS")
		b7m.Status.TargetImageDigest = "sha256:" + digestTestHexA
		b7m.Status.TargetImageDigestURL = s.URL + "/v1/SHA256SUMS"

		result, err := r.reconcileNormal(ctx, r.Log, b7m, owner)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(requeueShortly))
		mu.Lock()
		Expect(requested).To(Equal([]string{"/v2/SHA256SUMS"}))
		mu.Unlock()
		Expect(b7m.Status.TargetImageDigest).To(Equal("sha256:" + digestTestHexB))
		Expect(b7m.Status.TargetImageDigestURL).To(Equal(s.URL + "/v2/SHA256SUMS"))
		Expect(consumerOf(c, "free-host")).To(BeNil())
	})

	It("drops the old pin when the new URL does not resolve, so no host is claimed on the digest of another file", func() {
		s := newChecksumServer(failsWith(http.StatusNotFound))
		DeferCleanup(s.Close)
		c := newClientWith(availableHost("free-host"))
		r := newReconciler(c, &logCaptureSink{}, s)
		b7m := newMachine(s.URL + "/v2/SHA256SUMS")
		b7m.Status.TargetImageDigest = "sha256:" + digestTestHexA
		b7m.Status.TargetImageDigestURL = s.URL + "/v1/SHA256SUMS"

		_, err := r.reconcileNormal(ctx, r.Log, b7m, owner)
		Expect(err).NotTo(HaveOccurred())
		Expect(conditions.GetReason(b7m, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.WaitingForTargetImageDigestReason))
		Expect(b7m.Status.TargetImageDigest).To(BeEmpty())
		Expect(consumerOf(c, "free-host")).To(BeNil())
	})

	It("keeps the pinned digest when the URL changes after a host is claimed, and says so", func() {
		s := newChecksumServer(sums(digestTestHexB))
		DeferCleanup(s.Close)
		sink := &logCaptureSink{}
		held := availableHost("held-host")
		held.Spec.ConsumerRef = &corev1.ObjectReference{
			Kind: "Beskar7Machine", APIVersion: InfrastructureAPIVersion, Name: machineKey.Name, Namespace: machineKey.Namespace,
		}
		held.Status.State = infrav1.StateInUse
		c := newClientWith(held)
		r := newReconciler(c, sink, s)
		b7m := newMachine(s.URL + "/v2/SHA256SUMS?sig=" + digestTestLeakMarker)
		b7m.Status.TargetImageDigest = "sha256:" + digestTestHexA
		b7m.Status.TargetImageDigestURL = s.URL + "/v1/SHA256SUMS"

		result, proceed, err := r.ensureTargetImageDigest(ctx, r.Log, b7m)
		Expect(err).NotTo(HaveOccurred())
		Expect(proceed).To(BeTrue())
		Expect(result.IsZero()).To(BeTrue())
		Expect(s.hits.Load()).To(BeZero(), "the new URL is not even read")
		Expect(b7m.Status.TargetImageDigest).To(Equal("sha256:" + digestTestHexA))
		Expect(b7m.Status.TargetImageDigestURL).To(Equal(s.URL + "/v1/SHA256SUMS"))

		var said bool
		for _, entry := range sink.snapshot() {
			if strings.Contains(entry, "keeping the pinned digest") {
				said = true
			}
			Expect(entry).NotTo(ContainSubstring(digestTestLeakMarker), "the URL is logged without its query")
		}
		Expect(said).To(BeTrue(), "the kept pin is announced in a log line")
	})

	It("rebuilds a pin the status lost while a host is held, because /boot cannot render without one", func() {
		s := newChecksumServer(sums(digestTestHexA))
		DeferCleanup(s.Close)
		held := availableHost("held-host")
		held.Spec.ConsumerRef = &corev1.ObjectReference{
			Kind: "Beskar7Machine", APIVersion: InfrastructureAPIVersion, Name: machineKey.Name, Namespace: machineKey.Namespace,
		}
		held.Status.State = infrav1.StateInUse
		r := newReconciler(newClientWith(held), &logCaptureSink{}, s)
		b7m := newMachine(s.URL + "/SHA256SUMS")

		_, proceed, err := r.ensureTargetImageDigest(ctx, r.Log, b7m)
		Expect(err).NotTo(HaveOccurred())
		Expect(proceed).To(BeFalse(), "the pin is persisted first")
		Expect(b7m.Status.TargetImageDigest).To(Equal("sha256:" + digestTestHexA))
	})

	It("neither needs nor fetches the digest once the machine's host has been Ready", func() {
		s := newChecksumServer(failsWith(http.StatusNotFound))
		DeferCleanup(s.Close)
		sink := &logCaptureSink{}
		bootstrapName := "url-machine-bootstrap"
		host := availableHost("ready-host")
		host.Spec.ConsumerRef = &corev1.ObjectReference{
			Kind: "Beskar7Machine", APIVersion: InfrastructureAPIVersion, Name: machineKey.Name, Namespace: machineKey.Namespace,
		}
		host.Status.State = infrav1.StateReady
		c := newClientWith(host, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: bootstrapName, Namespace: "default"}})
		r := newReconciler(c, sink, s)
		r.BootstrapURLBase = "https://example.com:8082"
		r.RedfishClientFactory = func(context.Context, string, string, string, bool, []byte) (internalredfish.Client, error) {
			return internalredfish.NewMockClient(), nil
		}
		// A machine whose status a clusterctl move dropped: it has its
		// ProviderID (spec survives a move) and no pin.
		b7m := newMachine(s.URL + "/SHA256SUMS")
		b7m.Spec.ProviderID = providerID("default", "ready-host")
		withBootstrap := &clusterv1.Machine{Spec: clusterv1.MachineSpec{
			ClusterName: "fake-cluster", Bootstrap: clusterv1.Bootstrap{DataSecretName: &bootstrapName},
		}}

		_, err := r.reconcileNormal(ctx, r.Log, b7m, withBootstrap)
		Expect(err).NotTo(HaveOccurred())
		Expect(s.hits.Load()).To(BeZero(), "no fetch for a machine whose host is Ready")
		Expect(b7m.Status.Ready).To(BeTrue())
		Expect(conditions.GetReason(b7m, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.ProvisionedReason))
		Expect(b7m.Status.TargetImageDigest).To(BeEmpty(), "nothing is rebuilt that nothing needs")
	})

	It("drops the backoff record of a machine that is deleted", func() {
		s := newChecksumServer(failsWith(http.StatusNotFound))
		DeferCleanup(s.Close)
		r := newReconciler(newClientWith(), &logCaptureSink{}, s)
		b7m := newMachine(s.URL + "/SHA256SUMS")
		_, err := r.reconcileNormal(ctx, r.Log, b7m, owner)
		Expect(err).NotTo(HaveOccurred())
		_, ok := r.digestRetries.Load(machineKey)
		Expect(ok).To(BeTrue())

		r.digestPins.Store(machineKey, digestPin{url: "x"})
		r.forgetTargetImageDigest(machineKey)
		_, ok = r.digestRetries.Load(machineKey)
		Expect(ok).To(BeFalse())
		_, ok = r.digestPins.Load(machineKey)
		Expect(ok).To(BeFalse())
	})

	It("reports a spec with neither field as a wait, never a panic or a claim", func() {
		// The CRD admits no such spec; an object written before its CEL rule
		// existed, or by a client that skipped admission, still must not claim a host.
		c := newClientWith(availableHost("free-host"))
		r := newReconciler(c, &logCaptureSink{}, nil)
		b7m := newMachine("")

		result, err := r.reconcileNormal(ctx, r.Log, b7m, owner)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">=", 30*time.Second))
		cond := conditions.Get(b7m, infrav1.InfrastructureReadyCondition)
		Expect(cond.Reason).To(Equal(infrav1.WaitingForTargetImageDigestReason))
		Expect(cond.Message).To(ContainSubstring("neither spec.targetImageDigest nor spec.targetImageDigestURL is set"))
		Expect(consumerOf(c, "free-host")).To(BeNil())
	})

	It("refuses a URL with credentials in it, and an http one, without fetching", func() {
		plain := newChecksumServer(sums(digestTestHexA))
		DeferCleanup(plain.Close)
		for url, reason := range map[string]string{
			"https://admin:hunter2@" + strings.TrimPrefix(plain.URL, "https://") + "/SHA256SUMS": "must not contain credentials",
			strings.Replace(plain.URL, "https://", "http://", 1) + "/SHA256SUMS":                 "not an https URL",
		} {
			r := newReconciler(newClientWith(availableHost("free-host")), &logCaptureSink{}, plain)
			b7m := newMachine(url)

			_, err := r.reconcileNormal(ctx, r.Log, b7m, owner)
			Expect(err).NotTo(HaveOccurred())
			cond := conditions.Get(b7m, infrav1.InfrastructureReadyCondition)
			Expect(cond.Reason).To(Equal(infrav1.WaitingForTargetImageDigestReason))
			Expect(cond.Message).To(ContainSubstring(reason))
			Expect(cond.Message).NotTo(ContainSubstring("hunter2"))
		}
		Expect(plain.hits.Load()).To(BeZero())
	})
})

// ── /boot renders the effective digest ───────────────────────────────────────

var _ = Describe("Boot GET handler: the digest it renders (D-038)", func() {
	var ns string

	BeforeEach(func() {
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "boot-digest-"}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		ns = nsObj.Name
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	})

	const sumsURL = "https://sums.example.invalid/v1/SHA256SUMS"

	// asURLMachine turns the fixture's machine into one that names its digest by
	// URL, in one update so the CRD's exactly-one rule holds throughout.
	asURLMachine := func(b7m *infrav1.Beskar7Machine, pin string) {
		got := &infrav1.Beskar7Machine{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(b7m), got)).To(Succeed())
		got.Spec.TargetImageDigest = ""
		got.Spec.TargetImageDigestURL = sumsURL
		Expect(k8sClient.Update(ctx, got)).To(Succeed())
		if pin != "" {
			got.Status.TargetImageDigest = pin
			got.Status.TargetImageDigestURL = sumsURL
			Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
		}
	}
	serve := func(host *infrav1.PhysicalHost, nonce string) (int, string) {
		handler := &BootHandler{Client: k8sClient, Log: ctrl.Log.WithName("boot-digest"), Config: bootTestConfig()}
		w := serveBootFrom(handler, host.Namespace, host.Name, nonce, bootTestConsumingClient)
		return w.Code, w.Body.String()
	}

	It("renders the digest pinned in status for a machine that names it by URL", func() {
		host, b7m, _, nonce := bootTestFixture(ns)
		asURLMachine(b7m, "sha256:"+digestTestHexA)

		code, body := serve(host, nonce)
		Expect(code).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(" beskar7.target-digest=sha256:" + digestTestHexA + " "))
		Expect(body).NotTo(ContainSubstring(bootTestDigest))
	})

	It("renders the spec's digest when it is set, whatever status holds", func() {
		host, b7m, _, nonce := bootTestFixture(ns)
		got := &infrav1.Beskar7Machine{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(b7m), got)).To(Succeed())
		got.Status.TargetImageDigest = "sha256:" + digestTestHexA
		got.Status.TargetImageDigestURL = sumsURL
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())

		code, body := serve(host, nonce)
		Expect(code).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(" beskar7.target-digest=" + bootTestDigest + " "))
		Expect(body).NotTo(ContainSubstring(digestTestHexA))
	})

	It("renders nothing for a machine whose URL has not resolved: never an empty digest", func() {
		host, b7m, _, nonce := bootTestFixture(ns)
		asURLMachine(b7m, "")

		code, body := serve(host, nonce)
		Expect(code).To(Equal(bootHandlerOpaqueFailureStatus))
		Expect(strings.TrimSpace(body)).To(Equal(bootHandlerOpaqueFailureBody))
		Expect(body).NotTo(ContainSubstring("target-digest"))
	})

	It("serves the same client once the digest has resolved", func() {
		// The nonce is consumed before the script is rendered, so a render that
		// fails is a boot the host has to retry: the retry window re-serves it.
		host, b7m, _, nonce := bootTestFixture(ns)
		asURLMachine(b7m, "")
		code, _ := serve(host, nonce)
		Expect(code).To(Equal(bootHandlerOpaqueFailureStatus))

		got := &infrav1.Beskar7Machine{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(b7m), got)).To(Succeed())
		got.Status.TargetImageDigest = "sha256:" + digestTestHexB
		got.Status.TargetImageDigestURL = sumsURL
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())

		code, body := serve(host, nonce)
		Expect(code).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring(" beskar7.target-digest=sha256:" + digestTestHexB + " "))
	})

	DescribeTable("never renders a digest that is not in the canonical form",
		func(spec, pin string) {
			b7m := &infrav1.Beskar7Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "bad-digest-b7m", Namespace: "default"},
				Spec: infrav1.Beskar7MachineSpec{
					InspectionImageURL: "https://boot.example.com/inspect",
					TargetImageURL:     "https://boot.example.com/kairos.raw",
					TargetImageDigest:  spec,
				},
				Status: infrav1.Beskar7MachineStatus{TargetImageDigest: pin},
			}
			// A fake client takes what the CRD would refuse, which is the case:
			// the check runs on the value rendered, not the value admitted.
			c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(b7m).Build()
			h := &BootHandler{Client: c, Log: ctrl.Log.WithName("boot-digest"), Config: bootTestConfig()}
			ph := &infrav1.PhysicalHost{ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: "default"}}

			script, err := h.renderBootScript(ctx, h.Log, ph, client.ObjectKeyFromObject(b7m), "token", "")
			Expect(err).To(HaveOccurred())
			Expect(script).To(BeEmpty())
		},
		Entry("a pin with injected arguments", "", "sha256:"+digestTestHexA+" beskar7.token=stolen"),
		Entry("a pin in capitals", "", "sha256:"+strings.ToUpper(digestTestHexA)),
		Entry("a pin one character short", "", "sha256:"+digestTestHexA[:63]),
		Entry("a pin without the algorithm", "", digestTestHexA),
		Entry("neither digest set", "", ""),
	)

	It("picks the spec digest, then the pin", func() {
		b7m := &infrav1.Beskar7Machine{
			Spec:   infrav1.Beskar7MachineSpec{TargetImageDigest: "sha256:" + digestTestHexA},
			Status: infrav1.Beskar7MachineStatus{TargetImageDigest: "sha256:" + digestTestHexB},
		}
		Expect(effectiveTargetImageDigest(b7m)).To(Equal("sha256:" + digestTestHexA))
		b7m.Spec.TargetImageDigest = ""
		Expect(effectiveTargetImageDigest(b7m)).To(Equal("sha256:" + digestTestHexB))
		b7m.Status.TargetImageDigest = ""
		Expect(effectiveTargetImageDigest(b7m)).To(BeEmpty())
		Expect(validateBootDigest(effectiveTargetImageDigest(b7m))).To(MatchError(ContainSubstring("has not been resolved")))
	})
})

// ── the whole path, through a running manager ────────────────────────────────

// A manager per spec, scoped to the spec's namespace, as the other specs that
// need a cached client (the claim lists hosts by a field index).
var _ = Describe("Beskar7Machine resolving and pinning its image digest, end to end (D-038)", func() {
	var (
		ns         string
		sink       *logCaptureSink
		reconciler *Beskar7MachineReconciler
	)

	const machineName = "pinned-machine"
	machineKey := func() client.ObjectKey { return client.ObjectKey{Namespace: ns, Name: machineName} }
	hostKey := func(name string) client.ObjectKey { return client.ObjectKey{Namespace: ns, Name: name} }

	BeforeEach(func() {
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "digest-url-"}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		ns = nsObj.Name
		sink = &logCaptureSink{}
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns))).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	})

	// seedMachine creates the Cluster, bootstrap data, owner Machine and the
	// Beskar7Machine with spec, as the CAPI controllers leave them.
	seedMachine := func(spec infrav1.Beskar7MachineSpec) {
		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "digest-cluster", Namespace: ns},
			Spec:       clusterv1.ClusterSpec{Paused: ptr.To(false)},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: machineName + "-bootstrap", Namespace: ns},
			Data:       map[string][]byte{"value": []byte("#cloud-config\n")},
		})).To(Succeed())
		machine := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: machineName, Namespace: ns,
				Labels: map[string]string{clusterv1.ClusterNameLabel: cluster.Name},
			},
			Spec: clusterv1.MachineSpec{
				ClusterName: cluster.Name,
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: infrav1.GroupVersion.Group, Kind: "Beskar7Machine", Name: machineName,
				},
				Bootstrap: clusterv1.Bootstrap{DataSecretName: ptr.To(machineName + "-bootstrap")},
			},
		}
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		spec.InspectionImageURL = "http://boot/inspect.ipxe"
		if spec.TargetImageURL == "" {
			spec.TargetImageURL = digestTestImageURL
		}
		Expect(k8sClient.Create(ctx, &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: machineName, Namespace: ns,
				Labels: map[string]string{clusterv1.ClusterNameLabel: cluster.Name},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: machine.Name, UID: machine.UID,
				}},
			},
			Spec: spec,
		})).To(Succeed())
	}

	// seedHost creates an Available PhysicalHost.
	seedHost := func(name string) {
		host := &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: infrav1.PhysicalHostSpec{RedfishConnection: infrav1.RedfishConnection{
				Address: "https://mock-redfish.example.invalid:8443", CredentialsSecretRef: "bmc-credentials",
			}},
		}
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		host.Status.State = infrav1.StateAvailable
		Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
	}

	startManager := func(s *checksumServer) {
		skipNameValidation := true
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 k8sClient.Scheme(),
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			// envtest never finishes deleting namespaces, so a cluster-wide cache
			// would hand this controller every other spec's leftovers.
			Cache:      cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
			Controller: config.Controller{SkipNameValidation: &skipNameValidation},
		})
		Expect(err).NotTo(HaveOccurred())
		reconciler = &Beskar7MachineReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
			Log:    logWithSink(sink),
			RedfishClientFactory: func(context.Context, string, string, string, bool, []byte) (internalredfish.Client, error) {
				return internalredfish.NewMockClient(), nil
			},
			BootstrapURLBase: "https://example.com:8082",
		}
		if s != nil {
			reconciler.ChecksumRootCAs = s.pool
		}
		Expect(reconciler.SetupWithManager(mgr)).To(Succeed())
		mgrCtx, mgrCancel := context.WithCancel(ctx)
		DeferCleanup(mgrCancel)
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())
	}

	getMachine := func() *infrav1.Beskar7Machine {
		got := &infrav1.Beskar7Machine{}
		Expect(k8sClient.Get(ctx, machineKey(), got)).To(Succeed())
		return got
	}
	consumerName := func(name string) string {
		if ref := getPhysicalHost(hostKey(name)).Spec.ConsumerRef; ref != nil {
			return ref.Name
		}
		return ""
	}
	// poke makes the machine reconcile again without changing its spec.
	poke := func() {
		got := getMachine()
		base := got.DeepCopy()
		if got.Annotations == nil {
			got.Annotations = map[string]string{}
		}
		got.Annotations["poke"] = time.Now().Format(time.RFC3339Nano)
		Expect(k8sClient.Patch(ctx, got, client.MergeFrom(base))).To(Succeed())
	}
	setURL := func(url string) {
		got := getMachine()
		base := got.DeepCopy()
		got.Spec.TargetImageDigestURL = url
		Expect(k8sClient.Patch(ctx, got, client.MergeFrom(base))).To(Succeed())
	}

	It("resolves the URL once, pins it, claims a host only afterwards, and renders the pinned digest at /boot", func() {
		var body atomic.Value
		body.Store(gnuLine(digestTestHexB, "other.raw") + gnuLine(digestTestHexA, digestTestImageName))
		s := newChecksumServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, body.Load())
		}))
		DeferCleanup(s.Close)
		url := s.URL + "/v1/SHA256SUMS?sig=" + digestTestLeakMarker
		seedMachine(infrav1.Beskar7MachineSpec{TargetImageDigestURL: url})
		seedHost("pinned-host")
		startManager(s)

		By("pinning the digest and recording where it came from")
		Eventually(func(g Gomega) {
			got := getMachine()
			g.Expect(got.Status.TargetImageDigest).To(Equal("sha256:" + digestTestHexA))
			g.Expect(got.Status.TargetImageDigestURL).To(Equal(url))
			g.Expect(got.Spec.TargetImageDigest).To(BeEmpty(), "the controller never writes spec")
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		By("claiming the host after that")
		Eventually(func() string { return consumerName("pinned-host") }, 30*time.Second, 200*time.Millisecond).Should(Equal(machineName))
		Expect(s.hits.Load()).To(Equal(int32(1)))

		By("reporting a machine that is claimed and not provisioned as not Ready")
		Eventually(func(g Gomega) {
			got := getMachine()
			g.Expect(conditions.GetReason(got, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.PhysicalHostNotReadyReason))
			ready := conditions.Get(got, clusterv1.ReadyCondition)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		By("rendering the pinned digest into the boot script")
		host := getPhysicalHost(hostKey("pinned-host"))
		nonce := "pinned-boot-nonce"
		putCredentialSecret(hostKey("pinned-host"), boundCredentialData(machineName, "pinned-token", time.Hour, nonce, 10*time.Minute))
		handler := &BootHandler{Client: k8sClient, Log: ctrl.Log.WithName("pinned-boot"), Config: bootTestConfig()}
		first := serveBootFrom(handler, ns, host.Name, nonce, bootTestConsumingClient)
		Expect(first.Code).To(Equal(http.StatusOK))
		Expect(first.Body.String()).To(ContainSubstring(" beskar7.target-digest=sha256:" + digestTestHexA + " "))

		By("the checksum file changing after the claim")
		body.Store(gnuLine(digestTestHexC, digestTestImageName))
		for range 3 {
			poke()
		}
		Consistently(func(g Gomega) {
			g.Expect(getMachine().Status.TargetImageDigest).To(Equal("sha256:" + digestTestHexA))
			g.Expect(s.hits.Load()).To(Equal(int32(1)), "the pinned file is never read again")
		}, 3*time.Second, 250*time.Millisecond).Should(Succeed())

		By("a second boot of the same machine, which renders the same digest")
		again := serveBootFrom(handler, ns, host.Name, nonce, bootTestConsumingClient)
		Expect(again.Code).To(Equal(http.StatusOK))
		Expect(again.Body.String()).To(Equal(first.Body.String()))

		By("the host becoming Ready: the machine is Ready, and still holds the pin")
		Eventually(func() error {
			ready := getPhysicalHost(hostKey("pinned-host"))
			ready.Status.State = infrav1.StateReady
			ready.Status.Ready = true
			return k8sClient.Status().Update(ctx, ready)
		}, 10*time.Second, 100*time.Millisecond).Should(Succeed())
		Eventually(func(g Gomega) {
			got := getMachine()
			g.Expect(got.Status.Ready).To(BeTrue())
			g.Expect(got.Spec.ProviderID).To(Equal(providerID(ns, "pinned-host")))
			g.Expect(conditions.GetReason(got, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.ProvisionedReason))
			cond := conditions.Get(got, clusterv1.ReadyCondition)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(got.Status.TargetImageDigest).To(Equal("sha256:" + digestTestHexA))
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
		Expect(s.hits.Load()).To(Equal(int32(1)))

		By("never logging the URL's query")
		for _, entry := range sink.snapshot() {
			Expect(entry).NotTo(ContainSubstring(digestTestLeakMarker))
		}
	})

	It("holds an unresolvable URL at Ready=False with the reason, claims no host, asks again only after the backoff, and leaks nothing", func() {
		s := newChecksumServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Detail", digestTestLeakMarker)
			http.Error(w, digestTestLeakMarker, http.StatusNotFound)
		}))
		DeferCleanup(s.Close)
		seedMachine(infrav1.Beskar7MachineSpec{TargetImageDigestURL: s.URL + "/SHA256SUMS"})
		seedHost("unclaimed-host")
		startManager(s)

		var got *infrav1.Beskar7Machine
		Eventually(func(g Gomega) {
			got = getMachine()
			cond := conditions.Get(got, infrav1.InfrastructureReadyCondition)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal(infrav1.WaitingForTargetImageDigestReason))
			g.Expect(cond.Message).To(ContainSubstring("HTTP 404"))
			ready := conditions.Get(got, clusterv1.ReadyCondition)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionFalse), "an unresolved digest must not read as Ready")
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
		Expect(isTerminallyFailed(got)).To(BeFalse())

		By("events on the machine reconcile it again; the server is not asked again, and nothing is claimed")
		poke()
		poke()
		Consistently(func(g Gomega) {
			g.Expect(consumerName("unclaimed-host")).To(BeEmpty(), "no hardware is held for a digest that does not resolve")
			g.Expect(s.hits.Load()).To(Equal(int32(1)))
		}, 3*time.Second, 250*time.Millisecond).Should(Succeed())

		By("nothing the server sent reached the machine, an event or a log line")
		got = getMachine()
		expectNoDigestLeak(got, sink)
		conds, err := json.Marshal(got.Status.Conditions)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(conds)).NotTo(ContainSubstring(digestTestLeakMarker))
		events := &corev1.EventList{}
		Expect(k8sClient.List(ctx, events, client.InNamespace(ns))).To(Succeed())
		for _, e := range events.Items {
			Expect(e.Message).NotTo(ContainSubstring(digestTestLeakMarker))
		}
	})

	It("recovers when the file is fixed and the backoff has passed", func() {
		var body atomic.Value
		body.Store("# not yet\n")
		s := newChecksumServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, body.Load())
		}))
		DeferCleanup(s.Close)
		seedMachine(infrav1.Beskar7MachineSpec{TargetImageDigestURL: s.URL + "/SHA256SUMS"})
		seedHost("recovering-host")
		startManager(s)

		Eventually(func() string {
			return conditions.GetReason(getMachine(), infrav1.InfrastructureReadyCondition)
		}, 30*time.Second, 200*time.Millisecond).Should(Equal(infrav1.WaitingForTargetImageDigestReason))

		body.Store(gnuLine(digestTestHexA, digestTestImageName))
		key := types.NamespacedName{Namespace: ns, Name: machineName}
		v, ok := reconciler.digestRetries.Load(key)
		Expect(ok).To(BeTrue())
		retry, ok := v.(digestRetry)
		Expect(ok).To(BeTrue())
		retry.next = time.Now().Add(-time.Second)
		reconciler.digestRetries.Store(key, retry)
		poke()

		Eventually(func(g Gomega) {
			g.Expect(getMachine().Status.TargetImageDigest).To(Equal("sha256:" + digestTestHexA))
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
		Eventually(func() string { return consumerName("recovering-host") }, 30*time.Second, 200*time.Millisecond).Should(Equal(machineName))
		Expect(conditions.GetReason(getMachine(), infrav1.InfrastructureReadyCondition)).NotTo(Equal(infrav1.WaitingForTargetImageDigestReason))
	})

	It("fetches nothing for a machine that names the digest itself", func() {
		s := newChecksumServer(servingText(gnuLine(digestTestHexA, digestTestImageName)))
		DeferCleanup(s.Close)
		seedMachine(infrav1.Beskar7MachineSpec{TargetImageDigest: bootTestDigest})
		seedHost("plain-host")
		startManager(s)

		Eventually(func() string { return consumerName("plain-host") }, 30*time.Second, 200*time.Millisecond).Should(Equal(machineName))
		got := getMachine()
		Expect(got.Status.TargetImageDigest).To(BeEmpty())
		Expect(got.Status.TargetImageDigestURL).To(BeEmpty())
		Expect(s.hits.Load()).To(BeZero())
	})

	It("re-resolves a changed URL while no host is claimed, and keeps the pin once one is", func() {
		var mu sync.Mutex
		var requested []string
		files := map[string]string{
			"/v1/SHA256SUMS": digestTestHexA,
			"/v2/SHA256SUMS": digestTestHexB,
			"/v3/SHA256SUMS": digestTestHexC,
		}
		s := newChecksumServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			requested = append(requested, r.URL.Path)
			mu.Unlock()
			hex, ok := files[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = fmt.Fprint(w, gnuLine(hex, digestTestImageName))
		}))
		DeferCleanup(s.Close)
		seedMachine(infrav1.Beskar7MachineSpec{TargetImageDigestURL: s.URL + "/v1/SHA256SUMS"})
		startManager(s) // no host yet: the machine waits for one, pinned

		Eventually(func() string { return getMachine().Status.TargetImageDigest }, 30*time.Second, 200*time.Millisecond).
			Should(Equal("sha256:" + digestTestHexA))
		Eventually(func() string {
			return conditions.GetReason(getMachine(), infrav1.PhysicalHostAssociatedCondition)
		}, 30*time.Second, 200*time.Millisecond).Should(Equal(infrav1.WaitingForPhysicalHostReason))

		By("changing the URL while no host is claimed")
		setURL(s.URL + "/v2/SHA256SUMS")
		Eventually(func(g Gomega) {
			got := getMachine()
			g.Expect(got.Status.TargetImageDigest).To(Equal("sha256:" + digestTestHexB))
			g.Expect(got.Status.TargetImageDigestURL).To(Equal(s.URL + "/v2/SHA256SUMS"))
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		By("a host arriving and being claimed")
		seedHost("late-host")
		Eventually(func() string { return consumerName("late-host") }, 30*time.Second, 200*time.Millisecond).Should(Equal(machineName))

		By("changing the URL again, now that a host is claimed")
		setURL(s.URL + "/v3/SHA256SUMS")
		poke()
		Consistently(func(g Gomega) {
			got := getMachine()
			g.Expect(got.Status.TargetImageDigest).To(Equal("sha256:" + digestTestHexB))
			g.Expect(got.Status.TargetImageDigestURL).To(Equal(s.URL + "/v2/SHA256SUMS"))
		}, 3*time.Second, 250*time.Millisecond).Should(Succeed())
		mu.Lock()
		Expect(requested).To(Equal([]string{"/v1/SHA256SUMS", "/v2/SHA256SUMS"}), "the third URL is never read")
		mu.Unlock()
		var said bool
		for _, entry := range sink.snapshot() {
			if strings.Contains(entry, "keeping the pinned digest") {
				said = true
			}
		}
		Expect(said).To(BeTrue(), "the kept pin is announced in a log line")
	})

	It("does not fetch for a machine whose host is already Ready, as after a clusterctl move", func() {
		s := newChecksumServer(failsWith404())
		DeferCleanup(s.Close)
		seedMachine(infrav1.Beskar7MachineSpec{
			TargetImageDigestURL: s.URL + "/SHA256SUMS",
			ProviderID:           providerID(ns, "serving-host"),
		})
		host := claimedPhysicalHost(ns, "serving-host", machineName)
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		host.Status.State = infrav1.StateReady
		host.Status.Ready = true
		Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
		startManager(s)

		Eventually(func(g Gomega) {
			got := getMachine()
			g.Expect(got.Status.Ready).To(BeTrue())
			g.Expect(conditions.GetReason(got, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.ProvisionedReason))
			g.Expect(got.Status.TargetImageDigest).To(BeEmpty())
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
		Expect(s.hits.Load()).To(BeZero(), "the file is not even read")
	})
})

func failsWith404() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
}

// ── admission ────────────────────────────────────────────────────────────────

var _ = Describe("Admission of targetImageDigest and targetImageDigestURL (D-038)", func() {
	var ns string

	BeforeEach(func() {
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "digest-admission-"}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		ns = nsObj.Name
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	})

	const oneOf = "set exactly one of targetImageDigest and targetImageDigestURL"
	digest := "sha256:" + digestTestHexA
	sumsURL := "https://sums.example.invalid/v1/SHA256SUMS"

	baseSpec := func(digest, digestURL string) infrav1.Beskar7MachineSpec {
		return infrav1.Beskar7MachineSpec{
			InspectionImageURL:   "http://boot/inspect.ipxe",
			TargetImageURL:       digestTestImageURL,
			TargetImageDigest:    digest,
			TargetImageDigestURL: digestURL,
		}
	}
	machineOf := func(spec infrav1.Beskar7MachineSpec) client.Object {
		return &infrav1.Beskar7Machine{ObjectMeta: metav1.ObjectMeta{GenerateName: "admit-", Namespace: ns}, Spec: spec}
	}
	templateOf := func(spec infrav1.Beskar7MachineSpec) client.Object {
		return &infrav1.Beskar7MachineTemplate{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "admit-", Namespace: ns},
			Spec:       infrav1.Beskar7MachineTemplateSpec{Template: infrav1.Beskar7MachineTemplateResource{Spec: spec}},
		}
	}

	// The template embeds the same spec type, so the same rules have to hold in
	// the template CRD: a template that admits both would stamp out machines
	// that the machine CRD then refuses one by one.
	for _, kind := range []struct {
		name string
		make func(infrav1.Beskar7MachineSpec) client.Object
	}{
		{"Beskar7Machine", machineOf},
		{"Beskar7MachineTemplate", templateOf},
	} {
		Describe(kind.name, func() {
			DescribeTable("admission",
				func(spec infrav1.Beskar7MachineSpec, wantErr string) {
					err := k8sClient.Create(ctx, kind.make(spec))
					if wantErr == "" {
						Expect(err).NotTo(HaveOccurred())
						return
					}
					Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
					Expect(err.Error()).To(ContainSubstring(wantErr))
				},
				Entry("the digest alone", baseSpec(digest, ""), ""),
				Entry("the digest URL alone", baseSpec("", sumsURL), ""),
				Entry("the digest URL with a port and a query", baseSpec("", "https://sums.example.invalid:8443/v1/SHA256SUMS?sig=abc"), ""),
				Entry("both", baseSpec(digest, sumsURL), oneOf),
				Entry("neither", baseSpec("", ""), oneOf),
				Entry("an http digest URL", baseSpec("", "http://sums.example.invalid/v1/SHA256SUMS"), "targetImageDigestURL"),
				Entry("an uppercase scheme", baseSpec("", "HTTPS://sums.example.invalid/v1/SHA256SUMS"), "targetImageDigestURL"),
				Entry("another scheme", baseSpec("", "ftp://sums.example.invalid/SHA256SUMS"), "targetImageDigestURL"),
				Entry("no scheme", baseSpec("", "sums.example.invalid/SHA256SUMS"), "targetImageDigestURL"),
				Entry("whitespace in the URL", baseSpec("", "https://sums.example.invalid/SHA256 SUMS"), "targetImageDigestURL"),
				Entry("a URL over 2048 characters", baseSpec("", "https://sums.example.invalid/"+strings.Repeat("a", 2048)), "targetImageDigestURL"),
				Entry("a digest in capitals", baseSpec("sha256:"+strings.ToUpper(digestTestHexA), ""), "targetImageDigest"),
				Entry("a digest without the algorithm", baseSpec(digestTestHexA, ""), "targetImageDigest"),
			)
		})
	}

	It("lets a machine switch from one to the other in a single update, and refuses both on update", func() {
		m := machineOf(baseSpec(digest, "")).(*infrav1.Beskar7Machine)
		Expect(k8sClient.Create(ctx, m)).To(Succeed())

		both := m.DeepCopy()
		both.Spec.TargetImageDigestURL = sumsURL
		err := k8sClient.Update(ctx, both)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
		Expect(err.Error()).To(ContainSubstring(oneOf))

		switched := m.DeepCopy()
		switched.Spec.TargetImageDigest = ""
		switched.Spec.TargetImageDigestURL = sumsURL
		Expect(k8sClient.Update(ctx, switched)).To(Succeed())
	})

	It("accepts a machine written before the field existed, and still lets it be patched", func() {
		// What v0.10 stored: a digest, no URL, no pin in status.
		m := machineOf(baseSpec(digest, "")).(*infrav1.Beskar7Machine)
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		patched := m.DeepCopy()
		patched.Spec.ProviderID = providerID(ns, "some-host")
		Expect(k8sClient.Patch(ctx, patched, client.MergeFrom(m))).To(Succeed())
	})

	DescribeTable("status.targetImageDigest",
		func(value string, wantErr bool) {
			m := machineOf(baseSpec("", sumsURL)).(*infrav1.Beskar7Machine)
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			m.Status.TargetImageDigest = value
			m.Status.TargetImageDigestURL = sumsURL
			err := k8sClient.Status().Update(ctx, m)
			if wantErr {
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
				return
			}
			Expect(err).NotTo(HaveOccurred())
		},
		Entry("canonical", "sha256:"+digestTestHexA, false),
		Entry("capitals", "sha256:"+strings.ToUpper(digestTestHexA), true),
		Entry("short", "sha256:"+digestTestHexA[:63], true),
		Entry("injected arguments", "sha256:"+digestTestHexA+" beskar7.token=x", true),
	)
})
