package clusters

import (
	"context"
	"fmt"
	"sync"
	"time"

	pgmcv1alpha1 "github.com/aurelops/postgres-mc-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Registry provides cached controller-runtime clients for workload clusters.
// Each client is built from a kubeconfig stored in a Secret on the tech cluster.
type Registry interface {
	GetClient(ctx context.Context, target pgmcv1alpha1.ClusterTarget, mcNamespace string) (client.Client, error)
	Invalidate(secretKey types.NamespacedName)
}

type cacheKey struct {
	namespace, name, resourceVersion string
}

type cachedEntry struct {
	c       client.Client
	builtAt time.Time
}

type registry struct {
	techClient client.Client
	scheme     *runtime.Scheme
	ttl        time.Duration

	mu    sync.RWMutex
	cache map[string]*cachedEntry // key: "ns/name@rv"
}

// NewRegistry creates a Registry that reads kubeconfig Secrets from the tech cluster.
// ttl controls how long a client is cached before re-reading the Secret.
func NewRegistry(techClient client.Client, scheme *runtime.Scheme, ttl time.Duration) Registry {
	return &registry{
		techClient: techClient,
		scheme:     scheme,
		ttl:        ttl,
		cache:      make(map[string]*cachedEntry),
	}
}

func (r *registry) GetClient(ctx context.Context, target pgmcv1alpha1.ClusterTarget, mcNamespace string) (client.Client, error) {
	secretNS := target.SecretNamespace
	if secretNS == "" {
		secretNS = mcNamespace
	}

	var secret corev1.Secret
	if err := r.techClient.Get(ctx, types.NamespacedName{Namespace: secretNS, Name: target.ClusterRef}, &secret); err != nil {
		return nil, fmt.Errorf("getting kubeconfig secret %s/%s: %w", secretNS, target.ClusterRef, err)
	}

	key := fmt.Sprintf("%s/%s@%s", secretNS, target.ClusterRef, secret.ResourceVersion)

	r.mu.RLock()
	entry, ok := r.cache[key]
	r.mu.RUnlock()

	if ok && time.Since(entry.builtAt) < r.ttl {
		return entry.c, nil
	}

	kubeconfigData, ok := secret.Data["kubeconfig"]
	if !ok {
		return nil, fmt.Errorf("secret %s/%s missing key 'kubeconfig'", secretNS, target.ClusterRef)
	}

	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigData)
	if err != nil {
		return nil, fmt.Errorf("parsing kubeconfig from secret %s/%s: %w", secretNS, target.ClusterRef, err)
	}

	c, err := client.New(cfg, client.Options{Scheme: r.scheme})
	if err != nil {
		return nil, fmt.Errorf("building client for cluster %s: %w", target.Name, err)
	}

	r.mu.Lock()
	r.cache[key] = &cachedEntry{c: c, builtAt: time.Now()}
	r.mu.Unlock()

	return c, nil
}

func (r *registry) Invalidate(secretKey types.NamespacedName) {
	prefix := fmt.Sprintf("%s/%s@", secretKey.Namespace, secretKey.Name)
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.cache {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(r.cache, k)
		}
	}
}
