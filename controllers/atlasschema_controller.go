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
	// managed contains information about the managed database and its desired state.
	managed struct {
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

//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasschemas,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasschemas/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasschemas/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups="",resources=pods,verbs=delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *AtlasSchemaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	var (
		sc      = &dbv1alpha1.AtlasSchema{}
		managed *managed
		err     error
	)
	if err := r.Get(ctx, req.NamespacedName, sc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	defer func() {
		if err := r.Status().Update(ctx, sc); err != nil {
			log.Error(err, "failed to update status")
		}
		// Watch the configMap and secret referenced by the schema.
		r.watch(sc)

		// Clean up any resources created by the controller after the reconciler is successful.
		if sc.IsReady() {
			r.cleanUp(ctx, sc)
		}
	}()
	// When the resource is first created, create the "Ready" condition.
	if len(sc.Status.Conditions) == 0 {
		sc.SetNotReady("Reconciling", "Reconciling")
		return ctrl.Result{Requeue: true}, nil
	}
	managed, err = r.extractManaged(ctx, sc)
	if err != nil {
		sc.SetNotReady("ReadSchema", err.Error())
		return result(err)
	}
	// If the schema has changed and the schema's ready condition is not false, immediately set it to false.
	// This is done so that the observed status of the schema reflects its "in-progress" state while it is being
	// reconciled.
	hash := managed.hash()
	if sc.IsReady() && sc.IsHashModified(hash) {
		sc.SetNotReady("Reconciling", "current schema does not match last applied")
		return ctrl.Result{Requeue: true}, nil
	}
	var devURL string
	if managed.devURL != nil {
		// if the user has specified a devURL, use it.
		devURL = managed.devURL.String()
	} else {
		// otherwise, spin up a dev database.
		devURL, err = r.devURL(ctx, sc, *managed.url)
		if err != nil {
			sc.SetNotReady("GettingDevDBURL", err.Error())
			return result(err)
		}
	}
	conf, cleanconf, err := configFile(sc.Spec.Policy)
	if err != nil {
		sc.SetNotReady("CreatingConfigFile", err.Error())
		return result(err)
	}
	defer cleanconf()
	managed.configfile = conf
	// Verify the first run doesn't contain destructive changes.
	if sc.Status.LastApplied == 0 {
		if err := r.verifyFirstRun(ctx, managed, devURL); err != nil {
			reason := "VerifyingFirstRun"
			msg := err.Error()
			var d destructiveErr
			if errors.As(err, &d) {
				reason = "FirstRunDestructive"
				msg = err.Error() + "\n" +
					"To prevent accidental drop of resources, first run of a schema must not contain destructive changes.\n" +
					"Read more: https://atlasgo.io/integrations/kubernetes/#destructive-changes"
			}
			sc.SetNotReady(reason, msg)
			r.recorder.Event(sc, corev1.EventTypeWarning, reason, msg)
			return result(err)
		}
	}
	if shouldLint(managed) {
		if err := r.lint(ctx, managed, devURL); err != nil {
			sc.SetNotReady("LintPolicyError", err.Error())
			r.recorder.Event(sc, corev1.EventTypeWarning, "LintPolicyError", err.Error())
			return result(err)
		}
	}
	app, err := r.apply(ctx, managed, devURL)
	if err != nil {
		sc.SetNotReady("ApplyingSchema", err.Error())
		r.recorder.Event(sc, corev1.EventTypeWarning, "ApplyingSchema", err.Error())
		return result(err)
	}
	sc.SetReady(dbv1alpha1.AtlasSchemaStatus{
		ObservedHash: hash,
		LastApplied:  time.Now().Unix(),
	}, app)
	r.recorder.Event(sc, corev1.EventTypeNormal, "Applied", "Applied schema")
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

func (r *AtlasSchemaReconciler) watch(sc *dbv1alpha1.AtlasSchema) {
	if c := sc.Spec.Schema.ConfigMapKeyRef; c != nil {
		r.configMapWatcher.Watch(
			types.NamespacedName{Name: c.Name, Namespace: sc.Namespace},
			sc.NamespacedName(),
		)
	}
	if s := sc.Spec.URLFrom.SecretKeyRef; s != nil {
		r.secretWatcher.Watch(
			types.NamespacedName{Name: s.Name, Namespace: sc.Namespace},
			sc.NamespacedName(),
		)
	}
	if s := sc.Spec.Credentials.PasswordFrom.SecretKeyRef; s != nil {
		r.secretWatcher.Watch(
			types.NamespacedName{Name: s.Name, Namespace: sc.Namespace},
			sc.NamespacedName(),
		)
	}
	if s := sc.Spec.DevURLFrom.SecretKeyRef; s != nil {
		r.secretWatcher.Watch(
			types.NamespacedName{Name: s.Name, Namespace: sc.Namespace},
			sc.NamespacedName(),
		)
	}
}

func (r *AtlasSchemaReconciler) apply(ctx context.Context, des *managed, devURL string) (*atlas.SchemaApply, error) {
	file, clean, err := atlas.TempFile(des.desired, des.ext)
	if err != nil {
		return nil, err
	}
	defer clean()
	apply, err := r.cli.SchemaApply(ctx, &atlas.SchemaApplyParams{
		URL:       des.url.String(),
		To:        file,
		DevURL:    devURL,
		Exclude:   des.exclude,
		ConfigURL: des.configfile,
		Schema:    des.schemas,
	})
	if isSQLErr(err) {
		return nil, err
	}
	if err != nil {
		return nil, transient(err)
	}
	return apply, nil
}

// extractManaged extracts the info about the managed database and its desired state.
func (r *AtlasSchemaReconciler) extractManaged(ctx context.Context, sc *dbv1alpha1.AtlasSchema) (*managed, error) {
	var (
		dbURL, devURL *url.URL
	)
	data, ext, err := sc.Spec.Schema.Content(ctx, r.Client, sc.Namespace)
	if err != nil {
		return nil, transient(err)
	}
	dbURL, err = sc.Spec.DatabaseURL(ctx, r, sc.Namespace)
	if err != nil {
		r.recorder.Eventf(sc, corev1.EventTypeWarning, "DatabaseURL", err.Error())
		return nil, transient(err)
	}
	switch spec := sc.Spec; {
	case spec.DevURL != "":
		devURL, err = url.Parse(sc.Spec.DevURL)
		if err != nil {
			return nil, err
		}
	case spec.DevURLFrom.SecretKeyRef != nil:
		v, err := getSecretValue(ctx, r, sc.Namespace, *spec.DevURLFrom.SecretKeyRef)
		if err != nil {
			return nil, err
		}
		devURL, err = url.Parse(v)
		if err != nil {
			return nil, err
		}
	}
	return &managed{
		desired: string(data),
		driver:  driver(dbURL.Scheme),
		exclude: sc.Spec.Exclude,
		ext:     ext,
		policy:  sc.Spec.Policy,
		schemas: sc.Spec.Schemas,
		url:     dbURL,
		devURL:  devURL,
	}, nil
}

// hash returns the sha256 hash of the desired.
func (d *managed) hash() string {
	h := sha256.New()
	h.Write([]byte(d.desired))
	return hex.EncodeToString(h.Sum(nil))
}

func (d *managed) schemaBound() bool {
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
func shouldLint(des *managed) bool {
	return des.policy.Lint.Destructive.Error
}

func configFile(policy dbv1alpha1.Policy) (string, func() error, error) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "conf.tmpl", policy); err != nil {
		return "", nil, err
	}
	return atlas.TempFile(buf.String(), "hcl")
}

// transientErr is an error that should be retried.
type transientErr struct {
	err error
}

func (t *transientErr) Error() string {
	return t.err.Error()
}

func (t *transientErr) Unwrap() error {
	return t.err
}
