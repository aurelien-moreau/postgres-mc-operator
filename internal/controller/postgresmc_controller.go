// Package controller implements the PostgresMC reconciler.
package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	pgmcv1alpha1 "github.com/aurelops/postgres-mc-operator/api/v1alpha1"
	"github.com/aurelops/postgres-mc-operator/internal/clusters"
	"github.com/aurelops/postgres-mc-operator/internal/credentials"
	"github.com/aurelops/postgres-mc-operator/internal/crosscluster"
	"github.com/aurelops/postgres-mc-operator/internal/failover"
	"github.com/aurelops/postgres-mc-operator/internal/replicationservice"
	internstatus "github.com/aurelops/postgres-mc-operator/internal/status"
	"github.com/aurelops/postgres-mc-operator/internal/zalando"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	finalizerName   = "pgmc.aurelops.io/finalizer"
	requeueDefault  = 30 * time.Second
	requeueProgress = 10 * time.Second
)

// PostgresMCReconciler reconciles PostgresMC resources.
//
// +kubebuilder:rbac:groups=pgmc.aurelops.io,resources=postgresmcs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pgmc.aurelops.io,resources=postgresmcs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pgmc.aurelops.io,resources=postgresmcs/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
type PostgresMCReconciler struct {
	client.Client
	Scheme              *runtime.Scheme
	Recorder            record.EventRecorder
	Clusters            clusters.Registry
	CredSyncer          credentials.Syncer
	Services            crosscluster.ServiceManager
	Failover            failover.Orchestrator
	ZalandoBuilder      zalando.Builder
	ReplicationServices replicationservice.Manager
}

func (r *PostgresMCReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var mc pgmcv1alpha1.PostgresMC
	if err := r.Get(ctx, req.NamespacedName, &mc); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion.
	if !mc.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &mc)
	}

	// Ensure finalizer is set.
	if !containsFinalizer(&mc, finalizerName) {
		mc.Finalizers = append(mc.Finalizers, finalizerName)
		if err := r.Update(ctx, &mc); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Validate spec.
	if err := validateSpec(&mc); err != nil {
		logger.Error(err, "invalid spec")
		r.Recorder.Eventf(&mc, "Warning", pgmcv1alpha1.ReasonSpecInvalid, "%s", err)
		orig := mc.DeepCopy()
		internstatus.SetCondition(&mc.Status.Conditions, pgmcv1alpha1.ConditionReady,
			metav1.ConditionFalse, mc.Generation, pgmcv1alpha1.ReasonSpecInvalid, err.Error())
		mc.Status.Phase = internstatus.PhaseFailed
		_ = r.Status().Patch(ctx, &mc, client.MergeFrom(orig))
		return ctrl.Result{}, nil // permanent error
	}

	return r.reconcile(ctx, &mc)
}

func (r *PostgresMCReconciler) reconcile(ctx context.Context, mc *pgmcv1alpha1.PostgresMC) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	orig := mc.DeepCopy()

	// Ensure every ClusterStatus entry has a valid role before any status patch.
	// Without this, newly created entries have role="" which fails CRD enum validation.
	ensureClusterStatusRoles(mc)

	// Detect manual failover: user changed role in spec.
	specPrimary := primaryClusterName(mc)
	if mc.Status.CurrentPrimary != "" && mc.Status.CurrentPrimary != specPrimary {
		return r.runFailover(ctx, mc, orig)
	}

	var clusterClients []resolvedCluster
	var unreachable []string
	primaryUnreachable := false

	for _, target := range mc.Spec.Clusters {
		c, err := r.Clusters.GetClient(ctx, target, mc.Namespace)
		if err != nil {
			logger.Error(err, "cluster unreachable", "cluster", target.Name)
			r.Recorder.Eventf(mc, "Warning", pgmcv1alpha1.ReasonClusterUnreachable, "cluster %s: %s", target.Name, err)
			unreachable = append(unreachable, target.Name)
			mc.Status.Clusters = internstatus.SetClusterCondition(mc.Status.Clusters, target.Name,
				pgmcv1alpha1.ConditionClusterReachable, metav1.ConditionFalse,
				mc.Generation, pgmcv1alpha1.ReasonClusterUnreachable, err.Error())
			setClusterReady(mc, target.Name, metav1.ConditionFalse, pgmcv1alpha1.ReasonClusterUnreachable, err.Error())
			if target.Role == pgmcv1alpha1.ClusterRolePrimary {
				primaryUnreachable = true
			}
			continue
		}
		mc.Status.Clusters = internstatus.SetClusterCondition(mc.Status.Clusters, target.Name,
			pgmcv1alpha1.ConditionClusterReachable, metav1.ConditionTrue,
			mc.Generation, pgmcv1alpha1.ReasonReconcileSuccess, "")
		clusterClients = append(clusterClients, resolvedCluster{Target: target, Client: c})
	}

	// Auto-failover: primary unreachable for N consecutive reconciles → promote best standby.
	if primaryUnreachable {
		mc.Status.PrimaryFailureCount++
		threshold := mc.Spec.AutoFailover.PrimaryUnreachableThreshold
		if threshold == 0 {
			threshold = 3
		}
		logger.Info("primary unreachable",
			"failureCount", mc.Status.PrimaryFailureCount,
			"threshold", threshold,
			"autoFailoverEnabled", mc.Spec.AutoFailover.Enabled)

		if mc.Spec.AutoFailover.Enabled && mc.Status.PrimaryFailureCount >= threshold {
			return r.runAutoFailover(ctx, mc, orig, clusterClients)
		}
	} else {
		// Primary reachable — reset the failure counter.
		mc.Status.PrimaryFailureCount = 0
	}

	primary, standbys := splitByRole(clusterClients)
	requeue := requeueDefault

	// Reconcile primary cluster.
	// Ensure replication service exists on every reachable cluster and discover its endpoint.
	// This runs for both primary and standbys so any cluster is ready to become primary.
	endpointByCluster := make(map[string]clusterEndpoint)
	for _, rc := range clusterClients {
		host, port, err := r.ReplicationServices.Ensure(ctx, rc.Client, mc, rc.Target)
		if err != nil {
			logger.Error(err, "ensuring replication service", "cluster", rc.Target.Name)
			requeue = requeueProgress
			// Fall back to the last known endpoint so a transient failure (e.g. node
			// listing permission not yet applied) doesn't break standby reconciliation.
			if prev := clusterEndpointFromStatus(mc, rc.Target.Name); prev.Host != "" {
				endpointByCluster[rc.Target.Name] = prev
			}
		} else {
			endpointByCluster[rc.Target.Name] = clusterEndpoint{Host: host, Port: port}
			updateClusterEndpoint(mc, rc.Target.Name, fmt.Sprintf("%s:%d", host, port))
		}
	}

	primaryReady := false
	var primaryEndpoint clusterEndpoint
	if primary != nil {
		primaryEndpoint = endpointByCluster[primary.Target.Name]
		credReady, err := r.reconcilePrimary(ctx, mc, *primary)
		if err != nil {
			logger.Error(err, "reconciling primary", "cluster", primary.Target.Name)
			setClusterReady(mc, primary.Target.Name, metav1.ConditionFalse, pgmcv1alpha1.ReasonZalandoApplyFailed, err.Error())
		} else if !credReady {
			// Zalando hasn't created credential secrets yet — requeue, don't mark ready.
			requeue = requeueProgress
			setClusterReady(mc, primary.Target.Name, metav1.ConditionFalse, pgmcv1alpha1.ReasonCredsNotReady, "waiting for credential secrets")
		} else {
			primaryReady = true
			setClusterReady(mc, primary.Target.Name, metav1.ConditionTrue, pgmcv1alpha1.ReasonReconcileSuccess, "")
		}
	}

	// Sync credentials and reconcile standbys only when primary credentials are ready.
	if primary != nil && primaryReady && len(standbys) > 0 {
		if err := r.reconcileCredentials(ctx, mc, *primary, standbys); err != nil {
			logger.Error(err, "syncing credentials")
			requeue = requeueProgress
		}
		for _, standby := range standbys {
			if err := r.reconcileStandby(ctx, mc, primaryEndpoint, standby); err != nil {
				logger.Error(err, "reconciling standby", "cluster", standby.Target.Name)
				setClusterReady(mc, standby.Target.Name, metav1.ConditionFalse, pgmcv1alpha1.ReasonZalandoApplyFailed, err.Error())
			} else {
				setClusterReady(mc, standby.Target.Name, metav1.ConditionTrue, pgmcv1alpha1.ReasonReconcileSuccess, "")
			}
		}
	}

	mc.Status.CurrentPrimary = specPrimary
	mc.Status.ObservedGeneration = mc.Generation
	phase, _ := internstatus.Aggregate(mc.Status.Clusters, false)
	mc.Status.Phase = phase
	readyStatus := metav1.ConditionFalse
	readyReason := pgmcv1alpha1.ReasonPrimaryNotReady
	if phase == internstatus.PhaseReady {
		readyStatus = metav1.ConditionTrue
		readyReason = pgmcv1alpha1.ReasonReconcileSuccess
	}
	if len(unreachable) > 0 {
		mc.Status.Phase = internstatus.PhaseDegraded
		readyStatus = metav1.ConditionFalse
		readyReason = pgmcv1alpha1.ReasonClusterUnreachable
	}
	internstatus.SetCondition(&mc.Status.Conditions, pgmcv1alpha1.ConditionReady,
		readyStatus, mc.Generation, readyReason, "")

	if err := r.Status().Patch(ctx, mc, client.MergeFrom(orig)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching status: %w", err)
	}

	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *PostgresMCReconciler) runFailover(ctx context.Context, mc *pgmcv1alpha1.PostgresMC, orig *pgmcv1alpha1.PostgresMC) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("failover triggered", "from", mc.Status.CurrentPrimary, "to", primaryClusterName(mc))
	r.Recorder.Eventf(mc, "Normal", pgmcv1alpha1.ReasonFailoverStarted, "failover from %s to %s", mc.Status.CurrentPrimary, primaryClusterName(mc))

	mc.Status.Phase = internstatus.PhaseFailingOver
	internstatus.SetCondition(&mc.Status.Conditions, pgmcv1alpha1.ConditionFailoverInProgress,
		metav1.ConditionTrue, mc.Generation, pgmcv1alpha1.ReasonFailoverStarted, "")
	_ = r.Status().Patch(ctx, mc, client.MergeFrom(orig))

	// Build resolved cluster map.
	var oldPrimary, newPrimary failover.ResolvedCluster
	var others []failover.ResolvedCluster
	for _, target := range mc.Spec.Clusters {
		c, err := r.Clusters.GetClient(ctx, target, mc.Namespace)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("getting client for cluster %s during failover: %w", target.Name, err)
		}
		// Resolve endpoint: prefer spec ExternalHost, fall back to what the
		// replication-service manager discovered and stored in status.
		endpoint := clusterEndpointFromStatus(mc, target.Name)
		host := target.ExternalHost
		if host == "" {
			host = endpoint.Host
		}
		port := endpoint.Port
		if port == 0 {
			port = target.ExternalPort
		}
		if port == 0 {
			port = 5432
		}
		rc := failover.ResolvedCluster{Target: target, Client: c, Host: host, Port: port}
		switch {
		case target.Name == mc.Status.CurrentPrimary:
			oldPrimary = rc
		case target.Role == pgmcv1alpha1.ClusterRolePrimary:
			newPrimary = rc
		default:
			others = append(others, rc)
		}
	}

	result, err := r.Failover.Run(ctx, failover.Plan{
		PostgresMC:    mc,
		OldPrimary:    oldPrimary,
		NewPrimary:    newPrimary,
		OtherStandbys: others,
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failover: %w", err)
	}

	orig2 := mc.DeepCopy()
	mc.Status.CurrentPrimary = result.NewPrimaryName
	mc.Status.Phase = internstatus.PhaseReady
	internstatus.SetCondition(&mc.Status.Conditions, pgmcv1alpha1.ConditionFailoverInProgress,
		metav1.ConditionFalse, mc.Generation, pgmcv1alpha1.ReasonFailoverCompleted, "")
	internstatus.SetCondition(&mc.Status.Conditions, pgmcv1alpha1.ConditionReady,
		metav1.ConditionTrue, mc.Generation, pgmcv1alpha1.ReasonReconcileSuccess, "")
	if err := r.Status().Patch(ctx, mc, client.MergeFrom(orig2)); err != nil {
		return ctrl.Result{}, err
	}

	r.Recorder.Eventf(mc, "Normal", pgmcv1alpha1.ReasonFailoverCompleted, "failover completed, new primary: %s", result.NewPrimaryName)
	return ctrl.Result{RequeueAfter: requeueDefault}, nil
}

// runAutoFailover picks the first reachable standby and promotes it.
// Called only when spec.autoFailover.enabled=true and the primary has been
// unreachable for at least spec.autoFailover.primaryUnreachableThreshold cycles.
func (r *PostgresMCReconciler) runAutoFailover(ctx context.Context, mc *pgmcv1alpha1.PostgresMC, orig *pgmcv1alpha1.PostgresMC, reachable []resolvedCluster) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Pick the first reachable standby as the new primary.
	var newPrimary *resolvedCluster
	for i, rc := range reachable {
		if rc.Target.Role == pgmcv1alpha1.ClusterRoleStandby {
			newPrimary = &reachable[i]
			break
		}
	}
	if newPrimary == nil {
		logger.Info("auto-failover: no reachable standby found, waiting")
		_ = r.Status().Patch(ctx, mc, client.MergeFrom(orig))
		return ctrl.Result{RequeueAfter: requeueProgress}, nil
	}

	logger.Info("auto-failover triggered",
		"newPrimary", newPrimary.Target.Name,
		"failureCount", mc.Status.PrimaryFailureCount)
	r.Recorder.Eventf(mc, "Warning", pgmcv1alpha1.ReasonAutoFailover,
		"primary unreachable for %d cycles, promoting %s",
		mc.Status.PrimaryFailureCount, newPrimary.Target.Name)

	// Rewrite spec roles so the existing runFailover path handles promotion.
	// We patch spec directly so the change is durable and visible to users.
	specPatch := mc.DeepCopy()
	for i, c := range specPatch.Spec.Clusters {
		if c.Name == newPrimary.Target.Name {
			specPatch.Spec.Clusters[i].Role = pgmcv1alpha1.ClusterRolePrimary
		} else {
			specPatch.Spec.Clusters[i].Role = pgmcv1alpha1.ClusterRoleStandby
		}
	}
	if err := r.Patch(ctx, specPatch, client.MergeFrom(mc)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching spec for auto-failover: %w", err)
	}

	// The spec patch triggers a new reconcile which will detect the role change
	// and call runFailover. Reset the failure counter now.
	mc.Status.PrimaryFailureCount = 0
	_ = r.Status().Patch(ctx, mc, client.MergeFrom(orig))

	return ctrl.Result{Requeue: true}, nil
}

func (r *PostgresMCReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&pgmcv1alpha1.PostgresMC{}).
		Complete(r)
}

// --- helpers ---

type resolvedCluster struct {
	Target pgmcv1alpha1.ClusterTarget
	Client client.Client
}

func splitByRole(cls []resolvedCluster) (primary *resolvedCluster, standbys []resolvedCluster) {
	for i, rc := range cls {
		if rc.Target.Role == pgmcv1alpha1.ClusterRolePrimary {
			primary = &cls[i]
		} else {
			standbys = append(standbys, rc)
		}
	}
	return
}

func primaryClusterName(mc *pgmcv1alpha1.PostgresMC) string {
	for _, c := range mc.Spec.Clusters {
		if c.Role == pgmcv1alpha1.ClusterRolePrimary {
			return c.Name
		}
	}
	return ""
}

func containsFinalizer(mc *pgmcv1alpha1.PostgresMC, name string) bool {
	for _, f := range mc.Finalizers {
		if f == name {
			return true
		}
	}
	return false
}

// clusterEndpoint holds the discovered host and port for a workload cluster's replication service.
type clusterEndpoint struct {
	Host string
	Port int32
}

// updateClusterEndpoint stores the discovered endpoint in the cluster's status entry.
func updateClusterEndpoint(mc *pgmcv1alpha1.PostgresMC, clusterName, endpoint string) {
	for i, cs := range mc.Status.Clusters {
		if cs.Name == clusterName {
			mc.Status.Clusters[i].Endpoint = endpoint
			return
		}
	}
}

// clusterEndpointFromStatus parses the previously stored "host:port" endpoint from status.
// Returns a zero-value clusterEndpoint if not found or unparseable.
func clusterEndpointFromStatus(mc *pgmcv1alpha1.PostgresMC, clusterName string) clusterEndpoint {
	for _, cs := range mc.Status.Clusters {
		if cs.Name != clusterName || cs.Endpoint == "" {
			continue
		}
		i := strings.LastIndex(cs.Endpoint, ":")
		if i <= 0 {
			continue
		}
		p, err := strconv.Atoi(cs.Endpoint[i+1:])
		if err != nil || p == 0 {
			continue
		}
		return clusterEndpoint{Host: cs.Endpoint[:i], Port: int32(p)}
	}
	return clusterEndpoint{}
}

// setClusterReady sets the per-cluster Ready condition, which the aggregator uses
// to compute the top-level phase. Must be called for every cluster on every reconcile.
func setClusterReady(mc *pgmcv1alpha1.PostgresMC, clusterName string, status metav1.ConditionStatus, reason, message string) {
	mc.Status.Clusters = internstatus.SetClusterCondition(
		mc.Status.Clusters, clusterName,
		pgmcv1alpha1.ConditionReady, status,
		mc.Generation, reason, message,
	)
}

// ensureClusterStatusRoles pre-populates ClusterStatus.Role from spec.
// CRD validation requires role to be "primary" or "standby" — never empty.
// Called at the top of every reconcile before any status patch.
func ensureClusterStatusRoles(mc *pgmcv1alpha1.PostgresMC) {
	roleByName := make(map[string]pgmcv1alpha1.ClusterRole, len(mc.Spec.Clusters))
	for _, c := range mc.Spec.Clusters {
		roleByName[c.Name] = c.Role
	}
	// Update role on existing status entries.
	seen := make(map[string]bool, len(mc.Status.Clusters))
	for i, cs := range mc.Status.Clusters {
		if role, ok := roleByName[cs.Name]; ok {
			mc.Status.Clusters[i].Role = role
			seen[cs.Name] = true
		}
	}
	// Create entries for clusters not yet present in status.
	for _, c := range mc.Spec.Clusters {
		if !seen[c.Name] {
			mc.Status.Clusters = append(mc.Status.Clusters, pgmcv1alpha1.ClusterStatus{
				Name: c.Name,
				Role: c.Role,
			})
		}
	}
}

func validateSpec(mc *pgmcv1alpha1.PostgresMC) error {
	primaryCount := 0
	names := map[string]bool{}
	for _, c := range mc.Spec.Clusters {
		if c.Role == pgmcv1alpha1.ClusterRolePrimary {
			primaryCount++
		}
		if names[c.Name] {
			return fmt.Errorf("duplicate cluster name %q", c.Name)
		}
		names[c.Name] = true
	}
	if primaryCount != 1 {
		return fmt.Errorf("exactly one cluster must have role=primary, found %d", primaryCount)
	}
	return nil
}
