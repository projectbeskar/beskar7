package controllers

import (
	"fmt"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
)

// setTrue records conditionType=True with reason on obj. metav1.Condition
// requires a reason on every state, which is why the True side names one too.
func setTrue(obj conditions.Setter, conditionType, reason string) {
	conditions.Set(obj, metav1.Condition{
		Type:   conditionType,
		Status: metav1.ConditionTrue,
		Reason: reason,
	})
}

// setFalse records conditionType=False with reason and a formatted message.
func setFalse(obj conditions.Setter, conditionType, reason, format string, args ...any) {
	conditions.Set(obj, metav1.Condition{
		Type:    conditionType,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: fmt.Sprintf(format, args...),
	})
}

// setReadySummary rolls the resource's own conditions up into Ready. It is a
// no-op until at least one of them exists, because summarising an empty list
// is an error rather than an Unknown condition.
func setReadySummary(obj conditions.Setter, log logr.Logger, conditionTypes ...string) {
	present := false
	for _, t := range conditionTypes {
		if conditions.Has(obj, t) {
			present = true
			break
		}
	}
	if !present {
		return
	}
	if err := conditions.SetSummaryCondition(obj, obj, clusterv1.ReadyCondition,
		conditions.ForConditionTypes(conditionTypes), conditions.IgnoreTypesIfMissing(conditionTypes)); err != nil {
		log.Error(err, "Failed to set the Ready summary condition")
	}
}

// LegacyConditionReason is stamped on a condition that a pre-v0.6.0 controller
// stored without one. It is a placeholder: the reconcile that repairs the
// condition almost always overwrites it with the real reason on the same pass.
const LegacyConditionReason = "Migrated"

// repairLegacyConditions makes conditions written by a pre-v0.6.0 controller
// valid under the metav1.Condition schema, and reports whether it changed
// anything.
//
// Releases up to v0.5.0 used Cluster API's deprecated clusterv1.Conditions,
// where `reason` is optional, and they stored True conditions with no reason at
// all. v0.6.0's CRDs generate `reason` as required with minLength 1, so the
// API server rejects every status patch that carries those conditions forward:
//
//	status.conditions[0].reason: Invalid value: "":
//	conditions[0].reason in body should be at least 1 chars long
//
// The object then freezes with whatever status the old controller last wrote —
// and it still reads healthy under `kubectl get`, because nothing rewrites it.
// Measured on the libvirt + sushy-tools lab upgrading v0.5.0 to v0.6.0: every Beskar7Machine,
// Beskar7Cluster and PhysicalHost in the namespace, ~140 reconcile errors in
// four minutes, until the stale conditions were cleared by hand.
//
// `lastTransitionTime` is required as well, and is filled in where a legacy
// entry lacks one. `message` needs nothing: it is required but carries no
// minLength, and metav1.Condition serialises it without omitempty, so the
// empty string satisfies the schema. Every reason v0.5.0 could write is
// CamelCase and matches the generated pattern, so an absent reason is the only
// way a legacy condition fails validation. A condition that is already valid is
// left exactly as it is, including its transition time — this only ever repairs
// what would otherwise be rejected.
//
// Call this AFTER patch.NewHelper: the helper diffs against the object as it
// was when the helper was made, so a repair applied before it would not be
// recognised as a change and would never reach the API server.
func repairLegacyConditions(obj conditions.Setter, log logr.Logger) bool {
	existing := obj.GetConditions()
	repaired := false
	for i := range existing {
		c := &existing[i]
		if c.Reason == "" {
			c.Reason = LegacyConditionReason
			repaired = true
		}
		if c.LastTransitionTime.IsZero() {
			c.LastTransitionTime = metav1.Now()
			repaired = true
		}
	}
	if repaired {
		obj.SetConditions(existing)
		log.Info("Repaired conditions written by a pre-v0.6.0 controller so the status can be patched again")
	}
	return repaired
}
