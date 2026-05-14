// Package credentials handles credential secret synchronization between clusters.
package credentials

import "fmt"

// SecretName returns the Zalando-convention credential secret name for a given role and cluster.
// Format: {role}.{clusterName}.credentials.postgresql.acid.zalan.do
func SecretName(role, clusterName string) string {
	return fmt.Sprintf("%s.%s.credentials.postgresql.acid.zalan.do", role, clusterName)
}

// CredentialRoles are the three Zalando-managed credential types.
var CredentialRoles = []string{"postgres", "standby"}
