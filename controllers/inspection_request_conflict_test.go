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
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

// The Beskar7Machine controller writes inspect-complete as the inspection
// report it has just validated lands, which is when the PhysicalHost reconciler
// writes the same host. The optimistic patch lost that race often enough to
// show on the lab in every provision, as a reconciler error:
//
//	failed to set inspection-request annotation "inspect-complete" on PhysicalHost ...:
//	Operation cannot be fulfilled on physicalhosts ...: the object has been modified
//
// although the next pass, a second later, wrote it without trouble (NOISE-2).
// The timeout request has the same exposure, and logged it at Error. These
// specs make the conflict real: another write to the host lands between the
// machine's read and its patch, so the API server itself answers 409.

// conflictLogEntry is one line the controller logged.
type conflictLogEntry struct {
	level int
	err   error
	msg   string
	kv    []any
}

func (e conflictLogEntry) String() string {
	return fmt.Sprint(e.level, " ", e.msg, " ", e.err, " ", e.kv)
}

// conflictLogStore holds the lines of every logger derived from one conflictLogs.
type conflictLogStore struct {
	mu      sync.Mutex
	entries []conflictLogEntry
}

// conflictLogs is a logr sink that keeps every line, at any verbosity, with the
// key-value pairs its logger was derived with.
type conflictLogs struct {
	store  *conflictLogStore
	values []any
}

func newConflictLogs() *conflictLogs {
	return &conflictLogs{store: &conflictLogStore{}}
}

func (s *conflictLogs) logger() logr.Logger { return logr.New(s) }

func (s *conflictLogs) Init(logr.RuntimeInfo)        {}
func (s *conflictLogs) Enabled(int) bool             { return true }
func (s *conflictLogs) WithName(string) logr.LogSink { return s }
func (s *conflictLogs) WithValues(kv ...any) logr.LogSink {
	return &conflictLogs{store: s.store, values: append(append([]any{}, s.values...), kv...)}
}
func (s *conflictLogs) Info(level int, msg string, kv ...any) {
	s.record(conflictLogEntry{level: level, msg: msg, kv: append(append([]any{}, s.values...), kv...)})
}
func (s *conflictLogs) Error(err error, msg string, kv ...any) {
	s.record(conflictLogEntry{err: err, msg: msg, kv: append(append([]any{}, s.values...), kv...)})
}

func (s *conflictLogs) record(e conflictLogEntry) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	s.store.entries = append(s.store.entries, e)
}

func (s *conflictLogs) all() []conflictLogEntry {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	return append([]conflictLogEntry{}, s.store.entries...)
}

// errors returns the lines logged at Error.
func (s *conflictLogs) errors() []string {
	var out []string
	for _, e := range s.all() {
		if e.err != nil {
			out = append(out, e.String())
		}
	}
	return out
}

// with returns the lines whose message contains text.
func (s *conflictLogs) with(text string) []conflictLogEntry {
	var out []conflictLogEntry
	for _, e := range s.all() {
		if strings.Contains(e.msg, text) {
			out = append(out, e)
		}
	}
	return out
}

func (s *conflictLogs) text() string {
	var b strings.Builder
	for _, e := range s.all() {
		b.WriteString(e.String() + "\n")
	}
	return b.String()
}

// conflictWrites sits between the machine controller and the API server and
// sees every write of an inspection request (a patch of a PhysicalHost that
// carries the annotation). For the first `conflicts` of them it first makes
// another writer's change to the host, so that the patch that follows is
// answered with a genuine conflict.
type conflictWrites struct {
	mu sync.Mutex
	// conflicts is how many request writes meet another writer's change first;
	// negative: every one.
	conflicts int
	// otherWriter makes the change that wins the race, given the 1-based number
	// of the write it beats. The default touches an unrelated annotation, as any
	// write by the host's own reconciler bumps the resourceVersion.
	otherWriter func(attempt int)
	// fail, when set, is returned for a request write instead of sending it.
	fail error
	// hostGone makes a read of the host answer NotFound once a request write
	// has been tried, as a cache does for a host deleted since.
	hostGone bool

	patches []string
}

// attempts returns how many request writes were sent so far.
func (w *conflictWrites) attempts() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.patches)
}

// binding returns the binding the nth (1-based) request write carried.
func (w *conflictWrites) binding(n int) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var p struct {
		Metadata struct {
			Annotations map[string]*string `json:"annotations"`
		} `json:"metadata"`
	}
	Expect(json.Unmarshal([]byte(w.patches[n-1]), &p)).To(Succeed())
	binding := p.Metadata.Annotations[callbackBindingAnnotation(InspectionRequestAnnotation)]
	Expect(binding).NotTo(BeNil(), "request write %d carries a binding", n)
	return *binding
}

// bindings returns the binding of every request write.
func (w *conflictWrites) bindings() []string {
	var out []string
	for n := 1; n <= w.attempts(); n++ {
		out = append(out, w.binding(n))
	}
	return out
}

// client returns a client over the API server, not its cache, that applies w
// to the requests written for the host.
func (w *conflictWrites) client(host client.ObjectKey) client.Client {
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
	Expect(err).NotTo(HaveOccurred())
	return interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, isHost := obj.(*infrav1.PhysicalHost); isHost {
				w.mu.Lock()
				gone := w.hostGone && len(w.patches) > 0
				w.mu.Unlock()
				if gone {
					return apierrors.NewNotFound(schema.GroupResource{Group: infrav1.GroupVersion.Group, Resource: "physicalhosts"}, key.Name)
				}
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, isHost := obj.(*infrav1.PhysicalHost); !isHost {
				return c.Patch(ctx, obj, patch, opts...)
			}
			data, err := patch.Data(obj)
			if err != nil {
				return err
			}
			if !strings.Contains(string(data), InspectionRequestAnnotation) {
				return c.Patch(ctx, obj, patch, opts...)
			}

			w.mu.Lock()
			w.patches = append(w.patches, string(data))
			attempt := len(w.patches)
			conflicts, fail, otherWriter := w.conflicts, w.fail, w.otherWriter
			w.mu.Unlock()

			if fail != nil {
				return fail
			}
			if conflicts < 0 || attempt <= conflicts {
				if otherWriter == nil {
					otherWriter = func(attempt int) {
						annotateHost(host, "test.beskar7.io/other-writer", strconv.Itoa(attempt))
					}
				}
				otherWriter(attempt)
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
}

var _ = Describe("An inspection request that conflicts with the host's own write (NOISE-2)", func() {
	var (
		ns             *corev1.Namespace
		hostReconciler *PhysicalHostReconciler
		logs           *conflictLogs
		writes         *conflictWrites
	)

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "request-conflict-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
		hostReconciler = &PhysicalHostReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                  ctrl.Log.WithName("request-conflict-host"),
			Recorder:             record.NewFakeRecorder(10),
			RedfishClientFactory: reachableBMC(),
		}
		logs = newConflictLogs()
		writes = &conflictWrites{}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	// report is what the host holds once its inspection is complete.
	report := func() *infrav1.InspectionReport {
		return buildInspectionReport(InspectionReportRequest{
			Manufacturer: "Acme", Model: "Fast-1000", CPUs: []CPUData{{ID: "cpu0", Cores: 8}},
		})
	}

	// inspectedHost stages a claimed host whose inspection has completed and
	// whose report the machine is about to validate, with its credentials.
	inspectedHost := func() (client.ObjectKey, *infrav1.Beskar7Machine, *clusterv1.Machine) {
		b7m, machine := consumerWithBootstrapData(ns.Name, "conflict-machine", "CONFLICT-BOOTSTRAP-DATA")
		b7m.Finalizers = []string{Beskar7MachineFinalizer}
		key := provisioningHost(ns.Name, "conflict-host", b7m.Name, infrav1.StateInspecting, nil)
		host := getPhysicalHost(key)
		host.Status.InspectionPhase = infrav1.InspectionPhaseComplete
		host.Status.InspectionReport = report()
		Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
		ensureCallbackCredentials(key)
		return key, b7m, machine
	}

	// stalledHost stages a claimed host whose inspection began a minute ago and
	// has reported nothing, which a 30-second timeout has outlasted.
	stalledHost := func() (client.ObjectKey, *infrav1.Beskar7Machine, *clusterv1.Machine) {
		b7m, machine := consumerWithBootstrapData(ns.Name, "stalled-machine", "STALLED-BOOTSTRAP-DATA")
		b7m.Finalizers = []string{Beskar7MachineFinalizer}
		key := provisioningHost(ns.Name, "stalled-host", b7m.Name, infrav1.StateInspecting, nil)
		ensureCallbackCredentials(key)
		return key, b7m, machine
	}

	// machineOver returns the machine controller reading and writing through
	// writes, with a log that keeps everything.
	machineOver := func(key client.ObjectKey, inspectionTimeout time.Duration) *Beskar7MachineReconciler {
		return &Beskar7MachineReconciler{
			Client: writes.client(key), Scheme: k8sClient.Scheme(),
			Log:                  logs.logger(),
			RedfishClientFactory: reachableBMC(),
			BootstrapURLBase:     "https://callback.example.com:8082",
			InspectionTimeout:    inspectionTimeout,
		}
	}

	expectNoRequest := func(key client.ObjectKey) {
		Expect(getPhysicalHost(key).Annotations).NotTo(HaveKey(InspectionRequestAnnotation))
		Expect(getPhysicalHost(key).Annotations).NotTo(HaveKey(callbackBindingAnnotation(InspectionRequestAnnotation)))
	}

	// expectNothingSecretLogged checks the log for the credentials and for every
	// binding the controller computed.
	expectNothingSecretLogged := func(key client.ObjectKey) {
		text := logs.text()
		Expect(text).NotTo(ContainSubstring(callbackTokenOf(key)), "the bearer token is never logged")
		for _, binding := range writes.bindings() {
			Expect(text).NotTo(ContainSubstring(binding), "a binding is never logged")
		}
	}

	Context("inspect-complete", func() {
		It("is retried in the same pass: no reconciler error, the signed request lands, and the host goes to Deploying", func() {
			key, b7m, machine := inspectedHost()
			writes.conflicts = 1
			r := machineOver(key, 0)

			result, err := r.reconcileNormal(ctx, r.Log, b7m, machine)

			Expect(err).NotTo(HaveOccurred(), "the conflict must not come back as a reconciler error")
			Expect(result.RequeueAfter).To(Equal(requeueShortly))
			Expect(isTerminallyFailed(b7m)).To(BeFalse())
			Expect(writes.attempts()).To(Equal(2), "one write lost the race, the retry won it")
			Expect(logs.errors()).To(BeEmpty(), "nothing is logged at Error for a conflict")

			landed := getPhysicalHost(key)
			Expect(landed.Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, "inspect-complete"))
			Expect(callbackBinderFor(key).holds(landed, InspectionRequestAnnotation, "")).To(BeTrue())
			Expect(landed.Annotations).To(HaveKeyWithValue("test.beskar7.io/other-writer", "1"),
				"the other writer's change is still there: the retry patched the host it read, not a copy of the old one")

			deploying := settlePhysicalHost(hostReconciler, key)
			Expect(deploying.Status.State).To(Equal(infrav1.StateDeploying))
			Expect(deploying.Status.DeployingTimestamp).NotTo(BeNil())
			expectNothingSecretLogged(key)
		})

		It("is signed again on the retry, so its binding verifies against the credentials as they stand", func() {
			key, b7m, machine := inspectedHost()
			writes.conflicts = 1
			writes.otherWriter = func(int) {
				// Credentials that changed while the request was in flight: a
				// binding computed from the first read no longer verifies.
				rewriteCredentials(key, func(data map[string][]byte) {
					data[bootNonceSecretKey] = []byte("rotated-while-the-write-conflicted")
				})
				annotateHost(key, "test.beskar7.io/other-writer", "rotated")
			}
			r := machineOver(key, 0)

			_, err := r.reconcileNormal(ctx, r.Log, b7m, machine)
			Expect(err).NotTo(HaveOccurred())
			Expect(writes.attempts()).To(Equal(2))

			first, second := writes.binding(1), writes.binding(2)
			Expect(second).NotTo(Equal(first), "the retry's binding was computed again")
			landed := getPhysicalHost(key)
			Expect(landed.Annotations).To(HaveKeyWithValue(callbackBindingAnnotation(InspectionRequestAnnotation), second))
			Expect(callbackBinderFor(key).holds(landed, InspectionRequestAnnotation, "")).To(BeTrue(),
				"the landed request verifies against the current credentials")
			stale := landed.DeepCopy()
			stale.Annotations[callbackBindingAnnotation(InspectionRequestAnnotation)] = first
			Expect(callbackBinderFor(key).holds(stale, InspectionRequestAnnotation, "")).To(BeFalse(),
				"the binding from the first read would not have")

			Expect(settlePhysicalHost(hostReconciler, key).Status.State).To(Equal(infrav1.StateDeploying),
				"the host reconciler, which checks the binding against a live read, acts on it")
			expectNothingSecretLogged(key)
		})

		It("does not return an error when every attempt conflicts, and the next pass writes it", func() {
			key, b7m, machine := inspectedHost()
			writes.conflicts = -1
			r := machineOver(key, 0)

			result, err := r.reconcileNormal(ctx, r.Log, b7m, machine)

			Expect(err).NotTo(HaveOccurred(), "an exhausted conflict budget is not a reconciler error")
			Expect(result.RequeueAfter).To(Equal(requeueShortly))
			Expect(isTerminallyFailed(b7m)).To(BeFalse())
			Expect(writes.attempts()).To(Equal(retry.DefaultBackoff.Steps), "every attempt of the budget was made")
			Expect(logs.errors()).To(BeEmpty())
			given := logs.with("kept changing")
			Expect(given).To(HaveLen(1), "the pass says why it wrote nothing")
			Expect(given[0].level).To(BeZero())
			Expect(given[0].kv).To(ContainElement(key.Name))
			Expect(getPhysicalHost(key).Annotations).NotTo(HaveKey(InspectionRequestAnnotation))
			Expect(getPhysicalHost(key).Status.State).To(Equal(infrav1.StateInspecting))

			By("the next pass, with no one else writing the host")
			next := machineOver(key, 0)
			writes.mu.Lock()
			writes.conflicts = 0
			writes.mu.Unlock()
			result, err = next.reconcileNormal(ctx, next.Log, b7m, machine)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(requeueShortly))
			Expect(getPhysicalHost(key).Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, "inspect-complete"))
			Expect(settlePhysicalHost(hostReconciler, key).Status.State).To(Equal(infrav1.StateDeploying))
			expectNothingSecretLogged(key)
		})

		It("still returns a failure that is not a conflict, without retrying it", func() {
			key, b7m, machine := inspectedHost()
			writes.fail = apierrors.NewForbidden(
				schema.GroupResource{Group: infrav1.GroupVersion.Group, Resource: "physicalhosts"}, key.Name, errors.New("denied"))
			r := machineOver(key, 0)

			_, err := r.reconcileNormal(ctx, r.Log, b7m, machine)

			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsForbidden(err)).To(BeTrue())
			Expect(writes.attempts()).To(Equal(1))
			expectNoRequest(key)
		})

		// The other writer's change is the one that makes the request moot. The
		// first write loses the race to it, and the retry has to see that.
		for _, moot := range []struct {
			name   string
			change func(key client.ObjectKey)
		}{
			{"the host is claimed by another machine", func(key client.ObjectKey) { setHostConsumer(key, "another-machine") }},
			{"the claim has been released", releasePhysicalHost},
			{"the host is no longer Inspecting", func(key client.ObjectKey) {
				host := getPhysicalHost(key)
				host.Status.State = infrav1.StateDeploying
				Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
			}},
			{"the inspection is no longer complete", func(key client.ObjectKey) {
				host := getPhysicalHost(key)
				host.Status.InspectionPhase = infrav1.InspectionPhaseFailed
				Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
			}},
			{"the inspection report is not the one that was validated", func(key client.ObjectKey) {
				host := getPhysicalHost(key)
				host.Status.InspectionReport = buildInspectionReport(InspectionReportRequest{
					Manufacturer: "Acme", Model: "A-Different-Box", CPUs: []CPUData{{ID: "cpu0", Cores: 2}},
				})
				Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
			}},
			{"the inspection report is gone", func(key client.ObjectKey) {
				host := getPhysicalHost(key)
				host.Status.InspectionReport = nil
				Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
			}},
			{"the host is gone", func(key client.ObjectKey) {
				annotateHost(key, "test.beskar7.io/other-writer", "last-write")
				writes.mu.Lock()
				writes.hostGone = true
				writes.mu.Unlock()
			}},
		} {
			moot := moot
			It("is not written on the retry when "+moot.name+": no patch, a quiet requeue", func() {
				key, b7m, machine := inspectedHost()
				writes.conflicts = 1
				writes.otherWriter = func(int) { moot.change(key) }
				r := machineOver(key, 0)

				result, err := r.reconcileNormal(ctx, r.Log, b7m, machine)

				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(requeueShortly))
				Expect(isTerminallyFailed(b7m)).To(BeFalse())
				Expect(writes.attempts()).To(Equal(1), "only the write that lost the race; the host that moved on is not patched again")
				expectNoRequest(key)
				Expect(logs.errors()).To(BeEmpty())
				stopped := logs.with("no longer applies")
				Expect(stopped).To(HaveLen(1))
				Expect(stopped[0].level).To(Equal(1), "stopping is logged at V(1)")
			})
		}
	})

	Context("timeout", func() {
		const timeout = 30 * time.Second

		expectFailedWithTimeout := func(b7m *infrav1.Beskar7Machine) {
			Expect(isTerminallyFailed(b7m)).To(BeTrue(), "the machine is marked failed whatever happens to the annotation")
			Expect(conditions.GetReason(b7m, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.InspectionTimedOutReason))
		}

		It("is retried in the same pass: the machine fails with InspectionTimedOut and nothing is logged at Error", func() {
			key, b7m, machine := stalledHost()
			writes.conflicts = 1
			r := machineOver(key, timeout)

			result, err := r.reconcileNormal(ctx, r.Log, b7m, machine)

			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
			expectFailedWithTimeout(b7m)
			Expect(writes.attempts()).To(Equal(2), "one write lost the race, the retry won it")
			Expect(logs.errors()).To(BeEmpty(), "a conflict is not logged at Error")

			landed := getPhysicalHost(key)
			Expect(landed.Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, "timeout"))
			Expect(callbackBinderFor(key).holds(landed, InspectionRequestAnnotation, "")).To(BeTrue())

			applied := settlePhysicalHost(hostReconciler, key)
			Expect(applied.Status.State).To(Equal(infrav1.StateError))
			Expect(applied.Status.ErrorMessage).To(Equal(inspectionTimedOutMessage))
			expectNothingSecretLogged(key)
		})

		It("fails the machine all the same when every attempt conflicts, and says so below Error", func() {
			key, b7m, machine := stalledHost()
			writes.conflicts = -1
			r := machineOver(key, timeout)

			result, err := r.reconcileNormal(ctx, r.Log, b7m, machine)

			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
			expectFailedWithTimeout(b7m)
			Expect(writes.attempts()).To(Equal(retry.DefaultBackoff.Steps))
			Expect(logs.errors()).To(BeEmpty())
			given := logs.with("kept changing")
			Expect(given).To(HaveLen(1))
			Expect(given[0].level).To(BeZero())
			Expect(given[0].err).To(BeNil())
			expectNoRequest(key)
			expectNothingSecretLogged(key)
		})

		It("still logs a failure that is not a conflict at Error, and fails the machine", func() {
			key, b7m, machine := stalledHost()
			writes.fail = apierrors.NewForbidden(
				schema.GroupResource{Group: infrav1.GroupVersion.Group, Resource: "physicalhosts"}, key.Name, errors.New("denied"))
			r := machineOver(key, timeout)

			_, err := r.reconcileNormal(ctx, r.Log, b7m, machine)

			Expect(err).NotTo(HaveOccurred())
			expectFailedWithTimeout(b7m)
			Expect(writes.attempts()).To(Equal(1), "a refusal is not retried")
			Expect(logs.errors()).To(HaveLen(1))
			Expect(logs.errors()[0]).To(ContainSubstring("Failed to set inspection timeout annotation"))
		})

		for _, moot := range []struct {
			name   string
			change func(key client.ObjectKey)
		}{
			{"the host is claimed by another machine", func(key client.ObjectKey) { setHostConsumer(key, "another-machine") }},
			{"the claim has been released", releasePhysicalHost},
			{"the host is no longer Inspecting", func(key client.ObjectKey) {
				host := getPhysicalHost(key)
				host.Status.State = infrav1.StateDeploying
				Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
			}},
		} {
			moot := moot
			It("is not written on the retry when "+moot.name+", and the machine still fails", func() {
				key, b7m, machine := stalledHost()
				writes.conflicts = 1
				writes.otherWriter = func(int) { moot.change(key) }
				r := machineOver(key, timeout)

				_, err := r.reconcileNormal(ctx, r.Log, b7m, machine)

				Expect(err).NotTo(HaveOccurred())
				expectFailedWithTimeout(b7m)
				Expect(writes.attempts()).To(Equal(1), "the host that moved on is not patched again")
				expectNoRequest(key)
				Expect(logs.errors()).To(BeEmpty())
				stopped := logs.with("no longer applies")
				Expect(stopped).To(HaveLen(1))
				Expect(stopped[0].level).To(Equal(1))
			})
		}
	})
})
