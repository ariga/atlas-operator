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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"ariga.io/atlas/sql/migrate"
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

//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasmigrations,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasmigrations/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasmigrations/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch

type (
	// CLI is the interface used to interact with Atlas CLI
	MigrateCLI interface {
		Apply(ctx context.Context, data *atlas.ApplyParams) (*atlas.ApplyReport, error)
		Status(ctx context.Context, data *atlas.StatusParams) (*atlas.StatusReport, error)
	}
	// AtlasMigrationReconciler reconciles a AtlasMigration object
	AtlasMigrationReconciler struct {
		client.Client
		cli              MigrateCLI
		scheme           *runtime.Scheme
		secretWatcher    *watch.ResourceWatcher
		configMapWatcher *watch.ResourceWatcher
		recorder         record.EventRecorder
	}
	// migrationData is the data used to render the HCL template
	// that will be used for Atlas CLI
	migrationData struct {
		EnvName         string
		URL             string
		Migration       *migration
		Cloud           *cloud
		RevisionsSchema string
	}
	migration struct {
		Dir string
	}
	cloud struct {
		URL       string
		Token     string
		Project   string
		RemoteDir *remoteDir
	}
	remoteDir struct {
		Name string
		Tag  string
	}
)

func NewAtlasMigrationReconciler(mgr manager.Manager, execPath string) *AtlasMigrationReconciler {
	return &AtlasMigrationReconciler{
		Client:           mgr.GetClient(),
		scheme:           mgr.GetScheme(),
		cli:              atlas.NewClientWithPath(execPath),
		configMapWatcher: watch.New(),
		secretWatcher:    watch.New(),
		recorder:         mgr.GetEventRecorderFor("atlasmigration-controller"),
	}
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *AtlasMigrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, err error) {
	var (
		log = log.FromContext(ctx)
		res = &dbv1alpha1.AtlasMigration{}
	)
	if err := r.Get(ctx, req.NamespacedName, res); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// At the end of reconcile, update the status of the resource base on the error
	defer func() {
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
	// Extract migration data from the given resource
	data, cleanUp, err := r.extractData(ctx, res)
	if err != nil {
		res.SetNotReady("ReadingMigrationData", err.Error())
		r.recordErrEvent(res, err)
		return result(err)
	}
	defer cleanUp()
	hash, err := data.hash()
	if err != nil {
		res.SetNotReady("CalculatingHash", err.Error())
		return ctrl.Result{}, nil
	}
	// If the migration resource has changed and the resource ready condition is not false, immediately set it to false.
	// This is done so that the observed status of the migration reflects its "in-progress" state while it is being
	// reconciled.
	if res.IsReady() && res.IsHashModified(hash) {
		res.SetNotReady("Reconciling", "Current migration data has changed")
		return ctrl.Result{Requeue: true}, nil
	}
	// Reconcile given resource
	status, err := r.reconcile(ctx, data)
	if err != nil {
		res.SetNotReady("Migrating", strings.TrimSpace(err.Error()))
		r.recordErrEvent(res, err)
		return result(err)
	}
	r.recorder.Eventf(res, corev1.EventTypeNormal, "Applied", "Version %s applied", status.LastAppliedVersion)
	res.SetReady(status)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AtlasMigrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbv1alpha1.AtlasMigration{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&dbv1alpha1.AtlasMigration{}).
		Watches(&source.Kind{Type: &corev1.Secret{}}, r.secretWatcher).
		Watches(&source.Kind{Type: &corev1.ConfigMap{}}, r.configMapWatcher).
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
func (r *AtlasMigrationReconciler) reconcile(ctx context.Context, data *migrationData) (dbv1alpha1.AtlasMigrationStatus, error) {
	// Create atlas.hcl from template data
	atlasHCL, cleanUp, err := data.render()
	if err != nil {
		return dbv1alpha1.AtlasMigrationStatus{}, err
	}
	defer cleanUp()

	// Calculate the observedHash
	hash, err := data.hash()
	if err != nil {
		return dbv1alpha1.AtlasMigrationStatus{}, err
	}

	// Check if there are any pending migration files
	status, err := r.cli.Status(ctx, &atlas.StatusParams{Env: data.EnvName, ConfigURL: atlasHCL})
	if err != nil {
		return dbv1alpha1.AtlasMigrationStatus{}, transient(err)
	}
	if len(status.Pending) == 0 {
		var lastApplied int64
		if len(status.Applied) > 0 {
			lastApplied = status.Applied[len(status.Applied)-1].ExecutedAt.Unix()
		}
		return dbv1alpha1.AtlasMigrationStatus{
			ObservedHash:       hash,
			LastApplied:        lastApplied,
			LastAppliedVersion: status.Current,
		}, nil
	}

	// Execute Atlas CLI migrate command
	report, err := r.cli.Apply(ctx, &atlas.ApplyParams{Env: data.EnvName, ConfigURL: atlasHCL})
	if err != nil {
		return dbv1alpha1.AtlasMigrationStatus{}, transient(err)
	}
	if report != nil && report.Error != "" {
		err = errors.New(report.Error)
		if !isSQLErr(err) {
			err = transient(err)
		}
		return dbv1alpha1.AtlasMigrationStatus{}, err
	}
	return dbv1alpha1.AtlasMigrationStatus{
		ObservedHash:       hash,
		LastApplied:        report.End.Unix(),
		LastAppliedVersion: report.Target,
	}, nil
}

// Extract migration data from the given resource
func (r *AtlasMigrationReconciler) extractData(ctx context.Context, res *dbv1alpha1.AtlasMigration) (*migrationData, func() error, error) {
	// Get database connection string
	u, err := res.Spec.DatabaseURL(ctx, r, res.Namespace)
	if err != nil {
		return nil, nil, transient(err)
	}
	tmplData := &migrationData{
		URL: u.String(),
	}
	// Get temporary directory
	cleanUpDir := func() error { return nil }
	if c := res.Spec.Dir.ConfigMapRef; c != nil {
		tmplData.Migration = &migration{}
		tmplData.Migration.Dir, cleanUpDir, err = r.createTmpDirFromCfgMap(ctx, res.Namespace, c.Name)
		if err != nil {
			return nil, nil, err
		}
	}

	// Get temporary directory in case of local directory
	if m := res.Spec.Dir.Local; m != nil {
		if tmplData.Migration != nil {
			return nil, nil, errors.New("cannot define both configmap and local directory")
		}

		tmplData.Migration = &migration{}
		tmplData.Migration.Dir, cleanUpDir, err = r.createTmpDirFromMap(ctx, m)
		if err != nil {
			return nil, nil, err
		}
	}

	// Get Atlas Cloud Token from secret
	if res.Spec.Cloud.TokenFrom.SecretKeyRef != nil {
		tmplData.Cloud = &cloud{
			URL:     res.Spec.Cloud.URL,
			Project: res.Spec.Cloud.Project,
		}

		if res.Spec.Dir.Remote.Name != "" {
			tmplData.Cloud.RemoteDir = &remoteDir{
				Name: res.Spec.Dir.Remote.Name,
				Tag:  res.Spec.Dir.Remote.Tag,
			}
		}

		tmplData.Cloud.Token, err = getSecretValue(ctx, r, res.Namespace, *res.Spec.Cloud.TokenFrom.SecretKeyRef)
		if err != nil {
			return nil, nil, err
		}
	}

	// Mapping EnvName, default to "kubernetes"
	tmplData.EnvName = res.Spec.EnvName
	if tmplData.EnvName == "" {
		tmplData.EnvName = "kubernetes"
	}

	tmplData.RevisionsSchema = res.Spec.RevisionsSchema
	return tmplData, cleanUpDir, nil
}

func (r *AtlasMigrationReconciler) recordErrEvent(res *dbv1alpha1.AtlasMigration, err error) {
	reason := "Error"
	if isTransient(err) {
		reason = "TransientErr"
	}
	r.recorder.Event(res, corev1.EventTypeWarning, reason, strings.TrimSpace(err.Error()))
}

// createTmpDirFromCM creates a temporary directory by configmap
func (r *AtlasMigrationReconciler) createTmpDirFromCfgMap(
	ctx context.Context,
	ns, cfgName string,
) (string, func() error, error) {

	// Get configmap
	configMap := corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: ns,
		Name:      cfgName,
	}, &configMap); err != nil {
		return "", nil, transient(err)
	}

	return r.createTmpDirFromMap(ctx, configMap.Data)
}

// createTmpDirFromCM creates a temporary directory by configmap
func (r *AtlasMigrationReconciler) createTmpDirFromMap(
	ctx context.Context,
	m map[string]string,
) (string, func() error, error) {

	// Create temporary directory and remove it at the end of the function
	tmpDir, err := os.MkdirTemp("", "migrations")
	if err != nil {
		return "", nil, err
	}

	// Foreach configmap to build temporary directory
	// key is the name of the file and value is the content of the file
	for key, value := range m {
		filePath := filepath.Join(tmpDir, key)
		err := os.WriteFile(filePath, []byte(value), 0644)
		if err != nil {
			// Remove the temporary directory if there is an error
			os.RemoveAll(tmpDir)
			return "", nil, err
		}
	}

	return fmt.Sprintf("file://%s", tmpDir), func() error {
		return os.RemoveAll(tmpDir)
	}, nil
}

// Render atlas.hcl file from the given data
func (d *migrationData) render() (string, func() error, error) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "atlas_migration.tmpl", d); err != nil {
		return "", nil, err
	}

	return atlas.TempFile(buf.String(), "hcl")
}

// Calculate the hash of the given data
func (d *migrationData) hash() (string, error) {
	h := sha256.New()

	// Hash cloud directory
	h.Write([]byte(d.URL))
	if d.Cloud != nil {
		h.Write([]byte(d.Cloud.Token))
		h.Write([]byte(d.Cloud.URL))
		h.Write([]byte(d.Cloud.Project))
		if d.Cloud.RemoteDir != nil {
			h.Write([]byte(d.Cloud.RemoteDir.Name))
			h.Write([]byte(d.Cloud.RemoteDir.Tag))
			return hex.EncodeToString(h.Sum(nil)), nil
		}
	}

	// Hash local directory
	if d.Migration != nil {
		u, err := url.Parse(d.Migration.Dir)
		if err != nil {
			return "", err
		}
		d, err := migrate.NewLocalDir(u.Path)
		if err != nil {
			return "", err
		}
		hf, err := d.Checksum()
		if err != nil {
			return "", err
		}
		h.Write([]byte(hf.Sum()))
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}
