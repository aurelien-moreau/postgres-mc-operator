// Package crosscluster manages cross-cluster Kubernetes Services that give
// standby clusters a stable DNS name to reach the primary cluster.
package crosscluster

import "fmt"

// PrimaryAliasName returns the name of the Service created on a standby cluster
// to resolve the primary cluster endpoint.
func PrimaryAliasName(pgmcName string) string {
	return fmt.Sprintf("%s-primary", pgmcName)
}

// PrimaryAliasFQDN returns the in-cluster FQDN of the primary alias Service.
func PrimaryAliasFQDN(pgmcName, namespace string) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local", PrimaryAliasName(pgmcName), namespace)
}
