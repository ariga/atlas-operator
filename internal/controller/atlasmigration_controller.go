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

package controller

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"ariga.io/atlas/atlasexec"
	"ariga.io/atlas/sql/migrate"
	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
	"github.com/ariga/atlas-operator/internal/controller/watch"
	"github.com/go-logr/logr"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
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
		scheme           *k8sruntime.Scheme
		atlasClient      AtlasExecFn
		configMapWatcher *watch.ResourceWatcher
		secretWatcher    *watch.ResourceWatcher
		recorder         record.EventRecorder
		devDB            *devDBReconciler
		// AllowCustomConfig allows the controller to use custom atlas.hcl config.
		allowCustomConfig bool
	}
	// migrationData is the data used to render the HCL template
	// that will be used for Atlas CLI
	migrationData struct {
		EnvName         string
		URL             *url.URL
		DevURL          string
		Dir             migrate.Dir
		DirLatest       migrate.Dir
		Cloud           *Cloud
		RevisionsSchema string
		Baseline        string
		ExecOrder       string
		MigrateDown     bool
		ObservedHash    string
		RemoteDir       *dbv1alpha1.Remote
		Config          *hclwrite.File
		Vars            atlasexec.Vars2
	}
)

func NewAtlasMigrationReconciler(mgr Manager, prewarmDevDB bool) *AtlasMigrationReconciler {
	r := mgr.GetEventRecorderFor("atlasmigration-controller")
	return &AtlasMigrationReconciler{
		Client:           mgr.GetClient(),
		scheme:           mgr.GetScheme(),
		configMapWatcher: watch.New(),
		secretWatcher:    watch.New(),
		recorder:         r,
		devDB:            newDevDB(mgr, r, prewarmDevDB),
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
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	defer func() {
		// At the end of reconcile, update the status of the resource base on the error
		if err != nil {
			r.recordErrEvent(res, err)
		}
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			latest := &dbv1alpha1.AtlasMigration{}
			if err := r.Get(ctx, req.NamespacedName, latest); err != nil {
				return err
			}
			latest.Status = res.Status
			return r.Status().Update(ctx, latest)
		}); err != nil {
			log.Error(err, "failed to update resource status")
		}
		// After updating the status, watch the dependent resources
		r.watchRefs(res)
		// Clean up any resources created by the controller after the reconciler is successful.
		if res.IsReady() {
			r.devDB.cleanUp(ctx, res)
		}
	}()
	// When the resource is first created, create the "Ready" condition.
	if len(res.Status.Conditions) == 0 {
		res.SetReconciling("Reconciling")
		return ctrl.Result{Requeue: true}, nil
	}
	// Reconcile given resource
	return r.reconcile(ctx, res)
}

func (r *AtlasMigrationReconciler) readDirState(ctx context.Context, obj client.Object) (migrate.Dir, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      makeKeyLatest(obj.GetName()),
			Namespace: obj.GetNamespace(),
		},
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	return extractDirFromSecret(secret)
}

func (r *AtlasMigrationReconciler) storeDirState(ctx context.Context, obj client.Object, dir migrate.Dir) error {
	var labels = make(map[string]string, len(obj.GetLabels())+1)
	for k, v := range obj.GetLabels() {
		labels[k] = v
	}
	labels["name"] = obj.GetName()
	secret, err := newSecretObject(obj, dir, labels)
	if err != nil {
		return err
	}
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

// SetupWithManager sets up the controller with the Manager.
func (r *AtlasMigrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: runtime.NumCPU(),
		}).
		For(&dbv1alpha1.AtlasMigration{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&dbv1alpha1.AtlasMigration{}).
		Watches(&corev1.Secret{}, r.secretWatcher).
		Watches(&corev1.ConfigMap{}, r.configMapWatcher).
		Complete(r)
}

// SetAtlasClient sets the Atlas client for the reconciler.
func (r *AtlasMigrationReconciler) SetAtlasClient(fn AtlasExecFn) {
	r.atlasClient = fn
}

// AllowCustomConfig allows the controller to use custom atlas.hcl config.
func (r *AtlasMigrationReconciler) AllowCustomConfig() {
	r.allowCustomConfig = true
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

const (
	StatePending  = "PENDING_USER"
	StateApproved = "APPROVED"
	StateAborted  = "ABORTED"
	StateApplied  = "APPLIED"
)

type (
	// migrationRun holds the state shared between the steps of a single
	// reconciliation of an AtlasMigration resource.
	migrationRun struct {
		r   *AtlasMigrationReconciler
		res *dbv1alpha1.AtlasMigration
		log logr.Logger
		// Populated by the steps as they run.
		data   *migrationData
		wd     *atlasexec.WorkingDir
		cli    AtlasExec
		status *atlasexec.MigrateStatus
	}
)

// Reconcile the given AtlasMigration resource.
func (r *AtlasMigrationReconciler) reconcile(ctx context.Context, res *dbv1alpha1.AtlasMigration) (ctrl.Result, error) {
	s := &migrationRun{
		r:   r,
		res: res,
		log: ctrl.Log.WithName("atlas_migration.reconcile"),
	}
	defer s.close()

	if err := s.extractData(ctx); err != nil {
		if errors.Is(err, errRequeue) {
			return ctrl.Result{Requeue: true}, nil
		}
		return r.resultErr(res, err, dbv1alpha1.ReasonReadingMigrationData)
	}

	// Build the working directory and Atlas client without a dev database, so
	// we can run `migrate status` first. When there are no pending migrations,
	// the loop exits early and avoids the cost of spinning up a dev database.
	if err := s.workingDir(ctx); err != nil {
		return r.resultErr(res, err, dbv1alpha1.ReasonReadingMigrationData)
	}

	if err := s.atlasClient(ctx); err != nil {
		return r.resultErr(res, err, dbv1alpha1.ReasonCreatingAtlasClient)
	}

	if err := s.login(ctx); err != nil {
		return r.resultErr(res, err, dbv1alpha1.ReasonLogin)
	}

	if err := s.whoAmI(ctx); err != nil {
		return r.resultErr(res, err, dbv1alpha1.ReasonWhoAmI)
	}

	if err := s.migrateStatus(ctx); err != nil {
		return r.resultErr(res, err, dbv1alpha1.ReasonMigrating)
	}

	if err := s.applyChanges(ctx); err != nil {
		if p, ok := errors.AsType[*pendingError](err); ok {
			return r.resultPending(res, p.reason, p.message)
		}
		return r.resultErr(res, err, dbv1alpha1.ReasonMigrating)
	}

	if err := s.storeDirState(ctx); err != nil {
		return r.resultErr(res, err, dbv1alpha1.ReasonStoringDirState)
	}

	return ctrl.Result{}, nil
}

// close releases the working directory, if one was created.
func (s *migrationRun) close() {
	if s.wd != nil {
		s.wd.Close()
	}
}

// extractData extracts the migration data from the resource.
func (s *migrationRun) extractData(ctx context.Context) error {
	data, err := s.r.extractData(ctx, s.res)
	if err != nil {
		return err
	}
	// We need to update the ready condition immediately before doing
	// any heavy jobs if the hash is different from the last applied.
	// This is to ensure that other tools know we are still applying the changes.
	if s.res.IsReady() && s.res.IsHashModified(data.ObservedHash) {
		s.res.SetReconciling("Current migration data has changed")
		return errRequeue
	}
	s.data = data
	return nil
}

// devDB spins up a dev database for the migration, if needed.
//
// TODO(giautm): Create DevDB and run linter for new migration
// files before applying it to the target database.
func (s *migrationRun) devDB(ctx context.Context) error {
	data, res := s.data, s.res
	var err error
	switch {
	case data.URL == nil:
		// The user has not specified a URL for the schema, so no dev database is needed.
		return nil
	case res.Spec.DevDB != nil:
		// The user has provided a custom dev database configuration. spin it up.
		data.DevURL, err = s.r.devDB.devURL(ctx, res, *data.URL, &res.Spec.DevDB.Spec, data.DevURL)
	case !data.hasDevURL():
		// The user has not provided a custom dev database configuration. spin it up a dev-db to get the connection string.
		data.DevURL, err = s.r.devDB.devURL(ctx, res, *data.URL, nil, data.DevURL)
	}
	if err != nil {
		return &pendingError{reason: dbv1alpha1.ReasonGettingDevDB, message: err.Error()}
	}
	return nil
}

// workingDir creates a working directory for the Atlas CLI.
// The working directory contains the atlas.hcl config
// and the migrations directory (if any). At this point the config is rendered
// without a dev database URL; prepareDevDB re-renders it once a dev database
// is spun up.
func (s *migrationRun) workingDir(context.Context) error {
	wd, err := atlasexec.NewWorkingDir(
		atlasexec.WithAtlasHCL(s.data.render),
		atlasexec.WithMigrations(s.data.Dir),
	)
	if err != nil {
		return err
	}
	s.wd = wd
	return nil
}

// atlasClient creates the Atlas client for the working directory.
func (s *migrationRun) atlasClient(context.Context) error {
	cli, err := s.r.atlasClient(s.wd.Path(), s.data.Cloud, filepath.Join(s.res.Namespace, s.res.Name))
	if err != nil {
		return err
	}
	s.cli = cli
	return nil
}

// login logs in to Atlas Cloud, when a token is provided.
func (s *migrationRun) login(ctx context.Context) error {
	if s.data.Cloud == nil || s.data.Cloud.Token == "" {
		return nil
	}
	return s.cli.Login(ctx, &atlasexec.LoginParams{Token: s.data.Cloud.Token, GrantOnly: true})
}

// whoAmI verifies the connection to Atlas Cloud.
func (s *migrationRun) whoAmI(ctx context.Context) error {
	switch whoami, err := s.cli.WhoAmI(ctx, &atlasexec.WhoAmIParams{Vars: s.data.Vars}); {
	case errors.Is(err, atlasexec.ErrRequireLogin):
		s.log.Info("the resource is not connected to Atlas Cloud")
		if s.data.Config != nil {
			return errors.New("login is required to use custom atlas.hcl config")
		}
		return nil
	case err != nil:
		return err
	default:
		s.log.Info("the resource is connected to Atlas Cloud", "org", whoami.Org)
		return nil
	}
}

// migrateStatus runs `migrate status` to check whether there are any pending
// migration files. It does not require a dev database, so it can run before
// one is created.
func (s *migrationRun) migrateStatus(ctx context.Context) error {
	s.log.Info("reconciling migration", "env", s.data.EnvName)
	status, err := s.cli.MigrateStatus(ctx, &atlasexec.MigrateStatusParams{Env: s.data.EnvName, Vars: s.data.Vars})
	if err != nil {
		return err
	}
	s.status = status
	return nil
}

// applyChanges applies the pending migration files on the target database, or
// migrates the database down when it is ahead of the migration directory. When
// there are no changes to apply, it marks the resource as ready without
// spinning up a dev database.
func (s *migrationRun) applyChanges(ctx context.Context) error {
	switch status := s.status; {
	case len(status.Pending) == 0 && len(status.Applied) > 0 && len(status.Available) < len(status.Applied):
		if err := s.prepareDevDB(ctx); err != nil {
			return err
		}
		return s.migrateDown(ctx)
	case len(status.Pending) == 0:
		s.noPendingChanges()
		return nil
	default:
		if err := s.prepareDevDB(ctx); err != nil {
			return err
		}
		return s.migrateApply(ctx)
	}
}

// prepareDevDB spins up the dev database (when needed) and re-renders the
// atlas.hcl config so it includes the dev database URL. This is deferred until
// we know there is work to do, to avoid the cost of a dev database when there
// are no pending migrations.
func (s *migrationRun) prepareDevDB(ctx context.Context) error {
	if err := s.devDB(ctx); err != nil {
		return err
	}
	// Re-render atlas.hcl now that the dev database URL is known.
	// "atlas.hcl" is the file name used by atlasexec.WithAtlasHCL.
	return s.wd.CreateFile("atlas.hcl", s.data.render)
}

// migrateDown migrates the database down to the last available version.
func (s *migrationRun) migrateDown(ctx context.Context) error {
	data, res, status := s.data, s.res, s.status
	if !data.MigrateDown {
		return &ProtectedFlowError{
			reason: "ProtectedFlowError",
			msg:    "migrate down is not allowed, set `migrateDown.allow` to true to allow downgrade",
		}
	}
	// The downgrade is allowed, apply the last migration version
	last := status.Available[len(status.Available)-1]
	s.log.Info("downgrading to the last available version", "version", last.Version)
	params := &atlasexec.MigrateDownParams{
		Env:       data.EnvName,
		ToVersion: last.Version,
		Context: &atlasexec.DeployRunContext{
			TriggerType:    atlasexec.TriggerTypeKubernetes,
			TriggerVersion: dbv1alpha1.VersionFromContext(ctx),
		},
		Vars: data.Vars,
	}
	// Atlas needs all versions to be present in the directory
	// to downgrade to a specific version.
	switch {
	case data.Cloud != nil && data.RemoteDir != nil:
		// Use the `latest` tag of the remote directory to fetch all versions.
		params.DirURL = fmt.Sprintf("atlas://%s", data.RemoteDir.Name)
	case data.DirLatest != nil:
		// Copy the dir-state from latest deployment to the different location
		// (to avoid the conflict with the current migration directory)
		// then use it to downgrade.
		current := fmt.Sprintf("migrations-%s", status.Current)
		if err := s.wd.CopyFS(current, data.DirLatest); err != nil {
			return &reasonedError{err: err, reason: "CopyingDirState"}
		}
		params.DirURL = fmt.Sprintf("file://%s", current)
	default:
		return errors.New("unable to downgrade, no dir-state found")
	}
	run, err := s.cli.MigrateDown(ctx, params)
	if err != nil {
		return err
	}
	switch run.Status {
	case StatePending:
		res.Status.ApprovalURL = run.URL
		return &pendingError{
			reason:  dbv1alpha1.ReasonApprovalPending,
			message: fmt.Sprintf("plan approval pending, review here: %s", run.URL),
		}
	case StateAborted:
		res.Status.ApprovalURL = run.URL
		// Migration is aborted, no need to reapply
		return &reasonedError{
			err:    fmt.Errorf("plan rejected, review here: %s", run.URL),
			reason: "PlanRejected",
		}
	case StateApplied, StateApproved:
		res.SetReady(dbv1alpha1.AtlasMigrationStatus{
			ObservedHash:       data.ObservedHash,
			ApprovalURL:        run.URL,
			LastApplied:        run.Start.Unix(),
			LastAppliedVersion: run.Target,
			LastDeploymentURL:  run.URL,
		})
		s.r.recordApplied(res, run.Target)
	}
	return nil
}

// noPendingChanges marks the resource as ready when
// there are no pending migrations.
func (s *migrationRun) noPendingChanges() {
	s.log.Info("no pending migrations")
	// No pending migrations
	var lastApplied int64
	if len(s.status.Applied) > 0 {
		lastApplied = s.status.Applied[len(s.status.Applied)-1].ExecutedAt.Unix()
	}
	s.res.SetReady(dbv1alpha1.AtlasMigrationStatus{
		ObservedHash:       s.data.ObservedHash,
		LastApplied:        lastApplied,
		LastAppliedVersion: s.status.Current,
	})
	s.r.recordApplied(s.res, s.status.Current)
}

// migrateApply executes the pending migration files on the target database.
func (s *migrationRun) migrateApply(ctx context.Context) error {
	s.log.Info("applying pending migrations", "count", len(s.status.Pending))
	var stderr bytes.Buffer
	s.cli.SetStderr(&stderr)
	// There are pending migrations
	// Execute Atlas CLI migrate command
	reports, err := s.cli.MigrateApplySlice(ctx, &atlasexec.MigrateApplyParams{
		Env: s.data.EnvName,
		Context: &atlasexec.DeployRunContext{
			TriggerType:    atlasexec.TriggerTypeKubernetes,
			TriggerVersion: dbv1alpha1.VersionFromContext(ctx),
		},
		Vars: s.data.Vars,
	})
	if err != nil {
		return err
	}
	if len(reports) != 1 {
		return fmt.Errorf("unexpected number of reports: %d", len(reports))
	}
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		// In some cases, Atlas logs to stderr without returning a nonzero status code. Emit the message to the user.
		s.r.recorder.Event(s.res, corev1.EventTypeWarning, "Migrating", msg)
	}
	s.cli.SetStderr(nil)
	s.res.SetReady(dbv1alpha1.AtlasMigrationStatus{
		ObservedHash:       s.data.ObservedHash,
		LastApplied:        reports[0].End.Unix(),
		LastAppliedVersion: reports[0].Target,
	})
	s.r.recordApplied(s.res, reports[0].Target)
	return nil
}

// storeDirState compresses the migration directory then stores it in the
// secret for later use when atlas runs the migration down.
func (s *migrationRun) storeDirState(ctx context.Context) error {
	if s.data.Dir == nil {
		return nil
	}
	return s.r.storeDirState(ctx, s.res, s.data.Dir)
}

type ProtectedFlowError struct {
	reason string
	msg    string
}

// Error implements the error interface
func (e *ProtectedFlowError) Error() string {
	return e.msg
}

// Reason returns the reason of the error
func (e *ProtectedFlowError) Reason() string {
	return e.reason
}

// errRequeue stops the reconciliation and requeues the resource immediately,
// without reporting an error.
var errRequeue = errors.New("requeue")

type (
	// pendingError stops the reconciliation and reports the resource as waiting
	// for an external action (e.g. plan approval) via resultPending.
	pendingError struct {
		reason  string
		message string
	}
	// reasonedError overrides the default failure reason of the step
	// returning it. It intentionally has no Unwrap method, so recordErrEvent
	// keeps classifying the error as transient.
	reasonedError struct {
		err    error
		reason string
	}
)

// Error implements the error interface
func (e *pendingError) Error() string {
	return e.message
}

// Error implements the error interface
func (e *reasonedError) Error() string {
	return e.err.Error()
}

// Reason returns the reason of the error
func (e *reasonedError) Reason() string {
	return e.reason
}

// Extract migration data from the given resource
func (r *AtlasMigrationReconciler) extractData(ctx context.Context, res *dbv1alpha1.AtlasMigration) (_ *migrationData, err error) {
	var (
		s    = res.Spec
		data = &migrationData{
			EnvName:         defaultEnvName,
			DevURL:          s.DevURL,
			RevisionsSchema: s.RevisionsSchema,
			Baseline:        s.Baseline,
			ExecOrder:       string(s.ExecOrder),
			MigrateDown:     false,
		}
	)
	data.Config, err = s.GetConfig(ctx, r, res.Namespace)
	if err != nil {
		return nil, transient(err)
	}
	hasConfig := data.Config != nil
	if hasConfig {
		if !r.allowCustomConfig {
			return nil, errors.New("install the operator with \"--set allowCustomConfig=true\" to use custom atlas.hcl config")
		}
		if s.EnvName == "" {
			return nil, errors.New("env name must be set when using custom atlas.hcl config")
		}
	}
	if env := s.EnvName; env != "" {
		data.EnvName = env
	}
	if data.URL, err = s.DatabaseURL(ctx, r, res.Namespace); err != nil {
		return nil, transient(err)
	}
	if !hasConfig && data.URL == nil {
		return nil, transient(errors.New("no target database defined"))
	}
	if s := s.Cloud.TokenFrom.SecretKeyRef; s != nil {
		token, err := getSecretValue(ctx, r, res.Namespace, s)
		if err != nil {
			return nil, err
		}
		data.Cloud = &Cloud{Token: token}
	}
	if s.Cloud.Project != "" || s.Cloud.URL != "" {
		if data.Cloud == nil {
			data.Cloud = &Cloud{}
		}
		data.Cloud.Repo = s.Cloud.Project
		data.Cloud.URL = s.Cloud.URL
	}
	switch d := s.Dir; {
	case d.Remote.Name != "":
		c := s.Cloud
		if c.TokenFrom.SecretKeyRef == nil && !hasConfig {
			return nil, errors.New("cannot use remote directory without Atlas Cloud token")
		}
		if f := s.ProtectedFlows; f != nil {
			if d := f.MigrateDown; d != nil {
				if d.Allow && d.AutoApprove {
					return nil, &ProtectedFlowError{"ProtectedFlowError", "autoApprove is not allowed for a remote directory"}
				}
				data.MigrateDown = d.Allow
			}
		}
		data.RemoteDir = &d.Remote
	case d.Local != nil || d.ConfigMapRef != nil:
		if d.Local != nil && d.ConfigMapRef != nil {
			return nil, errors.New("cannot use both configmaps and local directory")
		}
		if f := s.ProtectedFlows; f != nil {
			if d := f.MigrateDown; d != nil {
				if d.Allow && !d.AutoApprove {
					return nil, &ProtectedFlowError{"ProtectedFlowError", "allow cannot be true without autoApprove for local migration directory"}
				}
				// Allow migrate-down only if the flow is allowed and auto-approved
				data.MigrateDown = d.Allow && d.AutoApprove
			}
		}
		files := d.Local
		if files == nil {
			cfgMap, err := getConfigMap(ctx, r, res.Namespace, d.ConfigMapRef)
			if err != nil {
				return nil, err
			}
			files = cfgMap.Data
		}
		data.Dir, err = memDir(files)
		if err != nil {
			return nil, err
		}
		data.DirLatest, err = r.readDirState(ctx, res)
		if err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("no directory specified")
	}
	if s := s.DevURLFrom.SecretKeyRef; s != nil {
		// SecretKeyRef is set, get the secret value
		// then override the dev url.
		data.DevURL, err = getSecretValue(ctx, r, res.Namespace, s)
		if err != nil {
			return nil, err
		}
	}
	data.ObservedHash, err = hashMigrationData(data)
	if err != nil {
		return nil, err
	}
	data.Vars, err = s.GetVars(ctx, r, res.Namespace)
	if err != nil {
		return nil, transient(err)
	}
	return data, nil
}

func (r *AtlasMigrationReconciler) recordApplied(res *dbv1alpha1.AtlasMigration, ver string) {
	r.recorder.Eventf(res, corev1.EventTypeNormal, "Applied", "Version %s applied", ver)
}

func (r *AtlasMigrationReconciler) recordErrEvent(res *dbv1alpha1.AtlasMigration, err error) {
	reason := "Error"
	switch e := (&ProtectedFlowError{}); {
	case errors.As(err, &e):
		reason = e.Reason()
	case isTransient(err):
		reason = "TransientErr"
	}
	r.recorder.Event(res, corev1.EventTypeWarning, reason, strings.TrimSpace(err.Error()))
}

func (r *AtlasMigrationReconciler) resultErr(
	res *dbv1alpha1.AtlasMigration, err error, reason string,
) (ctrl.Result, error) {
	if e, ok := err.(interface{ Reason() string }); ok {
		reason = e.Reason()
	}
	err = transient(err)
	res.SetNotReady(reason, err.Error())
	r.recordErrEvent(res, err)
	if res.IsExceedBackoffLimit() {
		r.recorder.Event(res, corev1.EventTypeWarning, "BackoffLimitExceeded", "backoff limit exceeded")
		return result(err, 0)
	}
	return result(err, backoffDelayAt(res.Status.Failed))
}

func (r *AtlasMigrationReconciler) resultPending(
	res *dbv1alpha1.AtlasMigration, reason, message string) (ctrl.Result, error) {
	res.SetNotReady(reason, message)
	r.recorder.Event(res, corev1.EventTypeWarning, reason, message)
	return ctrl.Result{
		RequeueAfter: retryDuration,
	}, nil
}

// Calculate the hash of the given data
func hashMigrationData(d *migrationData) (string, error) {
	h := sha256.New()
	if d.URL != nil {
		h.Write([]byte(d.URL.String()))
	}
	if d.Config != nil {
		h.Write([]byte(d.Config.Bytes()))
	}
	if c := d.Cloud; c != nil {
		h.Write([]byte(c.Token))
		h.Write([]byte(c.URL))
		h.Write([]byte(c.Repo))
	}
	switch {
	case d.hasRemoteDir():
		// Hash cloud directory
		h.Write([]byte(d.RemoteDir.Name))
		h.Write([]byte(d.RemoteDir.Tag))
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
	if d.hasRemoteDir() {
		return fmt.Sprintf("atlas://%s?tag=%s", d.RemoteDir.Name, d.RemoteDir.Tag)
	}
	return "file://migrations"
}

// render renders the atlas.hcl template.
//
// The template is used by the Atlas CLI to apply the migrations directory.
// It also validates the data before rendering the template.
func (d *migrationData) render(w io.Writer) error {
	f := hclwrite.NewFile()
	for _, b := range d.asBlocks() {
		f.Body().AppendBlock(b)
	}
	// Merge the config block if it is set
	if d.Config != nil {
		mergeBlocks(f.Body(), d.Config.Body(), d.EnvName)
	}
	env := searchBlock(f.Body(), hclwrite.NewBlock("env", []string{d.EnvName}))
	if env == nil {
		return fmt.Errorf("env block %q is not found", d.EnvName)
	}
	b := env.Body()
	if b.GetAttribute("url") == nil {
		return errors.New("database URL is empty")
	}
	migrationblock := searchBlock(b, hclwrite.NewBlock("migration", nil))
	var dirAttr *hclwrite.Attribute
	if migrationblock != nil {
		dirAttr = migrationblock.Body().GetAttribute("dir")
	}
	switch {
	case d.hasRemoteDir():
		dirURL := dirAttr.Expr().BuildTokens(nil).Bytes()
		if dirAttr != nil && !strings.Contains(string(dirURL), d.DirURL()) {
			return errors.New("cannot use both remote and local directory")
		}
		cloudBlock := searchBlock(f.Body(), hclwrite.NewBlock("atlas", nil))
		if cloudBlock == nil {
			if cloudBlock.Body().GetAttribute("token") == nil {
				return errors.New("Atlas Cloud token is empty")
			}
		}
	case dirAttr != nil:
	default:
		return errors.New("migration directory is empty")
	}
	if _, err := f.WriteTo(w); err != nil {
		return err
	}
	return nil
}

// hasRemoteDir returns true if the given migration data has a remote directory
func (c *migrationData) hasRemoteDir() bool {
	if c == nil {
		return false
	}
	return c.RemoteDir != nil && c.RemoteDir.Name != ""
}

// hasDevURL returns true if the given migration data has a dev URL
func (d *migrationData) hasDevURL() bool {
	if d.DevURL != "" {
		return true
	}
	if d.Config == nil {
		return false
	}
	env := searchBlock(d.Config.Body(), hclwrite.NewBlock("env", []string{d.EnvName}))
	if env != nil {
		dev := env.Body().GetAttribute("dev")
		if dev != nil {
			return true
		}
	}
	return false
}

// asBlocks returns the HCL blocks for the given migration data
func (d *migrationData) asBlocks() []*hclwrite.Block {
	var blocks []*hclwrite.Block
	if d.Cloud != nil {
		atlas := hclwrite.NewBlock("atlas", nil)
		cloud := atlas.Body().AppendNewBlock("cloud", nil).Body()
		if d.Cloud.Token != "" {
			cloud.SetAttributeValue("token", cty.StringVal(d.Cloud.Token))
		}
		if d.Cloud.URL != "" {
			cloud.SetAttributeValue("url", cty.StringVal(d.Cloud.URL))
		}
		if d.Cloud.Repo != "" {
			cloud.SetAttributeValue("project", cty.StringVal(d.Cloud.Repo))
		}
		blocks = append(blocks, atlas)
	}
	env := hclwrite.NewBlock("env", []string{d.EnvName})
	blocks = append(blocks, env)
	envBody := env.Body()
	if d.URL != nil {
		envBody.SetAttributeValue("url", cty.StringVal(d.URL.String()))
	}
	if d.DevURL != "" {
		envBody.SetAttributeValue("dev", cty.StringVal(d.DevURL))
	}
	migration := hclwrite.NewBlock("migration", nil)
	envBody.AppendBlock(migration)
	// env.migration
	migrationBody := migration.Body()
	migrationBody.SetAttributeValue("dir", cty.StringVal(d.DirURL()))
	if d.ExecOrder != "" {
		migrationBody.SetAttributeValue("exec_order", cty.StringVal(d.ExecOrder))
	}
	if d.Baseline != "" {
		migrationBody.SetAttributeValue("baseline", cty.StringVal(d.Baseline))
	}
	if d.RevisionsSchema != "" {
		migrationBody.SetAttributeValue("revisions_schema", cty.StringVal(d.RevisionsSchema))
	}
	return blocks
}

func makeKeyLatest(resName string) string {
	// Inspired by the helm chart key format
	const storageKey = "io.atlasgo.db.v1"
	return fmt.Sprintf("%s.%s.latest", storageKey, resName)
}

func newSecretObject(obj client.Object, dir migrate.Dir, labels map[string]string) (*corev1.Secret, error) {
	const owner = "atlasgo.io"
	if labels == nil {
		labels = map[string]string{}
	}
	labels["owner"] = owner
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if err := migrate.ArchiveDirTo(w, dir); err != nil {
		return nil, err
	}
	// Close the gzip writer to flush the buffer
	if err := w.Close(); err != nil {
		return nil, err
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      makeKeyLatest(obj.GetName()),
			Namespace: obj.GetNamespace(),
			Labels:    labels,
			OwnerReferences: []metav1.OwnerReference{
				// Set the owner reference to the given object
				// This will ensure that the secret is deleted when the owner is deleted.
				*metav1.NewControllerRef(obj, obj.GetObjectKind().GroupVersionKind()),
			},
		},
		Type: "atlasgo.io/db.v1",
		Data: map[string][]byte{
			// k8s already encodes the tarball in base64
			// so we don't need to encode it again.
			"migrations.tar.gz": buf.Bytes(),
		},
	}, nil
}

func extractDirFromSecret(sec *corev1.Secret) (migrate.Dir, error) {
	if sec.Type != "atlasgo.io/db.v1" {
		return nil, fmt.Errorf("invalid secret type, got %q", sec.Type)
	}
	tarball, ok := sec.Data["migrations.tar.gz"]
	if !ok {
		return nil, errors.New("migrations.tar.gz not found")
	}
	r, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return nil, err
	}
	return migrate.UnarchiveDirFrom(r)
}
