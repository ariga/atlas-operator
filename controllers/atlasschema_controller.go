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

package controllers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"text/template"
	"time"

	"ariga.io/atlas/sql/sqlcheck"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	atlas "ariga.io/atlas-go-sdk/atlasexec"
	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
	"github.com/ariga/atlas-operator/controllers/watch"
)

var (
	//go:embed templates
	tmpls embed.FS
	tmpl  = template.Must(template.New("operator").ParseFS(tmpls, "templates/*.tmpl"))
)

//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasschemas,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasschemas/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasschemas/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups="",resources=pods,verbs=delete

type (
	// AtlasSchemaReconciler reconciles a AtlasSchema object
	AtlasSchemaReconciler struct {
		client.Client
		cli              CLI
		scheme           *runtime.Scheme
		configMapWatcher *watch.ResourceWatcher
		secretWatcher    *watch.ResourceWatcher
		recorder         record.EventRecorder
	}
	// managedData contains information about the managedData database and its desired state.
	managedData struct {
		ext        string
		desired    string
		driver     string
		url        *url.URL
		exclude    []string
		configfile string
		policy     dbv1alpha1.Policy
		schemas    []string
		devURL     *url.URL
	}
	CLI interface {
		SchemaApply(context.Context, *atlas.SchemaApplyParams) (*atlas.SchemaApply, error)
		SchemaInspect(ctx context.Context, data *atlas.SchemaInspectParams) (string, error)
		Lint(ctx context.Context, data *atlas.LintParams) (*atlas.SummaryReport, error)
	}
	destructiveErr struct {
		diags []sqlcheck.Diagnostic
	}
)

func NewAtlasSchemaReconciler(mgr manager.Manager, execPath string) *AtlasSchemaReconciler {
	return &AtlasSchemaReconciler{
		Client:           mgr.GetClient(),
		scheme:           mgr.GetScheme(),
		cli:              atlas.NewClientWithPath(execPath),
		configMapWatcher: watch.New(),
		secretWatcher:    watch.New(),
		recorder:         mgr.GetEventRecorderFor("atlasschema-controller"),
	}
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *AtlasSchemaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, err error) {
	var (
		log = log.FromContext(ctx)
		res = &dbv1alpha1.AtlasSchema{}
	)
	if err := r.Get(ctx, req.NamespacedName, res); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	defer func() {
		if err := r.Status().Update(ctx, res); err != nil {
			log.Error(err, "failed to update status")
		}
		// Watch the configMap and secret referenced by the schema.
		r.watchRefs(res)
		// Clean up any resources created by the controller after the reconciler is successful.
		if res.IsReady() {
			r.cleanUp(ctx, res)
		}
	}()
	// When the resource is first created, create the "Ready" condition.
	if len(res.Status.Conditions) == 0 {
		res.SetNotReady("Reconciling", "Reconciling")
		return ctrl.Result{Requeue: true}, nil
	}
	data, err := r.extractData(ctx, res)
	if err != nil {
		res.SetNotReady("ReadSchema", err.Error())
		return result(err)
	}
	// If the schema has changed and the schema's ready condition is not false, immediately set it to false.
	// This is done so that the observed status of the schema reflects its "in-progress" state while it is being
	// reconciled.
	hash := data.hash()
	if res.IsReady() && res.IsHashModified(hash) {
		res.SetNotReady("Reconciling", "current schema does not match last applied")
		return ctrl.Result{Requeue: true}, nil
	}
	var devURL string
	if data.devURL != nil {
		// if the user has specified a devURL, use it.
		devURL = data.devURL.String()
	} else {
		// otherwise, spin up a dev database.
		devURL, err = r.devURL(ctx, res, *data.url)
		if err != nil {
			res.SetNotReady("GettingDevDBURL", err.Error())
			return result(err)
		}
	}
	conf, cleanconf, err := configFile(res.Spec.Policy)
	if err != nil {
		res.SetNotReady("CreatingConfigFile", err.Error())
		return result(err)
	}
	defer cleanconf()
	data.configfile = conf
	// Verify the first run doesn't contain destructive changes.
	if res.Status.LastApplied == 0 {
		if err := r.verifyFirstRun(ctx, data, devURL); err != nil {
			reason := "VerifyingFirstRun"
			msg := err.Error()
			var d destructiveErr
			if errors.As(err, &d) {
				reason = "FirstRunDestructive"
				msg = err.Error() + "\n" +
					"To prevent accidental drop of resources, first run of a schema must not contain destructive changes.\n" +
					"Read more: https://atlasgo.io/integrations/kubernetes/#destructive-changes"
			}
			res.SetNotReady(reason, msg)
			r.recorder.Event(res, corev1.EventTypeWarning, reason, msg)
			return result(err)
		}
	}
	if shouldLint(data) {
		if err := r.lint(ctx, data, devURL); err != nil {
			res.SetNotReady("LintPolicyError", err.Error())
			r.recorder.Event(res, corev1.EventTypeWarning, "LintPolicyError", err.Error())
			return result(err)
		}
	}
	app, err := r.apply(ctx, data, devURL)
	if err != nil {
		res.SetNotReady("ApplyingSchema", err.Error())
		r.recorder.Event(res, corev1.EventTypeWarning, "ApplyingSchema", err.Error())
		return result(err)
	}
	res.SetReady(dbv1alpha1.AtlasSchemaStatus{
		ObservedHash: hash,
		LastApplied:  time.Now().Unix(),
	}, app)
	r.recorder.Event(res, corev1.EventTypeNormal, "Applied", "Applied schema")
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AtlasSchemaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbv1alpha1.AtlasSchema{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&dbv1alpha1.AtlasSchema{}).
		Watches(&source.Kind{Type: &corev1.ConfigMap{}}, r.configMapWatcher).
		Watches(&source.Kind{Type: &corev1.Secret{}}, r.secretWatcher).
		Complete(r)
}

func (r *AtlasSchemaReconciler) watchRefs(res *dbv1alpha1.AtlasSchema) {
	if c := res.Spec.Schema.ConfigMapKeyRef; c != nil {
		r.configMapWatcher.Watch(
			types.NamespacedName{Name: c.Name, Namespace: res.Namespace},
			res.NamespacedName(),
		)
	}
	if s := res.Spec.URLFrom.SecretKeyRef; s != nil {
		r.secretWatcher.Watch(
			types.NamespacedName{Name: s.Name, Namespace: res.Namespace},
			res.NamespacedName(),
		)
	}
	if s := res.Spec.Credentials.PasswordFrom.SecretKeyRef; s != nil {
		r.secretWatcher.Watch(
			types.NamespacedName{Name: s.Name, Namespace: res.Namespace},
			res.NamespacedName(),
		)
	}
	if s := res.Spec.DevURLFrom.SecretKeyRef; s != nil {
		r.secretWatcher.Watch(
			types.NamespacedName{Name: s.Name, Namespace: res.Namespace},
			res.NamespacedName(),
		)
	}
}

func (r *AtlasSchemaReconciler) apply(ctx context.Context, data *managedData, devURL string) (*atlas.SchemaApply, error) {
	file, clean, err := atlas.TempFile(data.desired, data.ext)
	if err != nil {
		return nil, err
	}
	defer clean()
	apply, err := r.cli.SchemaApply(ctx, &atlas.SchemaApplyParams{
		URL:       data.url.String(),
		To:        file,
		DevURL:    devURL,
		Exclude:   data.exclude,
		ConfigURL: data.configfile,
		Schema:    data.schemas,
	})
	if isSQLErr(err) {
		return nil, err
	}
	if err != nil {
		return nil, transient(err)
	}
	return apply, nil
}

// extractData extracts the info about the managed database and its desired state.
func (r *AtlasSchemaReconciler) extractData(ctx context.Context, res *dbv1alpha1.AtlasSchema) (*managedData, error) {
	var (
		dbURL, devURL *url.URL
	)
	data, ext, err := res.Spec.Schema.Content(ctx, r.Client, res.Namespace)
	if err != nil {
		return nil, transient(err)
	}
	dbURL, err = res.Spec.DatabaseURL(ctx, r, res.Namespace)
	if err != nil {
		r.recorder.Eventf(res, corev1.EventTypeWarning, "DatabaseURL", err.Error())
		return nil, transient(err)
	}
	switch spec := res.Spec; {
	case spec.DevURL != "":
		devURL, err = url.Parse(res.Spec.DevURL)
		if err != nil {
			return nil, err
		}
	case spec.DevURLFrom.SecretKeyRef != nil:
		v, err := getSecretValue(ctx, r, res.Namespace, *spec.DevURLFrom.SecretKeyRef)
		if err != nil {
			return nil, err
		}
		devURL, err = url.Parse(v)
		if err != nil {
			return nil, err
		}
	}
	return &managedData{
		desired: string(data),
		driver:  driver(dbURL.Scheme),
		exclude: res.Spec.Exclude,
		ext:     ext,
		policy:  res.Spec.Policy,
		schemas: res.Spec.Schemas,
		url:     dbURL,
		devURL:  devURL,
	}, nil
}

func (r *AtlasSchemaReconciler) recordErrEvent(res *dbv1alpha1.AtlasMigration, err error) {
	reason := "Error"
	if isTransient(err) {
		reason = "TransientErr"
	}
	r.recorder.Event(res, corev1.EventTypeWarning, reason, strings.TrimSpace(err.Error()))
}

// hash returns the sha256 hash of the desired.
func (d *managedData) hash() string {
	h := sha256.New()
	h.Write([]byte(d.desired))
	return hex.EncodeToString(h.Sum(nil))
}

func (d *managedData) schemaBound() bool {
	switch {
	case d.driver == "sqlite":
		return true
	case d.driver == "postgres":
		if d.url.Query().Get("search_path") != "" {
			return true
		}
	case d.driver == "mysql":
		if d.url.Path != "" {
			return true
		}
	}
	return false
}

func (d destructiveErr) Error() string {
	var buf strings.Builder
	buf.WriteString("destructive changes detected:\n")
	for _, diag := range d.diags {
		buf.WriteString("- " + diag.Text + "\n")
	}
	return buf.String()
}

// shouldLint reports if the schema has a policy that requires linting.
func shouldLint(des *managedData) bool {
	return des.policy.Lint.Destructive.Error
}

func configFile(policy dbv1alpha1.Policy) (string, func() error, error) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "conf.tmpl", policy); err != nil {
		return "", nil, err
	}
	return atlas.TempFile(buf.String(), "hcl")
}
