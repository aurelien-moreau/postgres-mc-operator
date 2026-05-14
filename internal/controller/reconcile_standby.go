package controller

import (
	"context"
	"fmt"

	pgmcv1alpha1 "github.com/aurelops/postgres-mc-operator/api/v1alpha1"
	"github.com/aurelops/postgres-mc-operator/internal/crosscluster"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// reconcileStandby creates the cross-cluster alias Service and applies the standby
// Zalando postgresql resource. primaryEndpoint is the discovered (host, port) of the
// primary's replication service — set by the operator, not hard-coded in the spec.
func (r *PostgresMCReconciler) reconcileStandby(ctx context.Context, mc *pgmcv1alpha1.PostgresMC, primaryEndpoint clusterEndpoint, standby resolvedCluster) error {
	if primaryEndpoint.Host == "" {
		return fmt.Errorf("primary endpoint not yet discovered; cannot configure standby %s", standby.Target.Name)
	}
	if primaryEndpoint.Port == 0 {
		primaryEndpoint.Port = 5432
	}

	// Ensure the cross-cluster alias Service exists on the standby cluster.
	// It points to the primary's external host so standby pods can reach it via a stable DNS name.
	if err := r.Services.EnsurePrimaryAlias(ctx, crosscluster.AliasPlan{
		PostgresMCName: mc.Name,
		StandbyClient:  standby.Client,
		StandbyNS:      standby.Target.Namespace,
		PrimaryHost:    primaryEndpoint.Host,
		PrimaryPort:    primaryEndpoint.Port,
	}); err != nil {
		return fmt.Errorf("ensuring primary alias on standby %s: %w", standby.Target.Name, err)
	}

	// The standby Zalando postgresql uses the local alias FQDN — no cross-cluster IP in the spec.
	aliasFQDN := crosscluster.PrimaryAliasFQDN(mc.Name, standby.Target.Namespace)

	obj, err := r.ZalandoBuilder.BuildStandby(mc, standby.Target, aliasFQDN, primaryEndpoint.Port)
	if err != nil {
		return fmt.Errorf("building standby spec for %s: %w", standby.Target.Name, err)
	}

	if err := standby.Client.Patch(ctx, obj, client.Apply, client.ForceOwnership, client.FieldOwner("postgres-mc-operator")); err != nil {
		return fmt.Errorf("applying standby postgresql on %s: %w", standby.Target.Name, err)
	}
	return nil
}

// reconcileStandbyTarget is used during failover to reconcile a cluster that is becoming standby,
// using the new primary's discovered endpoint.
func (r *PostgresMCReconciler) reconcileStandbyTarget(ctx context.Context, mc *pgmcv1alpha1.PostgresMC, primaryEndpoint clusterEndpoint, standby pgmcv1alpha1.ClusterTarget, standbyClient client.Client) error {
	return r.reconcileStandby(ctx, mc, primaryEndpoint, resolvedCluster{Target: standby, Client: standbyClient})
}
