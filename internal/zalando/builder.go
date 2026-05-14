// Package zalando builds Zalando acid.zalan.do/v1 postgresql Unstructured objects
// from a PostgresMC spec. Using Unstructured avoids vendoring the Zalando module.
package zalando

import (
	"encoding/json"
	"fmt"

	pgmcv1alpha1 "github.com/aurelops/postgres-mc-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	zalandoAPIVersion = "acid.zalan.do/v1"
	zalandoKind       = "postgresql"
)

// Builder creates Zalando postgresql Unstructured objects.
type Builder interface {
	BuildPrimary(mc *pgmcv1alpha1.PostgresMC, target pgmcv1alpha1.ClusterTarget) (*unstructured.Unstructured, error)
	BuildStandby(mc *pgmcv1alpha1.PostgresMC, target pgmcv1alpha1.ClusterTarget, primaryHost string, primaryPort int32) (*unstructured.Unstructured, error)
}

type builder struct{}

func NewBuilder() Builder { return &builder{} }

// ClusterName returns the Zalando cluster name for a target.
// Convention: {teamId}-{targetName}
func ClusterName(teamID, targetName string) string {
	return fmt.Sprintf("%s-%s", teamID, targetName)
}

func (b *builder) BuildPrimary(mc *pgmcv1alpha1.PostgresMC, target pgmcv1alpha1.ClusterTarget) (*unstructured.Unstructured, error) {
	return buildPostgresql(mc, target, nil)
}

func (b *builder) BuildStandby(mc *pgmcv1alpha1.PostgresMC, target pgmcv1alpha1.ClusterTarget, primaryHost string, primaryPort int32) (*unstructured.Unstructured, error) {
	standby := map[string]interface{}{
		"standby_host": primaryHost,
		"standby_port": fmt.Sprintf("%d", primaryPort),
	}
	return buildPostgresql(mc, target, standby)
}

func buildPostgresql(mc *pgmcv1alpha1.PostgresMC, target pgmcv1alpha1.ClusterTarget, standby map[string]interface{}) (*unstructured.Unstructured, error) {
	// Parse the user-provided spec blob.
	var specMap map[string]interface{}
	if len(mc.Spec.PostgresqlSpec.Raw) > 0 {
		if err := json.Unmarshal(mc.Spec.PostgresqlSpec.Raw, &specMap); err != nil {
			return nil, fmt.Errorf("parsing postgresqlSpec: %w", err)
		}
	} else {
		specMap = make(map[string]interface{})
	}

	specMap["teamId"] = mc.Spec.TeamID

	if standby != nil {
		specMap["standby"] = standby
	} else {
		// Ensure no leftover standby from a previous failover state.
		delete(specMap, "standby")
	}

	clusterName := ClusterName(mc.Spec.TeamID, target.Name)

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": zalandoAPIVersion,
			"kind":       zalandoKind,
			"metadata": map[string]interface{}{
				"name":      clusterName,
				"namespace": target.Namespace,
				"labels": map[string]interface{}{
					"pgmc.aurelops.com/managed-by": "postgres-mc-operator",
					"pgmc.aurelops.com/postgresmc": mc.Name,
					"pgmc.aurelops.com/cluster":    target.Name,
				},
			},
			"spec": specMap,
		},
	}
	return obj, nil
}
