package v1alpha1

const (
	ConditionReady              = "Ready"
	ConditionProgressing        = "Progressing"
	ConditionDegraded           = "Degraded"
	ConditionFailoverInProgress = "FailoverInProgress"

	ConditionClusterReachable     = "Reachable"
	ConditionPostgresqlReconciled = "PostgresqlReconciled"
	ConditionCredentialsSynced    = "CredentialsSynced"
	ConditionCrossClusterSvcReady = "CrossClusterServiceReady"
	ConditionStandbyStreaming     = "StandbyStreaming"

	ReasonKubeconfigMissing  = "KubeconfigMissing"
	ReasonClusterUnreachable = "ClusterUnreachable"
	ReasonZalandoApplyFailed = "ZalandoApplyFailed"
	ReasonCredsNotReady      = "CredentialsNotReady"
	ReasonPrimaryNotReady    = "PrimaryNotReady"
	ReasonReconcileSuccess   = "ReconcileSuccess"
	ReasonFailoverStarted    = "FailoverStarted"
	ReasonFailoverCompleted  = "FailoverCompleted"
	ReasonSpecInvalid        = "SpecInvalid"
	ReasonAutoFailover       = "AutoFailover"
)
