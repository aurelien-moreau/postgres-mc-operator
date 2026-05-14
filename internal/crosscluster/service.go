package crosscluster

import (
	"context"
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ServiceManager creates and manages cross-cluster Services on standby clusters
// so standby pods can reach the primary via a stable in-cluster DNS name.
type ServiceManager interface {
	EnsurePrimaryAlias(ctx context.Context, plan AliasPlan) error
	DeleteAlias(ctx context.Context, c client.Client, ns, name string) error
}

// AliasPlan describes one alias to create or update.
type AliasPlan struct {
	PostgresMCName string
	StandbyClient  client.Client
	StandbyNS      string
	PrimaryHost    string // FQDN or IP reachable from the standby pod network
	PrimaryPort    int32
}

type serviceManager struct{}

func NewServiceManager() ServiceManager { return &serviceManager{} }

func (m *serviceManager) EnsurePrimaryAlias(ctx context.Context, plan AliasPlan) error {
	aliasName := PrimaryAliasName(plan.PostgresMCName)

	if net.ParseIP(plan.PrimaryHost) != nil {
		return m.ensureHeadlessWithEndpoints(ctx, plan, aliasName)
	}
	return m.ensureExternalName(ctx, plan, aliasName)
}

func (m *serviceManager) DeleteAlias(ctx context.Context, c client.Client, ns, pgmcName string) error {
	aliasName := PrimaryAliasName(pgmcName)
	svc := &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: aliasName}, svc); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return c.Delete(ctx, svc)
}

func (m *serviceManager) ensureExternalName(ctx context.Context, plan AliasPlan, aliasName string) error {
	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      aliasName,
			Namespace: plan.StandbyNS,
			Labels: map[string]string{
				"pgmc.aurelops.io/managed":    "true",
				"pgmc.aurelops.io/postgresmc": plan.PostgresMCName,
			},
		},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: plan.PrimaryHost,
			Ports: []corev1.ServicePort{
				{Port: plan.PrimaryPort, Name: "postgresql"},
			},
		},
	}

	var existing corev1.Service
	err := plan.StandbyClient.Get(ctx, types.NamespacedName{Namespace: plan.StandbyNS, Name: aliasName}, &existing)
	if errors.IsNotFound(err) {
		return plan.StandbyClient.Create(ctx, desired)
	}
	if err != nil {
		return fmt.Errorf("getting alias service %s/%s: %w", plan.StandbyNS, aliasName, err)
	}

	if existing.Spec.ExternalName == plan.PrimaryHost {
		return nil // already correct
	}

	updated := existing.DeepCopy()
	updated.Spec.ExternalName = plan.PrimaryHost
	updated.Spec.Ports = desired.Spec.Ports
	return plan.StandbyClient.Update(ctx, updated)
}

func (m *serviceManager) ensureHeadlessWithEndpoints(ctx context.Context, plan AliasPlan, aliasName string) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      aliasName,
			Namespace: plan.StandbyNS,
			Labels: map[string]string{
				"pgmc.aurelops.io/managed":    "true",
				"pgmc.aurelops.io/postgresmc": plan.PostgresMCName,
			},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: "None",
			Ports: []corev1.ServicePort{
				{Port: plan.PrimaryPort, Name: "postgresql"},
			},
		},
	}

	var existing corev1.Service
	err := plan.StandbyClient.Get(ctx, types.NamespacedName{Namespace: plan.StandbyNS, Name: aliasName}, &existing)
	if errors.IsNotFound(err) {
		if err := plan.StandbyClient.Create(ctx, svc); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("getting headless service %s/%s: %w", plan.StandbyNS, aliasName, err)
	}

	endpoints := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      aliasName,
			Namespace: plan.StandbyNS,
		},
		Subsets: []corev1.EndpointSubset{
			{
				Addresses: []corev1.EndpointAddress{{IP: plan.PrimaryHost}},
				Ports:     []corev1.EndpointPort{{Port: plan.PrimaryPort, Name: "postgresql"}},
			},
		},
	}

	var existingEp corev1.Endpoints
	if err := plan.StandbyClient.Get(ctx, types.NamespacedName{Namespace: plan.StandbyNS, Name: aliasName}, &existingEp); err != nil {
		if errors.IsNotFound(err) {
			return plan.StandbyClient.Create(ctx, endpoints)
		}
		return err
	}

	updated := existingEp.DeepCopy()
	updated.Subsets = endpoints.Subsets
	return plan.StandbyClient.Update(ctx, updated)
}
