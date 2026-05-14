package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ClusterRole defines a target cluster's role in the topology.
// +kubebuilder:validation:Enum=primary;standby
type ClusterRole string

const (
	ClusterRolePrimary ClusterRole = "primary"
	ClusterRoleStandby ClusterRole = "standby"
)

// ReplicationMode defines streaming replication semantics.
// +kubebuilder:validation:Enum=async;sync
type ReplicationMode string

const (
	ReplicationAsync ReplicationMode = "async"
	ReplicationSync  ReplicationMode = "sync"
)

// ClusterTarget describes one workload cluster the operator should reconcile against.
type ClusterTarget struct {
	// Name is a stable, user-defined identifier for this target (e.g. "eu-west-1").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// ClusterRef is the name of the Secret on the technical cluster that holds the kubeconfig.
	// The Secret must have a key "kubeconfig".
	// +kubebuilder:validation:MinLength=1
	ClusterRef string `json:"clusterRef"`

	// SecretNamespace is the namespace of the kubeconfig Secret on the technical cluster.
	// Defaults to the PostgresMC's own namespace.
	// +optional
	SecretNamespace string `json:"secretNamespace,omitempty"`

	// Namespace on the workload cluster where the Zalando postgresql resource will be created.
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`

	// Role defines whether this cluster is primary or standby. Exactly one target MUST be primary.
	// +kubebuilder:validation:Enum=primary;standby
	Role ClusterRole `json:"role"`

	// ExternalHost overrides the cross-cluster DNS name used by standbys to reach this cluster.
	// If empty the operator uses the Zalando LoadBalancer Service hostname (primary only).
	// +optional
	ExternalHost string `json:"externalHost,omitempty"`

	// ExternalPort is the port standbys use to reach this cluster.
	// +optional
	// +kubebuilder:default=5432
	ExternalPort int32 `json:"externalPort,omitempty"`
}

// ReplicationSpec controls cross-cluster replication settings.
type ReplicationSpec struct {
	// Mode is the streaming replication mode.
	// +kubebuilder:default=async
	// +optional
	Mode ReplicationMode `json:"mode,omitempty"`
}

// ServiceType is the Kubernetes Service type used to expose PostgreSQL for cross-cluster replication.
// +kubebuilder:validation:Enum=NodePort;LoadBalancer
type ServiceType string

const (
	ServiceTypeNodePort     ServiceType = "NodePort"
	ServiceTypeLoadBalancer ServiceType = "LoadBalancer"
)

// ReplicationServiceConfig defines how the operator exposes PostgreSQL on each cluster
// so any cluster can serve as primary after a failover.
// The operator creates this service automatically — no manual NodePort setup required.
type ReplicationServiceConfig struct {
	// Type is the Kubernetes Service type.
	// NodePort is recommended for local/kind environments.
	// LoadBalancer is recommended for cloud environments.
	// +kubebuilder:default=NodePort
	Type ServiceType `json:"type"`

	// NodePort is the fixed port used when Type=NodePort.
	// The same port is used on every cluster, routable via each cluster's node IP.
	// Must be in the Kubernetes NodePort range (30000–32767).
	// +optional
	// +kubebuilder:default=32432
	// +kubebuilder:validation:Minimum=30000
	// +kubebuilder:validation:Maximum=32767
	NodePort int32 `json:"nodePort,omitempty"`
}

// AutoFailoverSpec controls automatic promotion of a standby when the primary is unreachable.
type AutoFailoverSpec struct {
	// Enabled activates automatic failover. Default false.
	// When true, the operator promotes the first healthy standby after the primary
	// has been unreachable for PrimaryUnreachableThreshold consecutive reconciliations.
	// +optional
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// PrimaryUnreachableThreshold is the number of consecutive reconcile cycles during
	// which the primary must be unreachable before auto-failover is triggered.
	// Each cycle is ~30s, so the default of 3 means ~90s of downtime before promotion.
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	PrimaryUnreachableThreshold int32 `json:"primaryUnreachableThreshold,omitempty"`
}

// PostgresMCSpec defines the desired state of PostgresMC.
type PostgresMCSpec struct {
	// TeamID is propagated to every Zalando postgresql cluster.
	// +kubebuilder:validation:MinLength=1
	TeamID string `json:"teamId"`

	// Clusters is the list of workload-cluster targets. Exactly one must have role=primary.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	Clusters []ClusterTarget `json:"clusters"`

	// PostgresqlSpec is merged into every target's Zalando postgresql.spec.
	// The operator injects the standby section automatically for non-primary targets.
	// Use RawExtension to stay forward-compatible with Zalando schema changes.
	// +kubebuilder:pruning:PreserveUnknownFields
	PostgresqlSpec runtime.RawExtension `json:"postgresqlSpec"`

	// Replication tunables shared across all clusters.
	// +optional
	Replication ReplicationSpec `json:"replication,omitempty"`

	// ReplicationService configures the Service the operator creates on every cluster
	// to expose PostgreSQL for cross-cluster WAL streaming.
	// With this, externalHost on each ClusterTarget becomes optional — the operator
	// discovers the endpoint automatically from node IPs or the LoadBalancer status.
	// +optional
	ReplicationService ReplicationServiceConfig `json:"replicationService,omitempty"`

	// AutoFailover controls automatic promotion when the primary becomes unreachable.
	// Disabled by default to avoid split-brain on transient network failures.
	// +optional
	AutoFailover AutoFailoverSpec `json:"autoFailover,omitempty"`
}

// ClusterStatus reports reconciliation state for a single target cluster.
type ClusterStatus struct {
	// Name matches ClusterTarget.Name.
	Name string `json:"name"`
	// Role is the current role for this cluster (may differ from spec during failover).
	Role ClusterRole `json:"role"`
	// Phase is a human-readable phase for this cluster.
	Phase string `json:"phase,omitempty"`
	// ObservedGeneration is the PostgresMC generation last reconciled for this cluster.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions are per-cluster detailed conditions.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Endpoint is the primary service endpoint (populated for the primary cluster).
	Endpoint string `json:"endpoint,omitempty"`
	// LastSyncedAt is the timestamp of the last successful reconcile for this cluster.
	LastSyncedAt *metav1.Time `json:"lastSyncedAt,omitempty"`
}

// PostgresMCStatus is the observed state of PostgresMC.
type PostgresMCStatus struct {
	// Phase is the rolled-up phase across all targets.
	// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Degraded;FailingOver;Failed
	Phase string `json:"phase,omitempty"`

	// ObservedGeneration is the .metadata.generation last fully reconciled.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// CurrentPrimary is the Name of the ClusterTarget currently acting as primary.
	CurrentPrimary string `json:"currentPrimary,omitempty"`

	// Conditions are top-level conditions for the PostgresMC as a whole.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Clusters contains per-target reconciliation status.
	Clusters []ClusterStatus `json:"clusters,omitempty"`

	// PrimaryFailureCount is the number of consecutive reconciliations during which
	// the current primary was unreachable. Reset to 0 when the primary is reachable again.
	// Used by the auto-failover logic.
	// +optional
	PrimaryFailureCount int32 `json:"primaryFailureCount,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pgmc,scope=Namespaced
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Primary",type=string,JSONPath=`.status.currentPrimary`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PostgresMC struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PostgresMCSpec   `json:"spec,omitempty"`
	Status PostgresMCStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type PostgresMCList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PostgresMC `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PostgresMC{}, &PostgresMCList{})
}
