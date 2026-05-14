package main

import (
	"flag"
	"os"
	"time"

	pgmcv1alpha1 "github.com/aurelops/postgres-mc-operator/api/v1alpha1"
	"github.com/aurelops/postgres-mc-operator/internal/clusters"
	"github.com/aurelops/postgres-mc-operator/internal/controller"
	"github.com/aurelops/postgres-mc-operator/internal/credentials"
	"github.com/aurelops/postgres-mc-operator/internal/crosscluster"
	"github.com/aurelops/postgres-mc-operator/internal/failover"
	"github.com/aurelops/postgres-mc-operator/internal/replicationservice"
	"github.com/aurelops/postgres-mc-operator/internal/zalando"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	var metricsAddr string
	var probeAddr string
	var leaderElect bool

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Metrics endpoint address.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Health probe endpoint address.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Scheme for the technical cluster (operator's home).
	techScheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(techScheme)
	_ = pgmcv1alpha1.AddToScheme(techScheme)

	// Scheme for workload clusters: core resources + Zalando CRD (as Unstructured).
	workloadScheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(workloadScheme)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: techScheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "pgmc.aurelops.io",
	})
	if err != nil {
		ctrl.Log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	zalandoBuilder := zalando.NewBuilder()
	serviceMgr := crosscluster.NewServiceManager()
	clusterRegistry := clusters.NewRegistry(mgr.GetClient(), workloadScheme, 10*time.Minute)

	if err = (&controller.PostgresMCReconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		Recorder:            mgr.GetEventRecorderFor("postgres-mc-operator"),
		Clusters:            clusterRegistry,
		CredSyncer:          credentials.NewSyncer(),
		Services:            serviceMgr,
		Failover:            failover.NewOrchestrator(zalandoBuilder, serviceMgr),
		ZalandoBuilder:      zalandoBuilder,
		ReplicationServices: replicationservice.NewManager(),
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctrl.Log.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "problem running manager")
		os.Exit(1)
	}
}
