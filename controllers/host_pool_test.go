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
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/beskar7/beskar7/api/v1beta2"
)

// Pool tests turn the switch on through the reconciler's HostPools field and claimReader.

const (
	poolTestHostNamespace   = "beskar7-hosts"
	poolTestTenantNamespace = "team-a-dev"
	poolTestOtherNamespace  = "team-b"
	poolTestPool            = "default"
	poolTestGPUPool         = "team-a-gpu"
	poolTestTenantLabel     = "tenant"
	poolTestTenant          = "team-a"
	poolTestMachine         = "workers-x1"
	poolTestHost            = "host-a"
)

func poolTestNamespaces() []client.Object {
	return []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: poolTestHostNamespace}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   poolTestTenantNamespace,
			Labels: map[string]string{poolTestTenantLabel: poolTestTenant},
		}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: poolTestOtherNamespace}},
	}
}

// testPool grants the namespaces of the team-a tenant, or every namespace when tenant is "".
func testPool(name, tenant string) *infrav1.PhysicalHostPool {
	sel := &metav1.LabelSelector{}
	if tenant != "" {
		sel.MatchLabels = map[string]string{poolTestTenantLabel: tenant}
	}
	return &infrav1.PhysicalHostPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: infrav1.PhysicalHostPoolSpec{
			HostNamespaces:    []string{poolTestHostNamespace},
			NamespaceSelector: sel,
		},
	}
}

func testPoolHost(name, namespace string, pools ...string) *infrav1.PhysicalHost {
	lbls := map[string]string{}
	for _, p := range pools {
		lbls[poolLabelKey(p)] = ""
	}
	host := &infrav1.PhysicalHost{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: lbls},
		Spec:       infrav1.PhysicalHostSpec{Cleaning: &infrav1.CleaningSpec{Mode: infrav1.CleaningModeMetadata}},
		Status:     infrav1.PhysicalHostStatus{State: infrav1.StateAvailable},
	}
	// Wiped on its last release, as a pool claim requires.
	setTrue(host, infrav1.HostCleanedCondition, infrav1.HostCleanedReason)
	return host
}

// poolTestMachineUID is the UID testPoolMachine gives a machine, which claimedThrough stamps on the claim.
func poolTestMachineUID(namespace, name string) types.UID {
	return types.UID("uid-" + namespace + "-" + name)
}

func testPoolMachine(name, namespace, pool string) *infrav1.Beskar7Machine {
	return &infrav1.Beskar7Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: poolTestMachineUID(namespace, name)},
		Spec: infrav1.Beskar7MachineSpec{
			InspectionImageURL: "http://boot/inspect.ipxe",
			TargetImageURL:     "http://boot/kairos.raw",
			TargetImageDigest:  bootTestDigest,
			PoolRef:            pool,
		},
	}
}

func claimedThrough(host *infrav1.PhysicalHost, machineNamespace, machineName, pool string) *infrav1.PhysicalHost {
	host.Spec.ConsumerRef = &corev1.ObjectReference{
		Kind:       "Beskar7Machine",
		APIVersion: InfrastructureAPIVersion,
		Namespace:  machineNamespace,
		Name:       machineName,
		UID:        poolTestMachineUID(machineNamespace, machineName),
	}
	host.Spec.ConsumerPool = pool
	host.Status.State = infrav1.StateInUse
	return host
}

// newPoolTestClient registers the field indexes SetupWithManager registers on the real cache.
func newPoolTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newSchemeForTest(t)).
		WithObjects(objs...).
		WithStatusSubresource(&infrav1.PhysicalHost{}, &infrav1.PhysicalHostPool{}).
		WithIndex(&infrav1.PhysicalHost{}, PhysicalHostStateIndex, func(obj client.Object) []string {
			return []string{obj.(*infrav1.PhysicalHost).Status.State}
		}).
		WithIndex(&infrav1.PhysicalHost{}, PhysicalHostConsumerPoolIndex, func(obj client.Object) []string {
			if p := obj.(*infrav1.PhysicalHost).Spec.ConsumerPool; p != "" {
				return []string{p}
			}
			return nil
		}).
		WithIndex(&infrav1.PhysicalHost{}, PhysicalHostConsumerIndex, physicalHostConsumer).
		WithIndex(&infrav1.Beskar7Machine{}, Beskar7MachinePoolRefIndex, func(obj client.Object) []string {
			if p := obj.(*infrav1.Beskar7Machine).Spec.PoolRef; p != "" {
				return []string{p}
			}
			return nil
		}).
		Build()
}

func TestResolveConsumerThroughPool(t *testing.T) {
	tenantKey := types.NamespacedName{Namespace: poolTestTenantNamespace, Name: poolTestMachine}
	tests := []struct {
		name     string
		disabled bool
		pools    []*infrav1.PhysicalHostPool
		host     *infrav1.PhysicalHost
		// machine replaces the consumer machine, which by default proves the claim.
		machine   *infrav1.Beskar7Machine
		noMachine bool
		wantOK    bool
	}{
		{
			name:   "same namespace resolves without a pool",
			host:   claimedThrough(testPoolHost(poolTestHost, poolTestTenantNamespace), poolTestTenantNamespace, poolTestMachine, ""),
			wantOK: true,
		},
		{
			name:     "cross namespace is rejected while host pools are disabled",
			disabled: true,
			pools:    []*infrav1.PhysicalHostPool{testPool(poolTestPool, "")},
			host:     claimedThrough(testPoolHost(poolTestHost, poolTestHostNamespace, poolTestPool), poolTestTenantNamespace, poolTestMachine, poolTestPool),
		},
		{
			name:  "cross namespace without consumerPool is rejected (SEC-12)",
			pools: []*infrav1.PhysicalHostPool{testPool(poolTestPool, "")},
			host:  claimedThrough(testPoolHost(poolTestHost, poolTestHostNamespace, poolTestPool), poolTestTenantNamespace, poolTestMachine, ""),
		},
		{
			name: "missing pool is rejected",
			host: claimedThrough(testPoolHost(poolTestHost, poolTestHostNamespace, poolTestPool), poolTestTenantNamespace, poolTestMachine, poolTestPool),
		},
		{
			name:  "host without the pool label is rejected",
			pools: []*infrav1.PhysicalHostPool{testPool(poolTestPool, "")},
			host:  claimedThrough(testPoolHost(poolTestHost, poolTestHostNamespace), poolTestTenantNamespace, poolTestMachine, poolTestPool),
		},
		{
			name:  "host outside hostNamespaces is rejected",
			pools: []*infrav1.PhysicalHostPool{testPool(poolTestPool, "")},
			host:  claimedThrough(testPoolHost(poolTestHost, poolTestOtherNamespace, poolTestPool), poolTestTenantNamespace, poolTestMachine, poolTestPool),
		},
		{
			name:  "pool that does not grant the consumer namespace is rejected",
			pools: []*infrav1.PhysicalHostPool{testPool(poolTestGPUPool, "someone-else")},
			host:  claimedThrough(testPoolHost(poolTestHost, poolTestHostNamespace, poolTestGPUPool), poolTestTenantNamespace, poolTestMachine, poolTestGPUPool),
		},
		{
			name:  "membership in another granting pool does not count",
			pools: []*infrav1.PhysicalHostPool{testPool(poolTestPool, ""), testPool(poolTestGPUPool, poolTestTenant)},
			host:  claimedThrough(testPoolHost(poolTestHost, poolTestHostNamespace, poolTestPool), poolTestTenantNamespace, poolTestMachine, poolTestGPUPool),
		},
		{
			name:   "member of the claim's pool and granted resolves to the consumer namespace",
			pools:  []*infrav1.PhysicalHostPool{testPool(poolTestGPUPool, poolTestTenant)},
			host:   claimedThrough(testPoolHost(poolTestHost, poolTestHostNamespace, poolTestPool, poolTestGPUPool), poolTestTenantNamespace, poolTestMachine, poolTestGPUPool),
			wantOK: true,
		},
		{
			name:      "claim naming a machine that does not exist is rejected",
			pools:     []*infrav1.PhysicalHostPool{testPool(poolTestPool, "")},
			host:      claimedThrough(testPoolHost(poolTestHost, poolTestHostNamespace, poolTestPool), poolTestTenantNamespace, poolTestMachine, poolTestPool),
			noMachine: true,
		},
		{
			name:  "forged claim without the machine's UID is rejected",
			pools: []*infrav1.PhysicalHostPool{testPool(poolTestPool, "")},
			host: withConsumerUID(claimedThrough(testPoolHost(poolTestHost, poolTestHostNamespace, poolTestPool),
				poolTestTenantNamespace, poolTestMachine, poolTestPool), "forged"),
		},
		{
			name:  "claim with no UID is rejected",
			pools: []*infrav1.PhysicalHostPool{testPool(poolTestPool, "")},
			host: withConsumerUID(claimedThrough(testPoolHost(poolTestHost, poolTestHostNamespace, poolTestPool),
				poolTestTenantNamespace, poolTestMachine, poolTestPool), ""),
		},
		{
			name:    "claim through a pool the machine does not name is rejected",
			pools:   []*infrav1.PhysicalHostPool{testPool(poolTestPool, ""), testPool(poolTestGPUPool, "")},
			host:    claimedThrough(testPoolHost(poolTestHost, poolTestHostNamespace, poolTestPool, poolTestGPUPool), poolTestTenantNamespace, poolTestMachine, poolTestGPUPool),
			machine: testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool),
		},
		{
			name:  "moved machine proves the claim by providerID",
			pools: []*infrav1.PhysicalHostPool{testPool(poolTestPool, "")},
			host: withConsumerUID(claimedThrough(testPoolHost(poolTestHost, poolTestHostNamespace, poolTestPool),
				poolTestTenantNamespace, poolTestMachine, poolTestPool), "uid-before-move"),
			machine: withProviderID(testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool),
				providerID(poolTestHostNamespace, poolTestHost)),
			wantOK: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := poolTestNamespaces()
			for _, p := range tt.pools {
				objs = append(objs, p)
			}
			if !tt.noMachine {
				machine := tt.machine
				if machine == nil {
					machine = testPoolMachine(poolTestMachine, poolTestTenantNamespace, tt.host.Spec.ConsumerPool)
				}
				objs = append(objs, machine)
			}
			c := newPoolTestClient(t, objs...)
			consumer, ok, err := resolveConsumer(context.Background(), claimReader{Reader: c, hostPools: !tt.disabled}, tt.host)
			if err != nil {
				t.Fatalf("resolveConsumer: %v", err)
			}
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && consumer.key != tenantKey {
				t.Fatalf("key = %v, want %v", consumer.key, tenantKey)
			}
		})
	}
}

func withConsumerUID(host *infrav1.PhysicalHost, uid types.UID) *infrav1.PhysicalHost {
	host.Spec.ConsumerRef.UID = uid
	return host
}

func withProviderID(machine *infrav1.Beskar7Machine, id string) *infrav1.Beskar7Machine {
	machine.Spec.ProviderID = id
	return machine
}

// TestResolveConsumerReadErrorIsUnverifiable checks that a failed read of the
// pool, the Namespace or the machine is reported as unreadable, not as a
// refusal, so a signed report is kept for the next reconcile.
func TestResolveConsumerReadErrorIsUnverifiable(t *testing.T) {
	for _, kind := range []client.Object{&infrav1.PhysicalHostPool{}, &corev1.Namespace{}, &infrav1.Beskar7Machine{}} {
		t.Run(fmt.Sprintf("%T", kind), func(t *testing.T) {
			objs := append(poolTestNamespaces(),
				testPool(poolTestPool, ""),
				testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool))
			failing := failGetOf(newPoolTestClient(t, objs...), kind)
			host := claimedThrough(testPoolHost(poolTestHost, poolTestHostNamespace, poolTestPool),
				poolTestTenantNamespace, poolTestMachine, poolTestPool)
			_, ok, err := resolveConsumer(context.Background(), claimReader{Reader: failing, hostPools: true}, host)
			if ok || !errors.Is(err, errConsumerUnreadable) {
				t.Fatalf("ok = %v, err = %v; want an errConsumerUnreadable", ok, err)
			}
			verdict := checkCallbackAnnotation(callbackCheck{
				Logger: ctrl.Log, Host: host, Key: "k", CredsErr: err,
			})
			if verdict != callbackUnverifiable {
				t.Fatalf("verdict = %v, want callbackUnverifiable", verdict)
			}
		})
	}
}

// failGetOf returns a client whose Get fails with a server error for objects of kind's type.
func failGetOf(c client.Client, kind client.Object) client.Client {
	return interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if fmt.Sprintf("%T", obj) == fmt.Sprintf("%T", kind) {
				return apierrors.NewServiceUnavailable("api server unavailable")
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
}

func newPoolTestReconciler(c client.Client) *Beskar7MachineReconciler {
	return &Beskar7MachineReconciler{Client: c, Log: ctrl.Log.WithName("host-pool-test"), HostPools: true}
}

func TestClaimThroughPool(t *testing.T) {
	ctx := context.Background()
	machine := testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool)
	objs := append(poolTestNamespaces(),
		testPool(poolTestPool, poolTestTenant),
		testPoolHost(poolTestHost, poolTestHostNamespace, poolTestPool),
		machine,
	)
	c := newPoolTestClient(t, objs...)
	r := newPoolTestReconciler(c)

	got, _, err := r.findAndClaimOrGetAssociatedHost(ctx, r.Log, machine, nil)
	if err != nil || got == nil {
		t.Fatalf("claim: host=%v err=%v", got, err)
	}
	host := &infrav1.PhysicalHost{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: poolTestHostNamespace, Name: poolTestHost}, host); err != nil {
		t.Fatal(err)
	}
	if cr := host.Spec.ConsumerRef; cr == nil || cr.Namespace != poolTestTenantNamespace || cr.Name != poolTestMachine {
		t.Fatalf("consumerRef = %+v, want %s/%s", cr, poolTestTenantNamespace, poolTestMachine)
	}
	if host.Spec.ConsumerPool != poolTestPool {
		t.Fatalf("consumerPool = %q, want %q", host.Spec.ConsumerPool, poolTestPool)
	}

	// The next pass re-finds the held host instead of claiming another one.
	again, _, err := r.findAndClaimOrGetAssociatedHost(ctx, r.Log, machine, nil)
	if err != nil || again == nil || again.Name != poolTestHost {
		t.Fatalf("re-find: host=%v err=%v", again, err)
	}
}

func TestClaimThroughPoolNotAvailable(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		disabled bool
		pool     *infrav1.PhysicalHostPool
	}{
		{name: "host pools disabled", disabled: true, pool: testPool(poolTestPool, "")},
		{name: "pool missing"},
		{name: "pool does not grant the namespace", pool: testPool(poolTestPool, "someone-else")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			machine := testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool)
			objs := append(poolTestNamespaces(), testPoolHost(poolTestHost, poolTestHostNamespace, poolTestPool), machine)
			if tt.pool != nil {
				objs = append(objs, tt.pool)
			}
			r := newPoolTestReconciler(newPoolTestClient(t, objs...))
			r.HostPools = !tt.disabled
			got, _, err := r.findAndClaimOrGetAssociatedHost(ctx, r.Log, machine, nil)
			if !errors.Is(err, errPoolNotAvailable) || got != nil {
				t.Fatalf("host=%v err=%v, want errPoolNotAvailable", got, err)
			}
		})
	}
}

func TestClaimThroughPoolSkipsHostsOutsideThePool(t *testing.T) {
	ctx := context.Background()
	machine := testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool)
	notWiped := testPoolHost("not-wiped", poolTestHostNamespace, poolTestPool)
	// Released with the wipe skipped, then set back to Metadata: the mode alone must not make it claimable.
	setFalse(notWiped, infrav1.HostCleanedCondition, infrav1.CleaningSkippedReason, "released without a wipe")
	objs := append(poolTestNamespaces(),
		testPool(poolTestPool, ""),
		notWiped,
		testPoolHost("other-pool", poolTestHostNamespace, poolTestGPUPool),
		testPoolHost("wrong-namespace", poolTestOtherNamespace, poolTestPool),
		testPoolHost("own-namespace", poolTestTenantNamespace),
		machine,
	)
	r := newPoolTestReconciler(newPoolTestClient(t, objs...))
	got, _, err := r.findAndClaimOrGetAssociatedHost(ctx, r.Log, machine, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("claimed %s/%s; a pool claim takes only wiped members of its pool, with no fallback", got.Namespace, got.Name)
	}
}

func TestReleaseThroughPoolAfterGrantRevoked(t *testing.T) {
	ctx := context.Background()
	machine := testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool)
	host := claimedThrough(testPoolHost(poolTestHost, poolTestHostNamespace, poolTestPool), poolTestTenantNamespace, poolTestMachine, poolTestPool)
	objs := append(poolTestNamespaces(), testPool(poolTestPool, "someone-else"), host, machine)
	r := newPoolTestReconciler(newPoolTestClient(t, objs...))

	got, err := r.findClaimedHostForRelease(ctx, r.Log, machine)
	if err != nil || got == nil || got.Name != poolTestHost {
		t.Fatalf("release lookup: host=%v err=%v; a revoked grant must not strand the host", got, err)
	}
}

func TestPoolClaimIgnoresForgedHostOutsideThePool(t *testing.T) {
	ctx := context.Background()
	machine := testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool)
	// Someone with patch rights in team-b points their own host at the tenant's machine.
	forged := withConsumerUID(claimedThrough(testPoolHost("forged", poolTestOtherNamespace, poolTestPool),
		poolTestTenantNamespace, poolTestMachine, poolTestPool), "forged")
	objs := append(poolTestNamespaces(), testPool(poolTestPool, ""), forged, machine)
	r := newPoolTestReconciler(newPoolTestClient(t, objs...))

	got, _, err := r.findAndClaimOrGetAssociatedHost(ctx, r.Log, machine, nil)
	if err != nil || got != nil {
		t.Fatalf("host=%v err=%v; a claim the machine cannot prove is never adopted", got, err)
	}
}

func TestReleaseFindsPoolHostAfterPoolOrSwitchChanged(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		disabled bool
		pool     *infrav1.PhysicalHostPool
	}{
		{name: "host namespace dropped from hostNamespaces", pool: func() *infrav1.PhysicalHostPool {
			p := testPool(poolTestPool, "")
			p.Spec.HostNamespaces = []string{poolTestOtherNamespace}
			return p
		}()},
		{name: "pool label removed while claimed", pool: testPool(poolTestPool, "")},
		{name: "host pools switched off", disabled: true, pool: testPool(poolTestPool, "")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			machine := testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool)
			labelled := []string{poolTestPool}
			if tt.name == "pool label removed while claimed" {
				labelled = nil
			}
			host := claimedThrough(testPoolHost(poolTestHost, poolTestHostNamespace, labelled...), poolTestTenantNamespace, poolTestMachine, poolTestPool)
			r := newPoolTestReconciler(newPoolTestClient(t, append(poolTestNamespaces(), tt.pool, host, machine)...))
			r.HostPools = !tt.disabled

			got, err := r.findClaimedHostForRelease(ctx, r.Log, machine)
			if err != nil || got == nil || got.Name != poolTestHost {
				t.Fatalf("release lookup: host=%v err=%v; the host would stay claimed by a deleted machine", got, err)
			}
		})
	}
}

// TestReleaseIgnoresForgedClaim checks that a forged claim naming a machine and
// its pool cannot take the place of the machine's real host on release.
func TestReleaseIgnoresForgedClaim(t *testing.T) {
	machine := testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool)
	// The forged host sorts first, so a lookup without the proof would return it.
	forged := withConsumerUID(claimedThrough(testPoolHost("forged", "a-forger", poolTestPool),
		poolTestTenantNamespace, poolTestMachine, poolTestPool), "forged")
	real := claimedThrough(testPoolHost(poolTestHost, poolTestHostNamespace, poolTestPool),
		poolTestTenantNamespace, poolTestMachine, poolTestPool)
	r := newPoolTestReconciler(newPoolTestClient(t,
		append(poolTestNamespaces(), testPool(poolTestPool, ""), forged, real, machine)...))

	got, err := r.findClaimedHostForRelease(context.Background(), r.Log, machine)
	if err != nil || got == nil || got.Name != poolTestHost {
		t.Fatalf("release lookup: host=%v err=%v; want the machine's real host %s", got, err, poolTestHost)
	}
}

func TestAvailablePoolHostWakesWaitingMachinesInOtherNamespaces(t *testing.T) {
	host := testPoolHost(poolTestHost, poolTestHostNamespace, poolTestPool)
	waiting := testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool)
	otherPool := testPoolMachine("gpu-1", poolTestTenantNamespace, poolTestGPUPool)
	r := newPoolTestReconciler(newPoolTestClient(t, host, waiting, otherPool))

	reqs := r.AvailablePhysicalHostToWaitingBeskar7Machines(context.Background(), host)
	if len(reqs) != 1 || reqs[0].NamespacedName != client.ObjectKeyFromObject(waiting) {
		t.Fatalf("requests = %v, want only %s", reqs, client.ObjectKeyFromObject(waiting))
	}
}

func TestPoolFinalizerWaitsForClaims(t *testing.T) {
	ctx := context.Background()
	pool := testPool(poolTestPool, "")
	pool.Finalizers = []string{infrav1.PhysicalHostPoolFinalizer}
	host := claimedThrough(testPoolHost(poolTestHost, poolTestHostNamespace, poolTestPool), poolTestTenantNamespace, poolTestMachine, poolTestPool)
	c := newPoolTestClient(t, pool, host)
	r := &PhysicalHostPoolReconciler{Client: c}
	key := client.ObjectKeyFromObject(pool)

	if err := c.Delete(ctx, pool); err != nil {
		t.Fatal(err)
	}
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if err != nil || res.RequeueAfter == 0 {
		t.Fatalf("result=%v err=%v, want a requeue while a host is claimed through the pool", res, err)
	}
	if err := c.Get(ctx, key, &infrav1.PhysicalHostPool{}); err != nil {
		t.Fatalf("pool gone while still claimed: %v", err)
	}

	released := &infrav1.PhysicalHost{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(host), released); err != nil {
		t.Fatal(err)
	}
	released.Spec.ConsumerRef = nil
	released.Spec.ConsumerPool = ""
	if err := c.Update(ctx, released); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, &infrav1.PhysicalHostPool{}); err == nil {
		t.Fatal("pool still present after its last claim was released")
	}
}

// TestPoolFinalizerIgnoresLeftovers checks that a consumerPool with no claim, and
// a claim on a host outside the pool's hostNamespaces, do not hold the pool.
func TestPoolFinalizerIgnoresLeftovers(t *testing.T) {
	ctx := context.Background()
	pool := testPool(poolTestPool, "")
	pool.Finalizers = []string{infrav1.PhysicalHostPoolFinalizer}
	// A dangling consumerRef cleared by hand leaves consumerPool behind.
	leftover := testPoolHost("leftover", poolTestHostNamespace, poolTestPool)
	leftover.Spec.ConsumerPool = poolTestPool
	outside := claimedThrough(testPoolHost("outside", poolTestOtherNamespace, poolTestPool),
		poolTestTenantNamespace, poolTestMachine, poolTestPool)
	c := newPoolTestClient(t, pool, leftover, outside)
	key := client.ObjectKeyFromObject(pool)
	if err := c.Delete(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := (&PhysicalHostPoolReconciler{Client: c}).Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, &infrav1.PhysicalHostPool{}); err == nil {
		t.Fatal("pool held by a host that is not claimed through it")
	}
}

func TestPausedPoolIsLeftAlone(t *testing.T) {
	ctx := context.Background()
	pool := testPool(poolTestPool, "")
	pool.Annotations = map[string]string{clusterv1.PausedAnnotation: ""}
	c := newPoolTestClient(t, pool)
	key := client.ObjectKeyFromObject(pool)
	if _, err := (&PhysicalHostPoolReconciler{Client: c}).Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	got := &infrav1.PhysicalHostPool{}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if len(got.Finalizers) != 0 || len(got.Status.Conditions) != 0 {
		t.Fatalf("paused pool was written: finalizers %v, conditions %v", got.Finalizers, got.Status.Conditions)
	}
}

// claimedByMachine stamps the machine's UID on the claim, as a real claim does.
func claimedByMachine(host *infrav1.PhysicalHost, m *infrav1.Beskar7Machine) *infrav1.PhysicalHost {
	claimedThrough(host, m.Namespace, m.Name, m.Spec.PoolRef)
	host.Spec.ConsumerRef.UID = m.UID
	return host
}

func TestHeldPoolHostStaysHeldAfterItLeavesThePool(t *testing.T) {
	ctx := context.Background()
	machine := testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool)
	held := claimedByMachine(testPoolHost(poolTestHost, poolTestHostNamespace), machine) // pool label removed
	spare := testPoolHost("spare", poolTestHostNamespace, poolTestPool)
	c := newPoolTestClient(t, append(poolTestNamespaces(), testPool(poolTestPool, poolTestTenant), held, spare, machine)...)
	r := newPoolTestReconciler(c)

	got, _, err := r.findAndClaimOrGetAssociatedHost(ctx, r.Log, machine, nil)
	if err != nil || got == nil || got.Name != poolTestHost {
		t.Fatalf("host=%v err=%v; the machine must keep its host, not claim a second one", got, err)
	}
	valid, err := r.markPoolClaim(ctx, machine, got)
	if err != nil {
		t.Fatal(err)
	}
	if valid {
		t.Fatal("markPoolClaim reported a claim on a host that left its pool as valid")
	}
	cond := conditions.Get(machine, infrav1.PoolClaimValidCondition)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != infrav1.PoolGrantRevokedReason {
		t.Fatalf("PoolClaimValid = %+v, want False/%s", cond, infrav1.PoolGrantRevokedReason)
	}
}

func TestRevokedPoolClaimStartsNothingNew(t *testing.T) {
	ctx := context.Background()
	machine := testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool)
	machine.Finalizers = []string{Beskar7MachineFinalizer}
	// The pool no longer grants the tenant namespace.
	held := claimedByMachine(testPoolHost(poolTestHost, poolTestHostNamespace, poolTestPool), machine)
	held.Status.State = infrav1.StateInUse
	c := newPoolTestClient(t, append(poolTestNamespaces(), testPool(poolTestPool, "other-tenant"), held, machine)...)
	r := newPoolTestReconciler(c)

	capiMachine := &clusterv1.Machine{Spec: clusterv1.MachineSpec{ClusterName: "fake-cluster"}}
	res, err := r.reconcileNormal(ctx, r.Log, machine, capiMachine)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != time.Minute {
		t.Fatalf("result = %+v, want a one minute backstop requeue", res)
	}
	if cond := conditions.Get(machine, infrav1.PoolClaimValidCondition); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("PoolClaimValid = %+v, want False", cond)
	}
	// The pass stops before the bootstrap check and the state machine that boots the host.
	if conditions.Has(machine, infrav1.BootstrapDataReadyCondition) {
		t.Fatal("a revoked claim went on to the bootstrap check")
	}
	// A machine without InfrastructureReady, such as one recreated by a move, must not summarise to Ready=True.
	if cond := conditions.Get(machine, infrav1.InfrastructureReadyCondition); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("InfrastructureReady = %+v, want False", cond)
	}
	got := &infrav1.PhysicalHost{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(held), got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.ConsumerRef == nil || got.Status.State != infrav1.StateInUse || len(got.Annotations) != len(held.Annotations) {
		t.Fatalf("host = consumerRef %v, state %q, annotations %v; want it held and untouched",
			got.Spec.ConsumerRef, got.Status.State, got.Annotations)
	}
}

func TestHostErrorForMachine(t *testing.T) {
	const bmcError = "BMC https://10.0.0.5 rejected credentials from Secret beskar7-hosts/bmc-node-07"
	machine := testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool)
	tests := []struct {
		name      string
		namespace string
		msg       string
		want      string
	}{
		{name: "own namespace shows the error", namespace: poolTestTenantNamespace, msg: bmcError, want: bmcError},
		{name: "other namespace withholds a BMC error", namespace: poolTestHostNamespace, msg: bmcError, want: hostErrorWithheld},
		{
			name: "other namespace shows the machine's own deploy failure", namespace: poolTestHostNamespace,
			msg: provisionFailedReasonPrefix + "disk not found", want: provisionFailedReasonPrefix + "disk not found",
		},
		{
			name: "other namespace shows an inspection timeout", namespace: poolTestHostNamespace,
			msg: inspectionTimedOutMessage, want: inspectionTimedOutMessage,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host := testPoolHost(poolTestHost, tt.namespace)
			host.Status.ErrorMessage = tt.msg
			if got := hostErrorForMachine(machine, host); got != tt.want {
				t.Fatalf("hostErrorForMachine = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPoolStatusCountsAndRevokedClaims(t *testing.T) {
	ctx := context.Background()
	pool := testPool(poolTestPool, poolTestTenant)
	pool.Finalizers = []string{infrav1.PhysicalHostPoolFinalizer}
	machine := testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool)
	free := testPoolHost("free", poolTestHostNamespace, poolTestPool)
	notWiped := testPoolHost("not-wiped", poolTestHostNamespace, poolTestPool)
	// Released with the wipe skipped, then set back to Metadata: the mode alone must not make it claimable.
	setFalse(notWiped, infrav1.HostCleanedCondition, infrav1.CleaningSkippedReason, "released without a wipe")
	valid := claimedByMachine(testPoolHost("valid", poolTestHostNamespace, poolTestPool), machine)
	revoked := claimedThrough(testPoolHost("revoked", poolTestHostNamespace), poolTestTenantNamespace, "gone-1", poolTestPool)
	c := newPoolTestClient(t, append(poolTestNamespaces(), pool, free, notWiped, valid, revoked)...)

	if _, err := (&PhysicalHostPoolReconciler{Client: c}).Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}); err != nil {
		t.Fatal(err)
	}
	got := &infrav1.PhysicalHostPool{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pool), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Members != 3 || got.Status.Available != 1 || got.Status.Claimed != 2 {
		t.Fatalf("status = members %d, available %d, claimed %d; want 3, 1, 2", got.Status.Members, got.Status.Available, got.Status.Claimed)
	}
	cond := conditions.Get(got, infrav1.ClaimsValidCondition)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != infrav1.PoolGrantRevokedReason {
		t.Fatalf("ClaimsValid = %+v, want False/%s", cond, infrav1.PoolGrantRevokedReason)
	}
}

func TestHardwareMismatchReleasesPoolHost(t *testing.T) {
	ctx := context.Background()
	machine := testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool)
	machine.Spec.HardwareRequirements = &infrav1.HardwareRequirements{MinCPUCores: 64}
	host := claimedByMachine(testPoolHost(poolTestHost, poolTestHostNamespace, poolTestPool), machine)
	host.Status.State = infrav1.StateInspecting
	host.Status.InspectionReport = &infrav1.InspectionReport{CPUs: []infrav1.CPUInfo{{Cores: 8}}}
	c := newPoolTestClient(t, append(poolTestNamespaces(), testPool(poolTestPool, ""), host, machine)...)
	r := newPoolTestReconciler(c)

	if _, err := r.validateInspectionReport(ctx, r.Log, machine, host); err != nil {
		t.Fatal(err)
	}
	if !isTerminallyFailed(machine) {
		t.Fatal("machine not terminally failed on a hardware mismatch")
	}
	got := &infrav1.PhysicalHost{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(host), got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.ConsumerRef != nil || got.Spec.ConsumerPool != "" {
		t.Fatalf("pool host still claimed after the mismatch: consumerRef=%+v consumerPool=%q", got.Spec.ConsumerRef, got.Spec.ConsumerPool)
	}
}

func TestMovedPoolMachineFindsItsHostByProviderID(t *testing.T) {
	ctx := context.Background()
	source := testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool)
	held := claimedByMachine(testPoolHost(poolTestHost, poolTestHostNamespace, poolTestPool), source)
	spare := testPoolHost("spare", poolTestHostNamespace, poolTestPool)
	// clusterctl move recreates the machine with a new UID and keeps its providerID.
	moved := testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool)
	moved.UID = "uid-after-move"
	moved.Spec.ProviderID = providerID(poolTestHostNamespace, poolTestHost)
	c := newPoolTestClient(t, append(poolTestNamespaces(), testPool(poolTestPool, poolTestTenant), held, spare, moved)...)
	r := newPoolTestReconciler(c)

	got, _, err := r.findAndClaimOrGetAssociatedHost(ctx, r.Log, moved, nil)
	if err != nil || got == nil || got.Name != poolTestHost {
		t.Fatalf("host=%v err=%v; the moved machine must find its host by providerID", got, err)
	}
	if consumer := mustGetHost(t, c, "spare").Spec.ConsumerRef; consumer != nil {
		t.Fatalf("spare claimed by %v; a moved machine must not claim a second host", consumer)
	}
}

func TestPoolMachineWithProviderIDNeverClaimsASecondHost(t *testing.T) {
	ctx := context.Background()
	// The host named by providerID has not been moved yet; a free member has.
	machine := testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool)
	machine.Spec.ProviderID = providerID(poolTestHostNamespace, poolTestHost)
	spare := testPoolHost("spare", poolTestHostNamespace, poolTestPool)
	c := newPoolTestClient(t, append(poolTestNamespaces(), testPool(poolTestPool, poolTestTenant), spare, machine)...)
	r := newPoolTestReconciler(c)

	got, _, err := r.findAndClaimOrGetAssociatedHost(ctx, r.Log, machine, nil)
	if !errors.Is(err, errPoolHostMissing) || got != nil {
		t.Fatalf("host=%v err=%v; want errPoolHostMissing and no host", got, err)
	}
	if consumer := mustGetHost(t, c, "spare").Spec.ConsumerRef; consumer != nil {
		t.Fatalf("spare claimed by %v", consumer)
	}
}

func TestProviderIDAloneDoesNotAdoptAnotherMachinesHost(t *testing.T) {
	ctx := context.Background()
	other := testPoolMachine("other", poolTestTenantNamespace, poolTestPool)
	held := claimedByMachine(testPoolHost(poolTestHost, poolTestHostNamespace, poolTestPool), other)
	// A tenant sets providerID to a host claimed by another machine.
	machine := testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool)
	machine.Spec.ProviderID = providerID(poolTestHostNamespace, poolTestHost)
	c := newPoolTestClient(t, append(poolTestNamespaces(), testPool(poolTestPool, poolTestTenant), held, other, machine)...)
	r := newPoolTestReconciler(c)

	got, _, err := r.findAndClaimOrGetAssociatedHost(ctx, r.Log, machine, nil)
	if !errors.Is(err, errPoolHostMissing) || got != nil {
		t.Fatalf("host=%v err=%v; a providerID must not adopt a host whose claim names another machine", got, err)
	}
}

func mustGetHost(t *testing.T, c client.Client, name string) *infrav1.PhysicalHost {
	t.Helper()
	host := &infrav1.PhysicalHost{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: poolTestHostNamespace, Name: name}, host); err != nil {
		t.Fatal(err)
	}
	return host
}
