package controller

import (
	"context"
	"fmt"

	pgmcv1alpha1 "github.com/aurelops/postgres-mc-operator/api/v1alpha1"
	"github.com/aurelops/postgres-mc-operator/internal/credentials"
	"github.com/aurelops/postgres-mc-operator/internal/zalando"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// reconcilePrimary applies the Zalando postgresql resource for the primary cluster.
// Returns (credentialsReady bool, err error). When credentialsReady=false, the
// caller should requeue after a short interval to wait for Zalando to create secrets.
func (r *PostgresMCReconciler) reconcilePrimary(ctx context.Context, mc *pgmcv1alpha1.PostgresMC, primary resolvedCluster) (bool, error) {
	obj, err := r.ZalandoBuilder.BuildPrimary(mc, primary.Target)
	if err != nil {
		return false, fmt.Errorf("building primary spec: %w", err)
	}

	if err := primary.Client.Patch(ctx, obj, client.Apply, client.ForceOwnership, client.FieldOwner("postgres-mc-operator")); err != nil {
		return false, fmt.Errorf("applying primary postgresql: %w", err)
	}

	clusterName := zalando.ClusterName(mc.Spec.TeamID, primary.Target.Name)

	// Check whether Zalando has created the credential secrets yet.
	for _, role := range credentials.CredentialRoles {
		secretName := credentials.SecretName(role, clusterName)
		var s corev1.Secret
		if err := primary.Client.Get(ctx, types.NamespacedName{
			Namespace: primary.Target.Namespace,
			Name:      secretName,
		}, &s); err != nil {
			if errors.IsNotFound(err) {
				return false, nil // not yet ready, caller will requeue
			}
			return false, fmt.Errorf("checking credential secret %s: %w", secretName, err)
		}
	}
	return true, nil
}
