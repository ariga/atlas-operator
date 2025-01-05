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

	"ariga.io/atlas-go-sdk/atlasexec"
	"ariga.io/atlas/sql/migrate"
	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
	"github.com/ariga/atlas-operator/internal/controller/watch"
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
		scheme           *runtime.Scheme
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
		if err := r.Status().Update(ctx, res); err != nil {
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
		res.SetNotReady("Reconciling", "Reconciling")
		return ctrl.Result{Requeue: true}, nil
	}
	data, err := r.extractData(ctx, res)
	if err != nil {
		var reason = "ReadingMigrationData"
		if e, ok := err.(interface{ Reason() string }); ok {
			reason = e.Reason()
		}
		res.SetNotReady(reason, err.Error())
		r.recordErrEvent(res, err)
		return result(err)
	}
	// We need to update the ready condition immediately before doing
	// any heavy jobs if the hash is different from the last applied.
	// This is to ensure that other tools know we are still applying the changes.
	if res.IsReady() && res.IsHashModified(data.ObservedHash) {
		res.SetNotReady("Reconciling", "Current migration data has changed")
		return ctrl.Result{Requeue: true}, nil
	}
	// ====================================================
	// Starting area to handle the heavy jobs.
	// Below this line is the main logic of the controller.
	// ====================================================

	// TODO(giautm): Create DevDB and run linter for new migration
	// files before applying it to the target database.
	if !data.hasDevURL() && data.URL != nil {
		// The user has not specified an URL for dev-db,
		// spin up a dev-db and get the connection string.
		data.DevURL, err = r.devDB.devURL(ctx, res, *data.URL)
		if err != nil {
			res.SetNotReady("GettingDevDB", err.Error())
			return result(err)
		}
	}
	// Reconcile given resource
	err = r.reconcile(ctx, data, res)
	if err != nil {
		r.recordErrEvent(res, err)
	}
	return result(err)
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

// Reconcile the given AtlasMigration resource.
func (r *AtlasMigrationReconciler) reconcile(ctx context.Context, data *migrationData, res *dbv1alpha1.AtlasMigration) error {
	log := ctrl.Log.WithName("atlas_migration.reconcile")
	// Create a working directory for the Atlas CLI
	// The working directory contains the atlas.hcl config
	// and the migrations directory (if any)
	wd, err := atlasexec.NewWorkingDir(
		atlasexec.WithAtlasHCL(data.render),
		atlasexec.WithMigrations(data.Dir),
	)
	if err != nil {
		res.SetNotReady("ReadingMigrationData", err.Error())
		return err
	}
	defer wd.Close()
	c, err := r.atlasClient(wd.Path(), data.Cloud)
	if err != nil {
		return err
	}
	var whoami *atlasexec.WhoAmI
	switch whoami, err = c.WhoAmI(ctx); {
	case errors.Is(err, atlasexec.ErrRequireLogin):
		log.Info("the resource is not connected to Atlas Cloud")
		if data.Config != nil {
			err = errors.New("login is required to use custom atlas.hcl config")
			res.SetNotReady("WhoAmI", err.Error())
			r.recordErrEvent(res, err)
			return err
		}
	case err != nil:
		res.SetNotReady("WhoAmI", err.Error())
		r.recordErrEvent(res, err)
		return err
	default:
		log.Info("the resource is connected to Atlas Cloud", "org", whoami.Org)
	}
	log.Info("reconciling migration", "env", data.EnvName)
	// Check if there are any pending migration files
	status, err := c.MigrateStatus(ctx, &atlasexec.MigrateStatusParams{Env: data.EnvName, Vars: data.Vars})
	if err != nil {
		res.SetNotReady("Migrating", err.Error())
		if isChecksumErr(err) {
			return err
		}
		return transient(err)
	}
	switch {
	case len(status.Pending) == 0 && len(status.Applied) > 0 && len(status.Available) < len(status.Applied):
		if !data.MigrateDown {
			res.SetNotReady("ProtectedFlowError", "Migrate down is not allowed")
			return &ProtectedFlowError{
				reason: "ProtectedFlowError",
				msg:    "migrate down is not allowed, set `migrateDown.allow` to true to allow downgrade",
			}
		}
		// The downgrade is allowed, apply the last migration version
		last := status.Available[len(status.Available)-1]
		log.Info("downgrading to the last available version", "version", last.Version)
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
			if err = wd.CopyFS(current, data.DirLatest); err != nil {
				return err
			}
			params.DirURL = fmt.Sprintf("file://%s", current)
		default:
			return fmt.Errorf("unable to downgrade, no dir-state found")
		}
		run, err := c.MigrateDown(ctx, params)
		if err != nil {
			res.SetNotReady("Migrating", err.Error())
			if !isSQLErr(err) {
				err = transient(err)
			}
			return err
		}
		switch run.Status {
		case StatePending:
			res.SetNotReady("ApprovalPending", "Deployment is waiting for approval")
			res.Status.ApprovalURL = run.URL
			return transient(&ProtectedFlowError{
				reason: "ApprovalPending",
				msg:    fmt.Sprintf("plan approval pending, review here: %s", run.URL),
			})
		case StateAborted:
			res.SetNotReady("PlanRejected", "Deployment is aborted")
			res.Status.ApprovalURL = run.URL
			// Migration is aborted, no need to reapply
			return fmt.Errorf("plan rejected, review here: %s", run.URL)
		case StateApplied, StateApproved:
			res.SetReady(dbv1alpha1.AtlasMigrationStatus{
				ObservedHash:       data.ObservedHash,
				ApprovalURL:        run.URL,
				LastApplied:        run.Start.Unix(),
				LastAppliedVersion: run.Target,
				LastDeploymentURL:  run.URL,
			})
			r.recordApplied(res, run.Target)
		}
	case len(status.Pending) == 0:
		log.Info("no pending migrations")
		// No pending migrations
		var lastApplied int64
		if len(status.Applied) > 0 {
			lastApplied = status.Applied[len(status.Applied)-1].ExecutedAt.Unix()
		}
		res.SetReady(dbv1alpha1.AtlasMigrationStatus{
			ObservedHash:       data.ObservedHash,
			LastApplied:        lastApplied,
			LastAppliedVersion: status.Current,
		})
		r.recordApplied(res, status.Current)
	default:
		log.Info("applying pending migrations", "count", len(status.Pending))
		// There are pending migrations
		// Execute Atlas CLI migrate command
		reports, err := c.MigrateApplySlice(ctx, &atlasexec.MigrateApplyParams{
			Env: data.EnvName,
			Context: &atlasexec.DeployRunContext{
				TriggerType:    atlasexec.TriggerTypeKubernetes,
				TriggerVersion: dbv1alpha1.VersionFromContext(ctx),
			},
			Vars: data.Vars,
		})
		if err != nil {
			res.SetNotReady("Migrating", err.Error())
			if !isSQLErr(err) {
				err = transient(err)
			}
			return err
		}
		if len(reports) != 1 {
			return fmt.Errorf("unexpected number of reports: %d", len(reports))
		}
		res.SetReady(dbv1alpha1.AtlasMigrationStatus{
			ObservedHash:       data.ObservedHash,
			LastApplied:        reports[0].End.Unix(),
			LastAppliedVersion: reports[0].Target,
		})
		r.recordApplied(res, reports[0].Target)
	}
	if data.Dir != nil {
		// Compress the migration directory then store it in the secret
		// for later use when atlas runs the migration down.
		if err = r.storeDirState(ctx, res, data.Dir); err != nil {
			res.SetNotReady("StoringDirState", err.Error())
			return err
		}
	}
	return nil
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
		mergeBlocks(d.Config.Body(), f.Body())
		f = d.Config
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
