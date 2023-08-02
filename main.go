/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"flag"
	"os"
	"time"

	"golang.org/x/mod/semver"

	atlas "ariga.io/atlas-go-sdk/atlasexec"
	"github.com/ariga/atlas-operator/internal/vercheck"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
	"github.com/ariga/atlas-operator/controllers"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
	// version holds the operator version. It should be set by build flag
	// "-X 'main.version=${version}'"
	version string
)

const (
	// envNoUpdate when enabled it cancels checking for update
	envNoUpdate = "SKIP_VERCHECK"
	vercheckURL = "https://vercheck.ariga.io"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(dbv1alpha1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                        scheme,
		MetricsBindAddress:            metricsAddr,
		Port:                          9443,
		HealthProbeBindAddress:        probeAddr,
		LeaderElection:                enableLeaderElection,
		LeaderElectionID:              "5220c287.atlasgo.io",
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}
	cwd, err := os.Getwd()
	if err != nil {
		setupLog.Error(err, "unable to get current working directory")
		os.Exit(1)
	}
	cli, err := atlas.NewClient(cwd, "atlas")
	if err != nil {
		setupLog.Error(err, "unable to create atlas client")
		os.Exit(1)
	}
	if err = controllers.NewAtlasSchemaReconciler(mgr, cli).
		SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AtlasSchema")
		os.Exit(1)
	}

	if err = controllers.NewAtlasMigrationReconciler(mgr, cli).
		SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AtlasMigration")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}
	go checkForUpdate()
	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// checkForUpdate checks for version updates and security advisories for the Atlas Operator.
func checkForUpdate() {
	log := ctrl.Log.WithName("vercheck")
	// Users may skip update checking behavior.
	if v := os.Getenv(envNoUpdate); v != "" {
		log.Info("skipping version checking because SKIP_VERCHECK is set")
		return
	}
	// Skip if the current binary version isn't set (dev mode).
	if !semver.IsValid(version) {
		log.Info("skipping version checking because version is not valid")
		return
	}
	log.Info("setting up version checking", "version", version)
	vc := vercheck.New(vercheckURL, "")
	for {
		if err := func() error {
			payload, err := vc.Check(version)
			if err != nil {
				return err
			}
			var b bytes.Buffer
			if err := vercheck.Notify.Execute(&b, payload); err != nil {
				return err
			}
			if msg := b.String(); msg != "" {
				log.Info(msg)
			}
			return nil
		}(); err != nil {
			log.Error(err, "unable to check for update")
		}
		<-time.After(24 * time.Hour)
	}
}
