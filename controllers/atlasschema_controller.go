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

package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	atlas "ariga.io/atlas-go-sdk/atlasexec"
	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
	"github.com/ariga/atlas-operator/controllers/watch"
)

//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=configmaps;secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=core,resources=pods,verbs=create;delete;get;list;watch
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasschemas,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasschemas/finalizers,verbs=update
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasschemas/status,verbs=get;update;patch

type (
	// AtlasSchemaReconciler reconciles a AtlasSchema object
	AtlasSchemaReconciler struct {
		client.Client
		execPath         string
		scheme           *runtime.Scheme
		configMapWatcher *watch.ResourceWatcher
		secretWatcher    *watch.ResourceWatcher
		recorder         record.EventRecorder
		prewarmDevDB     bool
	}
	// managedData contains information about the managed database and its desired state.
	managedData struct {
		EnvName string
		URL     *url.URL
		DevURL  string
		Schemas []string
		Exclude []string
		Policy  *dbv1alpha1.Policy
		TxMode  dbv1alpha1.TransactionMode

		desired []byte
		ext     string
	}
)

const sqlLimitSize = 1024

func NewAtlasSchemaReconciler(mgr Manager, execPath string, prewarmDevDB bool) *AtlasSchemaReconciler {
	return &AtlasSchemaReconciler{
		Client:           mgr.GetClient(),
		scheme:           mgr.GetScheme(),
		execPath:         execPath,
		configMapWatcher: watch.New(),
		secretWatcher:    watch.New(),
		recorder:         mgr.GetEventRecorderFor("atlasschema-controller"),
		prewarmDevDB:     prewarmDevDB,
	}
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *AtlasSchemaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, err error) {
	var (
		log = log.FromContext(ctx)
		res = &dbv1alpha1.AtlasSchema{}
	)
	if err = r.Get(ctx, req.NamespacedName, res); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	defer func() {
		// At the end of reconcile, update the status of the resource base on the error
		if err != nil {
			r.recordErrEvent(res, err)
		}
		if err := r.Status().Update(ctx, res); err != nil {
			log.Error(err, "failed to update resource status")
		}
		// After updating the status, watch the dependent resources
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
		r.recordErrEvent(res, err)
		return result(err)
	}
	hash, err := data.hash()
	if err != nil {
		res.SetNotReady("CalculatingHash", err.Error())
		r.recordErrEvent(res, err)
		return result(err)
	}
	// We need to update the ready condition immediately before doing
	// any heavy jobs if the hash is different from the last applied.
	// This is to ensure that other tools know we are still applying the changes.
	if res.IsReady() && res.IsHashModified(hash) {
		res.SetNotReady("Reconciling", "current schema does not match last applied")
		return ctrl.Result{Requeue: true}, nil
	}
	// ====================================================
	// Starting area to handle the heavy jobs.
	// Below this line is the main logic of the controller.
	// ====================================================
	if data.DevURL == "" {
		// The user has not specified an URL for dev-db,
		// spin up a dev-db and get the connection string.
		data.DevURL, err = r.devURL(ctx, res, *data.URL)
		if err != nil {
			res.SetNotReady("GettingDevDB", err.Error())
			return result(err)
		}
	}
	// Create a working directory for the Atlas CLI
	// The working directory contains the atlas.hcl config.
	wd, err := atlas.NewWorkingDir(atlas.WithAtlasHCL(data.render))
	if err != nil {
		res.SetNotReady("CreatingWorkingDir", err.Error())
		r.recordErrEvent(res, err)
		return result(err)
	}
	defer wd.Close()
	// Write the schema file to the working directory.
	_, err = wd.WriteFile(data.Source(), data.desired)
	if err != nil {
		res.SetNotReady("CreatingSchemaFile", err.Error())
		r.recordErrEvent(res, err)
		return result(err)
	}
	switch {
	case res.Status.LastApplied == 0:
		// Verify the first run doesn't contain destructive changes.
		err = r.lint(ctx, wd, data.EnvName, atlas.Vars{"lint_destructive": "true"})
		switch d := (&destructiveErr{}); {
		case err == nil:
		case errors.As(err, &d):
			reason, msg := d.FirstRun()
			res.SetNotReady(reason, msg)
			r.recorder.Event(res, corev1.EventTypeWarning, reason, msg)
			// Don't requeue destructive errors.
			return ctrl.Result{}, nil
		default:
			reason, msg := "VerifyingFirstRun", err.Error()
			res.SetNotReady(reason, msg)
			r.recorder.Event(res, corev1.EventTypeWarning, reason, msg)
			if !isSQLErr(err) {
				err = transient(err)
			}
			r.recordErrEvent(res, err)
			return result(err)
		}
	case data.shouldLint():
		// Run the linting policy.
		if err = r.lint(ctx, wd, data.EnvName, nil); err != nil {
			reason, msg := "LintPolicyError", err.Error()
			res.SetNotReady(reason, msg)
			r.recorder.Event(res, corev1.EventTypeWarning, reason, msg)
			if !isSQLErr(err) {
				err = transient(err)
			}
			r.recordErrEvent(res, err)
			return result(err)
		}
	}
	report, err := r.apply(ctx, wd.Path(), data.EnvName, string(data.TxMode))
	if err != nil {
		res.SetNotReady("ApplyingSchema", err.Error())
		r.recorder.Event(res, corev1.EventTypeWarning, "ApplyingSchema", err.Error())
		if !isSQLErr(err) {
			err = transient(err)
		}
		r.recordErrEvent(res, err)
		return result(err)
	}
	// Truncate the applied and pending changes to 1024 bytes.
	report.Changes.Applied = truncateSQL(report.Changes.Applied, sqlLimitSize)
	report.Changes.Pending = truncateSQL(report.Changes.Pending, sqlLimitSize)
	res.SetReady(dbv1alpha1.AtlasSchemaStatus{
		LastApplied:  time.Now().Unix(),
		ObservedHash: hash,
	}, report)
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

func (r *AtlasSchemaReconciler) apply(ctx context.Context, dir, envName, txMode string) (*atlas.SchemaApply, error) {
	cli, err := atlas.NewClient(dir, r.execPath)
	if err != nil {
		return nil, err
	}
	return cli.SchemaApply(ctx, &atlas.SchemaApplyParams{
		Env:    envName,
		TxMode: txMode,
	})
}

// extractData extracts the info about the managed database and its desired state.
func (r *AtlasSchemaReconciler) extractData(ctx context.Context, res *dbv1alpha1.AtlasSchema) (_ *managedData, err error) {
	var (
		s    = res.Spec
		data = &managedData{
			EnvName: defaultEnvName,
			DevURL:  s.DevURL,
			Schemas: s.Schemas,
			Exclude: s.Exclude,
			Policy:  s.Policy,
			TxMode:  s.TxMode,
		}
	)
	data.URL, err = s.DatabaseURL(ctx, r, res.Namespace)
	if err != nil {
		return nil, transient(err)
	}
	data.desired, data.ext, err = s.Schema.Content(ctx, r, res.Namespace)
	if err != nil {
		return nil, transient(err)
	}
	if s := s.DevURLFrom.SecretKeyRef; s != nil {
		// SecretKeyRef is set, get the secret value
		// then override the dev url.
		data.DevURL, err = getSecretValue(ctx, r, res.Namespace, s)
		if err != nil {
			return nil, err
		}
	}
	return data, nil
}

func (r *AtlasSchemaReconciler) recordErrEvent(res *dbv1alpha1.AtlasSchema, err error) {
	reason := "Error"
	if isTransient(err) {
		reason = "TransientErr"
	}
	r.recorder.Event(res, corev1.EventTypeWarning, reason, strings.TrimSpace(err.Error()))
}

// Source returns the file name of the desired schema.
func (d *managedData) Source() string {
	return fmt.Sprintf("schema.%s", d.ext)
}

// ShouldLint returns true if the linting policy is set to error.
func (d *managedData) shouldLint() bool {
	p := d.Policy
	if p == nil || p.Lint == nil || p.Lint.Destructive == nil {
		return false
	}
	return p.Lint.Destructive.Error
}

// hash returns the sha256 hash of the desired.
func (d *managedData) hash() (string, error) {
	h := sha256.New()
	h.Write([]byte(d.desired))
	return hex.EncodeToString(h.Sum(nil)), nil
}

// render renders the atlas.hcl template.
//
// The template is used by the Atlas CLI to apply the schema.
// It also validates the data before rendering the template.
func (d *managedData) render(w io.Writer) error {
	if d.EnvName == "" {
		return errors.New("env name is not set")
	}
	if d.URL == nil {
		return errors.New("database url is not set")
	}
	if d.DevURL == "" {
		return errors.New("dev url is not set")
	}
	if d.ext == "" {
		return errors.New("schema extension is not set")
	}
	return tmpl.ExecuteTemplate(w, "atlas_schema.tmpl", d)
}

func truncateSQL(s []string, size int) []string {
	total := 0
	for _, v := range s {
		total += len(v)
	}
	if total > size {
		switch idx, len := strings.IndexRune(s[0], '\n'), len(s[0]); {
		case len <= size:
			total -= len
			return []string{s[0], fmt.Sprintf("-- truncated %d bytes...", total)}
		case idx != -1 && idx <= size:
			total -= (idx + 1)
			return []string{fmt.Sprintf("%s\n-- truncated %d bytes...", s[0][:idx], total)}
		default:
			return []string{fmt.Sprintf("-- truncated %d bytes...", total)}
		}
	}
	return s
}
