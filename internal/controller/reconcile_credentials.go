package controller

import (
	"context"
	"fmt"

	pgmcv1alpha1 "github.com/aurelops/postgres-mc-operator/api/v1alpha1"
	"github.com/aurelops/postgres-mc-operator/internal/credentials"
	"github.com/aurelops/postgres-mc-operator/internal/zalando"
)

// reconcileCredentials copies primary credential secrets to each standby cluster.
func (r *PostgresMCReconciler) reconcileCredentials(ctx context.Context, mc *pgmcv1alpha1.PostgresMC, primary resolvedCluster, standbys []resolvedCluster) error {
	primaryClusterName := zalando.ClusterName(mc.Spec.TeamID, primary.Target.Name)

	var targets []credentials.StandbyTarget
	for _, s := range standbys {
		targets = append(targets, credentials.StandbyTarget{
			ClusterName: zalando.ClusterName(mc.Spec.TeamID, s.Target.Name),
			Client:      s.Client,
			Namespace:   s.Target.Namespace,
		})
	}

	result, err := r.CredSyncer.SyncFromPrimary(ctx, credentials.SyncPlan{
		PostgresMCName: mc.Name,
		ClusterName:    primaryClusterName,
		PrimaryClient:  primary.Client,
		PrimaryNS:      primary.Target.Namespace,
		Standbys:       targets,
	})
	if err != nil {
		return fmt.Errorf("credential sync: %w", err)
	}

	if result.HasErrors() {
		for clusterName, syncErr := range result.PerCluster {
			if syncErr != nil {
				r.Recorder.Eventf(mc, "Warning", pgmcv1alpha1.ReasonCredsNotReady,
					"credential sync to %s failed: %s", clusterName, syncErr)
			}
		}
		return fmt.Errorf("credential sync had errors (see events)")
	}
	return nil
}
