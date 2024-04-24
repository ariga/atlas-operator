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

/*

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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	atlas "ariga.io/atlas-go-sdk/atlasexec"
	"ariga.io/atlas/sql/migrate"
	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
	"github.com/ariga/atlas-operator/controllers/watch"
)

//+kubebuilder:rbac:groups=core,resources=configmaps;secrets,verbs=create;update;delete;get;list;watch
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasmigrations,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasmigrations/finalizers,verbs=update
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasmigrations/status,verbs=get;update;patch

type (
	// AtlasMigrationReconciler reconciles a AtlasMigration object
	AtlasMigrationReconciler struct {
		client.Client
		scheme           *runtime.Scheme
		execPath         string
		configMapWatcher *watch.ResourceWatcher
		secretWatcher    *watch.ResourceWatcher
		recorder         record.EventRecorder
	}
	// migrationData is the data used to render the HCL template
	// that will be used for Atlas CLI
	migrationData struct {
		EnvName         string
		URL             *url.URL
		Dir             migrate.Dir
		Cloud           *cloud
		RevisionsSchema string
		Baseline        string
		ExecOrder       string
	}
	cloud struct {
		URL       string
		Token     string
		Project   string
		RemoteDir *dbv1alpha1.Remote
	}
)

func NewAtlasMigrationReconciler(mgr Manager, execPath string, _ bool) *AtlasMigrationReconciler {
	return &AtlasMigrationReconciler{
		Client:           mgr.GetClient(),
		scheme:           mgr.GetScheme(),
		execPath:         execPath,
		configMapWatcher: watch.New(),
		secretWatcher:    watch.New(),
		recorder:         mgr.GetEventRecorderFor("atlasmigration-controller"),
	}
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *AtlasMigrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, err error) {
	var (
		log = ctrl.LoggerFrom(ctx)
		res = &dbv1alpha1.AtlasMigration{}
	)
	if err = r.Get(ctx, req.NamespacedName, res); err != nil {
		if apierrors.IsNotFound(err) {
			// The resource has been deleted, clean up its migration directory.
			if err := r.deleteMigrations(ctx, res); err != nil {
				log.Error(err, "failed to delete migration directory")
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
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
	}()
	// When the resource is first created, create the "Ready" condition.
	if len(res.Status.Conditions) == 0 {
		res.SetNotReady("Reconciling", "Reconciling")
		return ctrl.Result{Requeue: true}, nil
	}
	data, err := r.extractData(ctx, res)
	if err != nil {
		res.SetNotReady("ReadingMigrationData", err.Error())
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
		res.SetNotReady("Reconciling", "Current migration data has changed")
		return ctrl.Result{Requeue: true}, nil
	}
	// ====================================================
	// Starting area to handle the heavy jobs.
	// Below this line is the main logic of the controller.
	// ====================================================

	// TODO(giautm): Create DevDB and run linter for new migration
	// files before applying it to the target database.

	// Create a working directory for the Atlas CLI
	// The working directory contains the atlas.hcl config
	// and the migrations directory (if any)
	wd, err := atlas.NewWorkingDir(
		atlas.WithAtlasHCL(data.render),
		atlas.WithMigrations(data.Dir),
	)
	if err != nil {
		res.SetNotReady("ReadingMigrationData", err.Error())
		r.recordErrEvent(res, err)
		return result(err)
	}
	defer wd.Close()
	// Reconcile given resource
	status, err := r.reconcile(ctx, wd.Path(), data.EnvName)
	if err != nil {
		res.SetNotReady("Migrating", strings.TrimSpace(err.Error()))
		r.recordErrEvent(res, err)
		return result(err)
	}
	if data.Dir != nil {
		// Compress the migration directory then store it in the secret
		// for later use when atlas runs the migration down.
		if err := r.storeMigrations(ctx, res, data.Dir); err != nil {
			res.SetNotReady("StoringMigrationDir", err.Error())
			r.recordErrEvent(res, err)
			return result(err)
		}
	}
	status.ObservedHash = hash
	res.SetReady(*status)
	r.recorder.Eventf(res, corev1.EventTypeNormal, "Applied", "Version %s applied", status.LastAppliedVersion)
	return ctrl.Result{}, nil
}

func (r *AtlasMigrationReconciler) storeMigrations(ctx context.Context, res *dbv1alpha1.AtlasMigration, dir migrate.Dir) error {
	var labels = map[string]string{}
	for k, v := range res.Labels {
		labels[k] = v
	}
	labels["name"] = res.Name
	secret, err := newSecretObject(makeKeyLatest(res.Name), dir, labels)
	if err != nil {
		return err
	}
	// Set the namespace of the secret to the same as the resource
	secret.Namespace = res.Namespace
	switch err := r.Create(ctx, secret); {
	case err == nil:
		return nil
	case apierrors.IsAlreadyExists(err):
		// Update the secret if it already exists
		return r.Update(ctx, secret)
	default:
		return err
	}
}

func (r *AtlasMigrationReconciler) deleteMigrations(ctx context.Context, res *dbv1alpha1.AtlasMigration) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      makeKeyLatest(res.Name),
			Namespace: res.Namespace,
		},
	}
	return client.IgnoreNotFound(r.Delete(ctx, secret))
}

// SetupWithManager sets up the controller with the Manager.
func (r *AtlasMigrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbv1alpha1.AtlasMigration{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&dbv1alpha1.AtlasMigration{}).
		Watches(&corev1.Secret{}, r.secretWatcher).
		Watches(&corev1.ConfigMap{}, r.configMapWatcher).
		Complete(r)
}

func (r *AtlasMigrationReconciler) watchRefs(res *dbv1alpha1.AtlasMigration) {
	if c := res.Spec.Dir.ConfigMapRef; c != nil {
		r.configMapWatcher.Watch(
			types.NamespacedName{Name: c.Name, Namespace: res.Namespace},
			res.NamespacedName(),
		)
	}
	if s := res.Spec.Cloud.TokenFrom.SecretKeyRef; s != nil {
		r.secretWatcher.Watch(
			types.NamespacedName{Name: s.Name, Namespace: res.Namespace},
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
}

// Reconcile the given AtlasMigration resource.
func (r *AtlasMigrationReconciler) reconcile(ctx context.Context, wd, envName string) (_ *dbv1alpha1.AtlasMigrationStatus, _ error) {
	c, err := atlas.NewClient(wd, r.execPath)
	if err != nil {
		return nil, err
	}
	// Check if there are any pending migration files
	status, err := c.MigrateStatus(ctx, &atlas.MigrateStatusParams{Env: envName})
	if err != nil {
		if isChecksumErr(err) {
			return nil, err
		}
		return nil, transient(err)
	}
	if len(status.Pending) == 0 {
		var lastApplied int64
		if len(status.Applied) > 0 {
			lastApplied = status.Applied[len(status.Applied)-1].ExecutedAt.Unix()
		}
		return &dbv1alpha1.AtlasMigrationStatus{
			LastApplied:        lastApplied,
			LastAppliedVersion: status.Current,
		}, nil
	}
	// Execute Atlas CLI migrate command
	report, err := c.MigrateApply(ctx, &atlas.MigrateApplyParams{
		Env: envName,
		Context: &atlas.DeployRunContext{
			TriggerType:    atlas.TriggerTypeKubernetes,
			TriggerVersion: dbv1alpha1.VersionFromContext(ctx),
		},
	})
	if err != nil {
		if !isSQLErr(err) {
			err = transient(err)
		}
		return nil, err
	}
	return &dbv1alpha1.AtlasMigrationStatus{
		LastApplied:        report.End.Unix(),
		LastAppliedVersion: report.Target,
	}, nil
}

// Extract migration data from the given resource
func (r *AtlasMigrationReconciler) extractData(ctx context.Context, res *dbv1alpha1.AtlasMigration) (_ *migrationData, err error) {
	var (
		s    = res.Spec
		data = &migrationData{
			EnvName:         defaultEnvName,
			RevisionsSchema: s.RevisionsSchema,
			Baseline:        s.Baseline,
			ExecOrder:       string(s.ExecOrder),
		}
	)
	if env := s.EnvName; env != "" {
		data.EnvName = env
	}
	if data.URL, err = s.DatabaseURL(ctx, r, res.Namespace); err != nil {
		return nil, transient(err)
	}
	switch d := s.Dir; {
	case d.Remote.Name != "":
		c := s.Cloud
		if c.TokenFrom.SecretKeyRef == nil {
			return nil, errors.New("cannot use remote directory without Atlas Cloud token")
		}
		token, err := getSecretValue(ctx, r, res.Namespace, c.TokenFrom.SecretKeyRef)
		if err != nil {
			return nil, err
		}
		data.Cloud = &cloud{
			Token:     token,
			Project:   c.Project,
			URL:       c.URL,
			RemoteDir: &d.Remote,
		}
	case d.ConfigMapRef != nil:
		if d.Local != nil {
			return nil, errors.New("cannot use both configmaps and local directory")
		}
		cfgMap, err := getConfigMap(ctx, r, res.Namespace, d.ConfigMapRef)
		if err != nil {
			return nil, err
		}
		data.Dir, err = memDir(cfgMap.Data)
		if err != nil {
			return nil, err
		}
	case d.Local != nil:
		data.Dir, err = memDir(d.Local)
		if err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("no directory specified")
	}
	return data, nil
}

func (r *AtlasMigrationReconciler) recordErrEvent(res *dbv1alpha1.AtlasMigration, err error) {
	reason := "Error"
	if isTransient(err) {
		reason = "TransientErr"
	}
	r.recorder.Event(res, corev1.EventTypeWarning, reason, strings.TrimSpace(err.Error()))
}

// Calculate the hash of the given data
func (d *migrationData) hash() (string, error) {
	h := sha256.New()
	h.Write([]byte(d.URL.String()))
	if c := d.Cloud; c != nil {
		h.Write([]byte(c.Token))
		h.Write([]byte(c.URL))
		h.Write([]byte(c.Project))
	}
	switch {
	case d.Cloud.hasRemoteDir():
		// Hash cloud directory
		h.Write([]byte(d.Cloud.RemoteDir.Name))
		h.Write([]byte(d.Cloud.RemoteDir.Tag))
	case d.Dir != nil:
		// Hash local directory
		hf, err := d.Dir.Checksum()
		if err != nil {
			return "", err
		}
		h.Write([]byte(hf.Sum()))
	default:
		return "", errors.New("migration data is empty")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (d *migrationData) DirURL() string {
	if d.Cloud.hasRemoteDir() {
		return fmt.Sprintf("atlas://%s?tag=%s", d.Cloud.RemoteDir.Name, d.Cloud.RemoteDir.Tag)
	}
	return "file://migrations"
}

// render renders the atlas.hcl template.
//
// The template is used by the Atlas CLI to apply the migrations directory.
// It also validates the data before rendering the template.
func (d *migrationData) render(w io.Writer) error {
	if d.URL == nil {
		return errors.New("database URL is empty")
	}
	switch {
	case d.Cloud.hasRemoteDir():
		if d.Dir != nil {
			return errors.New("cannot use both remote and local directory")
		}
		if d.Cloud.Token == "" {
			return errors.New("Atlas Cloud token is empty")
		}
	case d.Dir != nil:
	default:
		return errors.New("migration directory is empty")
	}
	return tmpl.ExecuteTemplate(w, "atlas_migration.tmpl", d)
}

// hasRemoteDir returns true if the given migration data has a remote directory
func (c *cloud) hasRemoteDir() bool {
	if c == nil {
		return false
	}
	return c.RemoteDir != nil && c.RemoteDir.Name != ""
}

func makeKeyLatest(resName string) string {
	// Inspired by the helm chart key format
	const storageKey = "io.atlasgo.db.v1"
	return fmt.Sprintf("%s.%s.latest", storageKey, resName)
}

func newSecretObject(key string, dir migrate.Dir, labels map[string]string) (*corev1.Secret, error) {
	const owner = "atlasgo.io"
	tar, err := migrate.ArchiveDir(dir)
	if err != nil {
		return nil, err
	}
	if labels == nil {
		labels = map[string]string{}
	}
	labels["owner"] = owner
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:   key,
			Labels: labels,
		},
		Type: "atlasgo.io/db.v1",
		Data: map[string][]byte{
			// k8s already encodes the tarball in base64
			// so we don't need to encode it again.
			"migrations.tar": tar,
		},
	}, nil
}
