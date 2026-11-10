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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

// The tests in this file flip the package-wide host pool switch, so none of
// them runs in parallel.

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

func withHostPools(t *testing.T) {
	t.Helper()
	EnableHostPools(true)
	t.Cleanup(func() { EnableHostPools(false) })
}

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
	return &infrav1.PhysicalHost{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: lbls},
		Spec:       infrav1.PhysicalHostSpec{CleaningMode: infrav1.CleaningModeMetadata},
		Status:     infrav1.PhysicalHostStatus{State: infrav1.StateAvailable},
	}
}

func testPoolMachine(name, namespace, pool string) *infrav1.Beskar7Machine {
	return &infrav1.Beskar7Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID("uid-" + namespace + "-" + name)},
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
		wantOK   bool
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.disabled {
				withHostPools(t)
			}
			objs := poolTestNamespaces()
			for _, p := range tt.pools {
				objs = append(objs, p)
			}
			c := newPoolTestClient(t, objs...)
			key, ok, err := resolveConsumer(context.Background(), c, tt.host)
			if err != nil {
				t.Fatalf("resolveConsumer: %v", err)
			}
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && key != tenantKey {
				t.Fatalf("key = %v, want %v", key, tenantKey)
			}
		})
	}
}

func newPoolTestReconciler(c client.Client) *Beskar7MachineReconciler {
	return &Beskar7MachineReconciler{Client: c, Log: ctrl.Log.WithName("host-pool-test")}
}

func TestClaimThroughPool(t *testing.T) {
	withHostPools(t)
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
			if !tt.disabled {
				withHostPools(t)
			}
			machine := testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool)
			objs := append(poolTestNamespaces(), testPoolHost(poolTestHost, poolTestHostNamespace, poolTestPool), machine)
			if tt.pool != nil {
				objs = append(objs, tt.pool)
			}
			r := newPoolTestReconciler(newPoolTestClient(t, objs...))
			got, _, err := r.findAndClaimOrGetAssociatedHost(ctx, r.Log, machine, nil)
			if !errors.Is(err, errPoolNotAvailable) || got != nil {
				t.Fatalf("host=%v err=%v, want errPoolNotAvailable", got, err)
			}
		})
	}
}

func TestClaimThroughPoolSkipsHostsOutsideThePool(t *testing.T) {
	withHostPools(t)
	ctx := context.Background()
	machine := testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool)
	notWiped := testPoolHost("not-wiped", poolTestHostNamespace, poolTestPool)
	notWiped.Spec.CleaningMode = infrav1.CleaningModeDisabled
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
	withHostPools(t)
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
	withHostPools(t)
	ctx := context.Background()
	machine := testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool)
	// Someone with patch rights in team-b points their own host at the tenant's machine.
	forged := claimedThrough(testPoolHost("forged", poolTestOtherNamespace, poolTestPool), poolTestTenantNamespace, poolTestMachine, poolTestPool)
	objs := append(poolTestNamespaces(), testPool(poolTestPool, ""), forged, machine)
	r := newPoolTestReconciler(newPoolTestClient(t, objs...))

	got, _, err := r.findAndClaimOrGetAssociatedHost(ctx, r.Log, machine, nil)
	if err != nil || got != nil {
		t.Fatalf("host=%v err=%v; a host outside the pool's hostNamespaces is never adopted", got, err)
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
			if !tt.disabled {
				withHostPools(t)
			}
			machine := testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool)
			labelled := []string{poolTestPool}
			if tt.name == "pool label removed while claimed" {
				labelled = nil
			}
			host := claimedThrough(testPoolHost(poolTestHost, poolTestHostNamespace, labelled...), poolTestTenantNamespace, poolTestMachine, poolTestPool)
			r := newPoolTestReconciler(newPoolTestClient(t, append(poolTestNamespaces(), tt.pool, host, machine)...))

			got, err := r.findClaimedHostForRelease(ctx, r.Log, machine)
			if err != nil || got == nil || got.Name != poolTestHost {
				t.Fatalf("release lookup: host=%v err=%v; the host would stay claimed by a deleted machine", got, err)
			}
		})
	}
}

func TestAvailablePoolHostWakesWaitingMachinesInOtherNamespaces(t *testing.T) {
	withHostPools(t)
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
	withHostPools(t)
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

// claimedByMachine stamps the machine's UID on the claim, as a real claim does.
func claimedByMachine(host *infrav1.PhysicalHost, m *infrav1.Beskar7Machine) *infrav1.PhysicalHost {
	claimedThrough(host, m.Namespace, m.Name, m.Spec.PoolRef)
	host.Spec.ConsumerRef.UID = m.UID
	return host
}

func TestHeldPoolHostStaysHeldAfterItLeavesThePool(t *testing.T) {
	withHostPools(t)
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
	if err := r.markPoolClaim(ctx, machine, got); err != nil {
		t.Fatal(err)
	}
	cond := conditions.Get(machine, infrav1.PoolClaimValidCondition)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != infrav1.PoolGrantRevokedReason {
		t.Fatalf("PoolClaimValid = %+v, want False/%s", cond, infrav1.PoolGrantRevokedReason)
	}
}

func TestPoolStatusCountsAndRevokedClaims(t *testing.T) {
	withHostPools(t)
	ctx := context.Background()
	pool := testPool(poolTestPool, poolTestTenant)
	pool.Finalizers = []string{infrav1.PhysicalHostPoolFinalizer}
	machine := testPoolMachine(poolTestMachine, poolTestTenantNamespace, poolTestPool)
	free := testPoolHost("free", poolTestHostNamespace, poolTestPool)
	notWiped := testPoolHost("not-wiped", poolTestHostNamespace, poolTestPool)
	notWiped.Spec.CleaningMode = infrav1.CleaningModeDisabled
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
	withHostPools(t)
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

func TestClusterFailureDomainsIncludePoolMembers(t *testing.T) {
	withHostPools(t)
	ctx := context.Background()
	zoned := func(name, zone string, pools ...string) *infrav1.PhysicalHost {
		h := testPoolHost(name, poolTestHostNamespace, pools...)
		h.Labels[zoneLabelKey] = zone
		return h
	}
	cluster := &infrav1.Beskar7Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-a", Namespace: poolTestTenantNamespace},
		Spec:       infrav1.Beskar7ClusterSpec{PoolRefs: []string{poolTestPool, poolTestGPUPool}},
	}
	objs := append(poolTestNamespaces(),
		testPool(poolTestPool, poolTestTenant),
		testPool(poolTestGPUPool, "someone-else"),
		zoned("a1", "zone-a", poolTestPool),
		zoned("b1", "zone-b", poolTestPool),
		zoned("gpu", "zone-gpu", poolTestGPUPool),
		zoned("unpooled", "zone-x"),
		cluster,
	)
	r := &Beskar7ClusterReconciler{Client: newPoolTestClient(t, objs...), Log: ctrl.Log.WithName("pool-fd-test")}
	if err := r.reconcileFailureDomains(ctx, r.Log, cluster); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, fd := range cluster.Status.FailureDomains {
		got = append(got, fd.Name)
	}
	if strings.Join(got, ",") != "zone-a,zone-b" {
		t.Fatalf("failure domains = %v, want zone-a,zone-b (granted pool only, no unpooled hosts)", got)
	}
}
