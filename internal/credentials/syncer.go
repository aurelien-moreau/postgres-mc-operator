package credentials

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Syncer reads credential Secrets from the primary workload cluster and upserts
// identical Secrets on each standby workload cluster.
type Syncer interface {
	SyncFromPrimary(ctx context.Context, plan SyncPlan) (SyncResult, error)
}

// SyncPlan describes what to sync and where.
type SyncPlan struct {
	PostgresMCName string
	ClusterName    string // Zalando cluster name on the primary
	PrimaryClient  client.Client
	PrimaryNS      string
	Standbys       []StandbyTarget
}

// StandbyTarget is one destination for credential sync.
type StandbyTarget struct {
	ClusterName string
	Client      client.Client
	Namespace   string
}

// SyncResult reports the outcome per standby cluster.
type SyncResult struct {
	PerCluster map[string]error
}

func (r SyncResult) HasErrors() bool {
	for _, err := range r.PerCluster {
		if err != nil {
			return true
		}
	}
	return false
}

type syncer struct{}

func NewSyncer() Syncer { return &syncer{} }

func (s *syncer) SyncFromPrimary(ctx context.Context, plan SyncPlan) (SyncResult, error) {
	result := SyncResult{PerCluster: make(map[string]error)}

	// Collect all credential secrets from the primary cluster.
	secrets, err := s.fetchCredentials(ctx, plan.PrimaryClient, plan.PrimaryNS, plan.ClusterName)
	if err != nil {
		return result, fmt.Errorf("fetching credentials from primary: %w", err)
	}

	for _, standby := range plan.Standbys {
		result.PerCluster[standby.ClusterName] = s.syncToStandby(ctx, secrets, plan.PostgresMCName, standby)
	}
	return result, nil
}

// fetchCredentials reads the three Zalando credential secrets from the primary cluster.
// It does not fail if a secret is not yet present — the caller checks completeness.
func (s *syncer) fetchCredentials(ctx context.Context, c client.Client, ns, clusterName string) ([]*corev1.Secret, error) {
	var out []*corev1.Secret
	for _, role := range CredentialRoles {
		name := SecretName(role, clusterName)
		var secret corev1.Secret
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &secret); err != nil {
			if errors.IsNotFound(err) {
				continue // primary operator hasn't created it yet
			}
			return nil, fmt.Errorf("getting secret %s/%s: %w", ns, name, err)
		}
		out = append(out, secret.DeepCopy())
	}
	return out, nil
}

func (s *syncer) syncToStandby(ctx context.Context, sources []*corev1.Secret, pgmcName string, standby StandbyTarget) error {
	for _, src := range sources {
		// Rewrite the cluster-name embedded in the secret name for the standby.
		// The Zalando operator on the standby expects to find secrets named after its own cluster.
		targetName := rewriteClusterName(src.Name, standby.ClusterName)
		target := buildTargetSecret(src, targetName, standby.Namespace, pgmcName, standby.ClusterName)

		if err := applySecret(ctx, standby.Client, target); err != nil {
			return fmt.Errorf("applying secret %s/%s on standby %s: %w", standby.Namespace, targetName, standby.ClusterName, err)
		}
	}
	return nil
}

// rewriteClusterName replaces the cluster name portion in a Zalando credential secret name.
// Input:  "standby.app-primary.credentials.postgresql.acid.zalan.do"
// Output: "standby.app-standby-eu.credentials.postgresql.acid.zalan.do"
func rewriteClusterName(secretName, newClusterName string) string {
	for _, role := range CredentialRoles {
		suffix := ".credentials.postgresql.acid.zalan.do"
		prefix := role + "."
		if len(secretName) > len(prefix) && secretName[:len(prefix)] == prefix {
			return prefix + newClusterName + suffix
		}
	}
	return secretName
}

func buildTargetSecret(src *corev1.Secret, name, namespace, pgmcName, clusterName string) *corev1.Secret {
	labels := map[string]string{
		"application":                "spilo",
		"cluster-name":               clusterName,
		"pgmc.aurelops.io/managed":   "true",
		"pgmc.aurelops.io/postgresmc": pgmcName,
	}

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Type: corev1.SecretTypeOpaque,
		Data: src.Data,
	}
}

func applySecret(ctx context.Context, c client.Client, secret *corev1.Secret) error {
	var existing corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name}, &existing)
	if errors.IsNotFound(err) {
		return c.Create(ctx, secret)
	}
	if err != nil {
		return err
	}

	// Skip update if data is identical to avoid unnecessary RV churn.
	if secretDataEqual(existing.Data, secret.Data) {
		return nil
	}

	updated := existing.DeepCopy()
	updated.Data = secret.Data
	updated.Labels = secret.Labels
	return c.Update(ctx, updated)
}

func secretDataEqual(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || string(va) != string(vb) {
			return false
		}
	}
	return true
}
