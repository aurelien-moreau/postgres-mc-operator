package controller

import (
	"context"
	"fmt"

	pgmcv1alpha1 "github.com/aurelops/postgres-mc-operator/api/v1alpha1"
	"github.com/aurelops/postgres-mc-operator/internal/zalando"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *PostgresMCReconciler) handleDeletion(ctx context.Context, mc *pgmcv1alpha1.PostgresMC) (ctrl.Result, error) {
	if !containsFinalizer(mc, finalizerName) {
		return ctrl.Result{}, nil
	}

	for _, target := range mc.Spec.Clusters {
		c, err := r.Clusters.GetClient(ctx, target, mc.Namespace)
		if err != nil {
			// Log but don't block deletion — cluster may be gone.
			continue
		}
		clusterName := zalando.ClusterName(mc.Spec.TeamID, target.Name)

		// Delete Zalando postgresql resource.
		pg := &unstructured.Unstructured{}
		pg.SetAPIVersion("acid.zalan.do/v1")
		pg.SetKind("postgresql")
		pg.SetName(clusterName)
		pg.SetNamespace(target.Namespace)
		if err := c.Delete(ctx, pg); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("deleting postgresql %s/%s on cluster %s: %w", target.Namespace, clusterName, target.Name, err)
		}

		// Delete replication service (all clusters).
		if err := r.ReplicationServices.Delete(ctx, c, mc, target); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting replication service on cluster %s: %w", target.Name, err)
		}

		// Delete cross-cluster alias Service (standbys only).
		if target.Role == pgmcv1alpha1.ClusterRoleStandby {
			if err := r.Services.DeleteAlias(ctx, c, target.Namespace, mc.Name); err != nil {
				return ctrl.Result{}, fmt.Errorf("deleting alias on cluster %s: %w", target.Name, err)
			}
		}
	}

	// Remove finalizer.
	orig := mc.DeepCopy()
	mc.Finalizers = removeFinalizer(mc.Finalizers, finalizerName)
	if err := r.Patch(ctx, mc, client.MergeFrom(orig)); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func removeFinalizer(finalizers []string, name string) []string {
	out := finalizers[:0]
	for _, f := range finalizers {
		if f != name {
			out = append(out, f)
		}
	}
	return out
}

