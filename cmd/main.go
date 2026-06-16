// Copyright 2023 The Atlas Operator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"ariga.io/atlas/atlasexec"
	"golang.org/x/mod/semver"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	//+kubebuilder:scaffold:imports

	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
	"github.com/ariga/atlas-operator/internal/controller"
	"github.com/ariga/atlas-operator/internal/vercheck"
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
	// prewarmDevDB when disabled it deletes the devDB pods after the schema is created
	prewarmDevDB = "PREWARM_DEVDB"
	// allowCustomConfig when enabled it allows the use of custom config
	allowsCustomConfig = "ALLOW_CUSTOM_CONFIG"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(dbv1alpha1.AddToScheme(scheme))

	//+kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var leaderElectionID string
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var labelSelector string
	var watchNamespaces string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&leaderElectionID, "leader-election-id", "5220c287.atlasgo.io",
		"The name of the resource that leader election will use for holding the leader lock. "+
			"Set a unique value per operator instance when running multiple operators with "+
			"leader election enabled in the same namespace.")
	flag.BoolVar(&secureMetrics, "metrics-secure", false,
		"If set the metrics endpoint is served securely")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.StringVar(&labelSelector, "label-selector", "",
		"A label selector (e.g. \"app=foo,env in (prod,staging)\") that restricts which "+
			"AtlasSchema and AtlasMigration resources the operator manages. When empty, all "+
			"resources are managed. Use this to run multiple operator instances in the same "+
			"namespace, each handling resources matching different labels.")
	flag.StringVar(&watchNamespaces, "namespace", "",
		"A comma-separated list of namespaces that restricts where the operator watches "+
			"for AtlasSchema and AtlasMigration resources. When empty, the operator watches "+
			"all namespaces (cluster scope).")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	namespaces := parseNamespaces(watchNamespaces)
	cacheOpts, err := cacheOptions(labelSelector, namespaces)
	if err != nil {
		setupLog.Error(err, "invalid label selector", "selector", labelSelector)
		os.Exit(1)
	}
	if labelSelector != "" {
		setupLog.Info("restricting managed resources to label selector", "selector", labelSelector)
	}
	if len(namespaces) > 0 {
		setupLog.Info("restricting watch to namespaces", "namespaces", namespaces)
	} else {
		setupLog.Info("watching all namespaces (cluster scope)")
	}
	if v, err := atlasVersion(context.Background()); err != nil {
		setupLog.Error(err, "unable to get atlas version")
		os.Exit(1)
	} else {
		setupLog.Info("detected atlas version", "version", v)
	}
	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancelation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	tlsOpts := []func(*tls.Config){}
	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: tlsOpts,
	})

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Cache:  cacheOpts,
		Metrics: server.Options{
			BindAddress:   metricsAddr,
			SecureServing: secureMetrics,
			TLSOpts:       tlsOpts,
		},
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       leaderElectionID,
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		LeaderElectionReleaseOnCancel: true,
		BaseContext: func() context.Context {
			return dbv1alpha1.WithVersionContext(context.Background(), version)
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}
	prewarmDevDB := getPrewarmDevDBEnv()
	allowCustomConfig := getAllowCustomConfigEnv()
	// Setup controller for AtlasSchema
	schemaController := controller.NewAtlasSchemaReconciler(mgr, prewarmDevDB)
	schemaController.SetAtlasClient(controller.NewAtlasExec)
	if allowCustomConfig {
		schemaController.AllowCustomConfig()
	}
	if err := schemaController.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AtlasSchema")
		os.Exit(1)
	}
	// Setup controller for AtlasMigration
	migrationController := controller.NewAtlasMigrationReconciler(mgr, prewarmDevDB)
	migrationController.SetAtlasClient(controller.NewAtlasExec)
	if allowCustomConfig {
		migrationController.AllowCustomConfig()
	}
	if err = migrationController.SetupWithManager(mgr); err != nil {
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

// cacheOptions builds the manager's cache options from the given label selector
// and the list of namespaces to watch.
//
// When namespaces is empty, the operator watches all namespaces (cluster scope).
// Otherwise the cache (and therefore the operator) is restricted to the given
// namespaces, which also limits the referenced ConfigMaps and Secrets it reads.
//
// When labelSelector is set, only AtlasSchema and AtlasMigration resources
// matching the selector are watched, cached, and therefore reconciled. This
// makes it possible to run multiple operator instances side by side, each
// owning a distinct set of resources. Referenced ConfigMaps and Secrets are
// intentionally left unfiltered by label, since they usually do not carry the
// operator's labels.
func cacheOptions(labelSelector string, namespaces []string) (cache.Options, error) {
	opts := cache.Options{}
	if len(namespaces) > 0 {
		opts.DefaultNamespaces = make(map[string]cache.Config, len(namespaces))
		for _, ns := range namespaces {
			opts.DefaultNamespaces[ns] = cache.Config{}
		}
	}
	if labelSelector != "" {
		selector, err := labels.Parse(labelSelector)
		if err != nil {
			return opts, fmt.Errorf("parsing label selector %q: %w", labelSelector, err)
		}
		// Leaving ByObject.Namespaces nil lets controller-runtime default it
		// from DefaultNamespaces, so the label filter and namespace scope are
		// combined for the managed resources.
		opts.ByObject = map[client.Object]cache.ByObject{
			&dbv1alpha1.AtlasSchema{}:    {Label: selector},
			&dbv1alpha1.AtlasMigration{}: {Label: selector},
		}
	}
	return opts, nil
}

// parseNamespaces splits a comma-separated namespace list into a clean slice,
// trimming whitespace and dropping empty entries.
func parseNamespaces(s string) []string {
	var out []string
	for _, ns := range strings.Split(s, ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			out = append(out, ns)
		}
	}
	return out
}

// atlasVersion runs `atlas version` and returns its string representation.
func atlasVersion(ctx context.Context) (string, error) {
	c, err := atlasexec.NewClient(os.TempDir(), "atlas")
	if err != nil {
		return "", fmt.Errorf("creating atlas client: %w", err)
	}
	v, err := c.Version(ctx)
	if err != nil {
		return "", fmt.Errorf("running atlas version: %w", err)
	}
	return v.String(), nil
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

// getPrewarmDevDBEnv returns the value of the env var PREWARM_DEVDB.
// if the env var is not set, it returns true.
func getPrewarmDevDBEnv() bool {
	env := os.Getenv(prewarmDevDB)
	if env == "" {
		return true
	}
	prewarmDevDB, err := strconv.ParseBool(env)
	if err != nil {
		setupLog.Error(err, "invalid value for env var PREWARM_DEVDB, expected true or false")
		os.Exit(1)
	}
	return prewarmDevDB
}

// getAllowCustomConfigEnv returns the value of the env var ALLOW_CUSTOM_CONFIG.
// if the env var is not set, it returns false.
func getAllowCustomConfigEnv() bool {
	env := os.Getenv(allowsCustomConfig)
	if env == "" {
		return false
	}
	allowsCustomConfig, err := strconv.ParseBool(env)
	if err != nil {
		setupLog.Error(err, "invalid value for env var ALLOW_CUSTOM_CONFIG, expected true or false")
		os.Exit(1)
	}
	return allowsCustomConfig
}
