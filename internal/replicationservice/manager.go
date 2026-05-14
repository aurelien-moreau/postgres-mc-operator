// Package replicationservice manages the Kubernetes Service that exposes PostgreSQL
// on each workload cluster for cross-cluster WAL streaming replication.
//
// The operator creates one service per cluster (named {pgmcName}-replication) so
// any cluster can act as primary after a failover without manual intervention.
// The external host (node IP or LoadBalancer address) is discovered automatically
// and stored in ClusterStatus.Endpoint.
package replicationservice

import (
	"context"
	"fmt"

	pgmcv1alpha1 "github.com/aurelops/postgres-mc-operator/api/v1alpha1"
	"github.com/aurelops/postgres-mc-operator/internal/zalando"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Manager creates and discovers the replication service on a workload cluster.
type Manager interface {
	// Ensure creates or updates the replication Service on the given workload cluster.
	// Returns the (host, port) that other clusters should use to reach PostgreSQL here.
	Ensure(ctx context.Context, c client.Client, mc *pgmcv1alpha1.PostgresMC, target pgmcv1alpha1.ClusterTarget) (host string, port int32, err error)

	// Delete removes the replication Service (called during finalization).
	Delete(ctx context.Context, c client.Client, mc *pgmcv1alpha1.PostgresMC, target pgmcv1alpha1.ClusterTarget) error
}

// ServiceName returns the name of the replication Service for a given PostgresMC.
func ServiceName(pgmcName string) string {
	return fmt.Sprintf("%s-replication", pgmcName)
}

type manager struct{}

func NewManager() Manager { return &manager{} }

func (m *manager) Ensure(ctx context.Context, c client.Client, mc *pgmcv1alpha1.PostgresMC, target pgmcv1alpha1.ClusterTarget) (string, int32, error) {
	cfg := mc.Spec.ReplicationService
	svcType := cfg.Type
	if svcType == "" {
		svcType = pgmcv1alpha1.ServiceTypeNodePort
	}

	switch svcType {
	case pgmcv1alpha1.ServiceTypeNodePort:
		return m.ensureNodePort(ctx, c, mc, target)
	case pgmcv1alpha1.ServiceTypeLoadBalancer:
		return m.ensureLoadBalancer(ctx, c, mc, target)
	default:
		return "", 0, fmt.Errorf("unsupported replicationService.type: %q", svcType)
	}
}

func (m *manager) Delete(ctx context.Context, c client.Client, mc *pgmcv1alpha1.PostgresMC, target pgmcv1alpha1.ClusterTarget) error {
	svc := &corev1.Service{}
	err := c.Get(ctx, types.NamespacedName{Namespace: target.Namespace, Name: ServiceName(mc.Name)}, svc)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return c.Delete(ctx, svc)
}

// ensureNodePort creates a NodePort Service and returns (nodeIP, nodePort).
func (m *manager) ensureNodePort(ctx context.Context, c client.Client, mc *pgmcv1alpha1.PostgresMC, target pgmcv1alpha1.ClusterTarget) (string, int32, error) {
	nodePort := mc.Spec.ReplicationService.NodePort
	if nodePort == 0 {
		nodePort = 32432
	}

	zalandoClusterName := zalando.ClusterName(mc.Spec.TeamID, target.Name)
	desired := buildNodePortService(mc.Name, target.Namespace, zalandoClusterName, nodePort)

	if err := applyService(ctx, c, desired); err != nil {
		return "", 0, fmt.Errorf("applying NodePort replication service on %s: %w", target.Name, err)
	}

	// Discover node IP — use ExternalHost override if set, otherwise query a node.
	host := target.ExternalHost
	if host == "" {
		var err error
		host, err = discoverNodeIP(ctx, c)
		if err != nil {
			return "", 0, fmt.Errorf("discovering node IP for cluster %s: %w", target.Name, err)
		}
	}

	return host, nodePort, nil
}

// ensureLoadBalancer creates a LoadBalancer Service and returns (lbHostname, 5432).
func (m *manager) ensureLoadBalancer(ctx context.Context, c client.Client, mc *pgmcv1alpha1.PostgresMC, target pgmcv1alpha1.ClusterTarget) (string, int32, error) {
	zalandoClusterName := zalando.ClusterName(mc.Spec.TeamID, target.Name)
	desired := buildLoadBalancerService(mc.Name, target.Namespace, zalandoClusterName)

	if err := applyService(ctx, c, desired); err != nil {
		return "", 0, fmt.Errorf("applying LoadBalancer replication service on %s: %w", target.Name, err)
	}

	// Use ExternalHost override if set.
	if target.ExternalHost != "" {
		return target.ExternalHost, 5432, nil
	}

	// Wait for the LoadBalancer to be provisioned.
	host, err := discoverLoadBalancerHost(ctx, c, target.Namespace, ServiceName(mc.Name))
	if err != nil {
		return "", 0, fmt.Errorf("LoadBalancer not ready on cluster %s: %w", target.Name, err)
	}
	return host, 5432, nil
}

func buildNodePortService(pgmcName, namespace, zalandoClusterName string, nodePort int32) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceName(pgmcName),
			Namespace: namespace,
			Labels: map[string]string{
				"pgmc.aurelops.io/managed":    "true",
				"pgmc.aurelops.io/postgresmc": pgmcName,
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeNodePort,
			// Targets the Patroni leader pod within the Zalando cluster.
			Selector: map[string]string{
				"cluster-name": zalandoClusterName,
				"spilo-role":   "master",
			},
			Ports: []corev1.ServicePort{
				{
					Name:     "postgresql",
					Port:     5432,
					NodePort: nodePort,
					Protocol: corev1.ProtocolTCP,
				},
			},
		},
	}
}

func buildLoadBalancerService(pgmcName, namespace, zalandoClusterName string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceName(pgmcName),
			Namespace: namespace,
			Labels: map[string]string{
				"pgmc.aurelops.io/managed":    "true",
				"pgmc.aurelops.io/postgresmc": pgmcName,
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer,
			Selector: map[string]string{
				"cluster-name": zalandoClusterName,
				"spilo-role":   "master",
			},
			Ports: []corev1.ServicePort{
				{Name: "postgresql", Port: 5432, Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

func applyService(ctx context.Context, c client.Client, desired *corev1.Service) error {
	var existing corev1.Service
	err := c.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, &existing)
	if errors.IsNotFound(err) {
		return c.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	// Only update selector and ports — preserve nodePort allocation.
	updated := existing.DeepCopy()
	updated.Spec.Selector = desired.Spec.Selector
	// Carry over the allocated NodePort if Kubernetes already assigned one.
	if len(existing.Spec.Ports) > 0 && len(desired.Spec.Ports) > 0 {
		if desired.Spec.Ports[0].NodePort != 0 {
			updated.Spec.Ports = desired.Spec.Ports
		}
	}
	return c.Update(ctx, updated)
}

// discoverNodeIP returns the InternalIP of the first Ready node on the cluster.
func discoverNodeIP(ctx context.Context, c client.Client) (string, error) {
	var nodeList corev1.NodeList
	if err := c.List(ctx, &nodeList); err != nil {
		return "", fmt.Errorf("listing nodes: %w", err)
	}
	for _, node := range nodeList.Items {
		if !isNodeReady(node) {
			continue
		}
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP && addr.Address != "" {
				return addr.Address, nil
			}
		}
	}
	return "", fmt.Errorf("no ready node with InternalIP found")
}

func isNodeReady(node corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// discoverLoadBalancerHost returns the hostname or IP from the Service's LB status.
func discoverLoadBalancerHost(ctx context.Context, c client.Client, namespace, name string) (string, error) {
	var svc corev1.Service
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &svc); err != nil {
		return "", err
	}
	ingress := svc.Status.LoadBalancer.Ingress
	if len(ingress) == 0 {
		return "", fmt.Errorf("LoadBalancer not yet provisioned (ingress empty)")
	}
	if ingress[0].Hostname != "" {
		return ingress[0].Hostname, nil
	}
	if ingress[0].IP != "" {
		return ingress[0].IP, nil
	}
	return "", fmt.Errorf("LoadBalancer ingress has neither hostname nor IP")
}
