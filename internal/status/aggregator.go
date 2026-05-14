package status

import (
	pgmcv1alpha1 "github.com/aurelops/postgres-mc-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Aggregate computes the rolled-up phase and top-level conditions from per-cluster statuses.
func Aggregate(clusterStatuses []pgmcv1alpha1.ClusterStatus, failoverInProgress bool) (phase string, conditions []metav1.Condition) {
	if failoverInProgress {
		return PhaseFailingOver, nil
	}
	if len(clusterStatuses) == 0 {
		return PhasePending, nil
	}

	total := len(clusterStatuses)
	ready := 0
	for _, cs := range clusterStatuses {
		if conditionTrue(cs.Conditions, pgmcv1alpha1.ConditionReady) {
			ready++
		}
	}

	switch {
	case ready == total:
		phase = PhaseReady
	case ready == 0:
		phase = PhaseFailed
	default:
		phase = PhaseDegraded
	}
	return phase, nil
}

func conditionTrue(conditions []metav1.Condition, condType string) bool {
	for _, c := range conditions {
		if c.Type == condType {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}
