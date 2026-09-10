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
