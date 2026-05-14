// Package status provides helpers for managing status conditions on PostgresMC.
package status

import (
	"time"

	pgmcv1alpha1 "github.com/aurelops/postgres-mc-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	PhasePending      = "Pending"
	PhaseProvisioning = "Provisioning"
	PhaseReady        = "Ready"
	PhaseDegraded     = "Degraded"
	PhaseFailingOver  = "FailingOver"
	PhaseFailed       = "Failed"
)

// SetCondition upserts a condition into a slice, updating LastTransitionTime only when Status changes.
func SetCondition(conditions *[]metav1.Condition, condType string, condStatus metav1.ConditionStatus, generation int64, reason, message string) {
	now := metav1.NewTime(time.Now())
	for i, c := range *conditions {
		if c.Type == condType {
			changed := c.Status != condStatus
			(*conditions)[i].Status = condStatus
			(*conditions)[i].Reason = reason
			(*conditions)[i].Message = message
			(*conditions)[i].ObservedGeneration = generation
			if changed {
				(*conditions)[i].LastTransitionTime = now
			}
			return
		}
	}
	*conditions = append(*conditions, metav1.Condition{
		Type:               condType,
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
		LastTransitionTime: now,
	})
}

// SetClusterCondition upserts a condition for a specific cluster in the status.
func SetClusterCondition(clusterStatuses []pgmcv1alpha1.ClusterStatus, clusterName string, condType string, condStatus metav1.ConditionStatus, generation int64, reason, message string) []pgmcv1alpha1.ClusterStatus {
	for i, cs := range clusterStatuses {
		if cs.Name == clusterName {
			SetCondition(&clusterStatuses[i].Conditions, condType, condStatus, generation, reason, message)
			return clusterStatuses
		}
	}
	// New cluster entry.
	cs := pgmcv1alpha1.ClusterStatus{Name: clusterName}
	SetCondition(&cs.Conditions, condType, condStatus, generation, reason, message)
	return append(clusterStatuses, cs)
}
