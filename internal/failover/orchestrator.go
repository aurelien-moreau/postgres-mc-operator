// Package failover orchestrates the promotion of a standby cluster to primary.
package failover

import (
	"context"
	"encoding/json"
	"fmt"

	pgmcv1alpha1 "github.com/aurelops/postgres-mc-operator/api/v1alpha1"
	"github.com/aurelops/postgres-mc-operator/internal/crosscluster"
	"github.com/aurelops/postgres-mc-operator/internal/zalando"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Orchestrator executes a controlled failover from one primary to another.
type Orchestrator interface {
	Run(ctx context.Context, plan Plan) (Result, error)
}

// ResolvedCluster bundles a target spec with its live client and resolved endpoint.
// Host and Port are the network address other clusters use to reach this cluster's
// PostgreSQL replication service (ExternalHost override or discovered node IP/LB).
type ResolvedCluster struct {
	Target pgmcv1alpha1.ClusterTarget
	Client client.Client
	Host   string
	Port   int32
}

// Plan describes the failover to execute.
type Plan struct {
	PostgresMC    *pgmcv1alpha1.PostgresMC
	OldPrimary    ResolvedCluster
	NewPrimary    ResolvedCluster
	OtherStandbys []ResolvedCluster
}

// Result summarises the failover outcome.
type Result struct {
	NewPrimaryName string
}

type orchestrator struct {
	builder    zalando.Builder
	serviceMgr crosscluster.ServiceManager
}

func NewOrchestrator(builder zalando.Builder, serviceMgr crosscluster.ServiceManager) Orchestrator {
	return &orchestrator{builder: builder, serviceMgr: serviceMgr}
}

func (o *orchestrator) Run(ctx context.Context, plan Plan) (Result, error) {
	mc := plan.PostgresMC
	newPrimaryHost := plan.NewPrimary.Host
	if newPrimaryHost == "" {
		return Result{}, fmt.Errorf("new primary cluster %q has no resolvable host; set spec.clusters[].externalHost or wait for replication service endpoint discovery", plan.NewPrimary.Target.Name)
	}
	newPrimaryPort := plan.NewPrimary.Port
	if newPrimaryPort == 0 {
		newPrimaryPort = 5432
	}

	// Step 1: Demote old primary — add standby spec pointing to new primary.
	if err := o.demoteCluster(ctx, mc, plan.OldPrimary, newPrimaryHost, newPrimaryPort); err != nil {
		return Result{}, fmt.Errorf("demoting old primary %s: %w", plan.OldPrimary.Target.Name, err)
	}

	// Step 2: Promote new primary — remove standby spec.
	if err := o.promoteCluster(ctx, mc, plan.NewPrimary); err != nil {
		return Result{}, fmt.Errorf("promoting new primary %s: %w", plan.NewPrimary.Target.Name, err)
	}

	// Step 3: Repoint remaining standbys to the new primary alias FQDN.
	newPrimaryAliasFQDN := crosscluster.PrimaryAliasFQDN(mc.Name, plan.NewPrimary.Target.Namespace)
	for _, standby := range plan.OtherStandbys {
		if err := o.repoint(ctx, mc, standby, newPrimaryAliasFQDN, newPrimaryPort); err != nil {
			return Result{}, fmt.Errorf("repointing standby %s: %w", standby.Target.Name, err)
		}
	}

	// Step 4: Update the primary alias Service on all standby clusters to point to new primary.
	allStandbys := append(plan.OtherStandbys, plan.OldPrimary)
	for _, standby := range allStandbys {
		aliasPlan := crosscluster.AliasPlan{
			PostgresMCName: mc.Name,
			StandbyClient:  standby.Client,
			StandbyNS:      standby.Target.Namespace,
			PrimaryHost:    newPrimaryHost,
			PrimaryPort:    newPrimaryPort,
		}
		if err := o.serviceMgr.EnsurePrimaryAlias(ctx, aliasPlan); err != nil {
			return Result{}, fmt.Errorf("updating alias on %s: %w", standby.Target.Name, err)
		}
	}

	return Result{NewPrimaryName: plan.NewPrimary.Target.Name}, nil
}

func (o *orchestrator) demoteCluster(ctx context.Context, mc *pgmcv1alpha1.PostgresMC, target ResolvedCluster, newPrimaryHost string, newPrimaryPort int32) error {
	obj, err := o.builder.BuildStandby(mc, target.Target, newPrimaryHost, newPrimaryPort)
	if err != nil {
		return err
	}
	return applyUnstructured(ctx, target.Client, obj)
}

func (o *orchestrator) promoteCluster(ctx context.Context, mc *pgmcv1alpha1.PostgresMC, target ResolvedCluster) error {
	obj, err := o.builder.BuildPrimary(mc, target.Target)
	if err != nil {
		return err
	}

	// Use a JSON merge patch to explicitly null out spec.standby so it is removed.
	existing := &unstructured.Unstructured{}
	existing.SetAPIVersion("acid.zalan.do/v1")
	existing.SetKind("postgresql")
	if err := target.Client.Get(ctx, types.NamespacedName{
		Namespace: target.Target.Namespace,
		Name:      zalando.ClusterName(mc.Spec.TeamID, target.Target.Name),
	}, existing); err != nil {
		// If not found, just create via SSA.
		return applyUnstructured(ctx, target.Client, obj)
	}

	// Remove the standby key via merge patch.
	patch, _ := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{
			"standby": nil,
		},
	})
	return target.Client.Patch(ctx, existing, client.RawPatch(types.MergePatchType, patch))
}

func (o *orchestrator) repoint(ctx context.Context, mc *pgmcv1alpha1.PostgresMC, standby ResolvedCluster, newPrimaryAlias string, newPrimaryPort int32) error {
	obj, err := o.builder.BuildStandby(mc, standby.Target, newPrimaryAlias, newPrimaryPort)
	if err != nil {
		return err
	}
	return applyUnstructured(ctx, standby.Client, obj)
}

func applyUnstructured(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
	return c.Patch(ctx, obj, client.Apply, client.ForceOwnership, client.FieldOwner("postgres-mc-operator"))
}

