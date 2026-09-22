// Copyright 2025 The Atlas Operator Authors.
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
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"ariga.io/atlas/atlasexec"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
)

//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasdriftchecks,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasdriftchecks/finalizers,verbs=update
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasdriftchecks/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasmigrations,verbs=get;list;watch

const (
	// defaultDriftInterval is used when no interval is set.
	defaultDriftInterval = 5 * time.Minute
	// defaultDriftTimeout is the timeout for a single check.
	defaultDriftTimeout = 5 * time.Minute
	// transientDriftRetry limits the retry delay for temporary failures.
	transientDriftRetry = time.Minute
	// busyDriftRetry is the retry delay when the target or database is busy.
	busyDriftRetry = 30 * time.Second
	// driftedCondition is the condition used to report drift.
	driftedCondition = "Drifted"
)

type (
	// AtlasDriftCheckReconciler reconciles an AtlasDriftCheck.
	//
	// It checks the target database for drift and reports the result in the
	// resource status. It does not change the database.
	AtlasDriftCheckReconciler struct {
		client.Client
		atlasClient AtlasExecFn
		recorder    record.EventRecorder
		// allowCustomConfig allows targets with a custom atlas.hcl config.
		allowCustomConfig bool
		// jitter spreads checks that use the same interval.
		jitter func(time.Duration) time.Duration
	}
	// driftRun holds the state of a single drift check.
	driftRun struct {
		r   *AtlasDriftCheckReconciler
		res *dbv1alpha1.AtlasDriftCheck
		log logr.Logger
		// Set by the steps as they run.
		target *dbv1alpha1.AtlasMigration
		data   *migrationData
		wd     *atlasexec.WorkingDir
		cli    AtlasExec
	}
	// checkError describes how a failed check should be reported.
	checkError struct {
		reason    string
		message   string
		permanent bool
	}
)

// errTargetBusy means the target is applying a migration.
var errTargetBusy = errors.New("migration apply in progress")

// errLocked means another client holds the database lock.
var errLocked = errors.New("acquiring database lock")

// Error implements the error interface.
func (e *checkError) Error() string { return e.message }

func NewAtlasDriftCheckReconciler(mgr Manager, _ bool) *AtlasDriftCheckReconciler {
	return &AtlasDriftCheckReconciler{
		Client:   mgr.GetClient(),
		recorder: mgr.GetEventRecorderFor("atlasdriftcheck-controller"),
		jitter:   func(d time.Duration) time.Duration { return wait.Jitter(d, 0.1) },
	}
}

// SetAtlasClient sets the Atlas client for the reconciler.
func (r *AtlasDriftCheckReconciler) SetAtlasClient(fn AtlasExecFn) {
	r.atlasClient = fn
}

// AllowCustomConfig allows the controller to check targets that use a custom
// atlas.hcl config.
func (r *AtlasDriftCheckReconciler) AllowCustomConfig() {
	r.allowCustomConfig = true
}

// SetupWithManager sets up the controller with the Manager.
func (r *AtlasDriftCheckReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: 2}).
		For(&dbv1alpha1.AtlasDriftCheck{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}

// Reconcile runs a drift check and schedules the next one. Failures are
// reported in the status and retried without the controller's error backoff.
func (r *AtlasDriftCheckReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var (
		log = ctrl.LoggerFrom(ctx)
		res = &dbv1alpha1.AtlasDriftCheck{}
	)
	if err := r.Get(ctx, req.NamespacedName, res); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	prev := res.Status.DeepCopy()
	defer func() {
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			latest := &dbv1alpha1.AtlasDriftCheck{}
			if err := r.Get(ctx, req.NamespacedName, latest); err != nil {
				return err
			}
			latest.Status = res.Status
			return r.Status().Update(ctx, latest)
		}); err != nil {
			log.Error(err, "failed to update resource status")
		}
	}()
	result := r.reconcile(ctx, res)
	r.recordTransitions(res, prev)
	return result, nil
}

// reconcile runs a drift check and updates its status and retry time.
func (r *AtlasDriftCheckReconciler) reconcile(ctx context.Context, res *dbv1alpha1.AtlasDriftCheck) ctrl.Result {
	if res.Spec.Suspend {
		res.SetSuspended()
		return ctrl.Result{}
	}
	s := &driftRun{
		r:   r,
		res: res,
		log: ctrl.Log.WithName("atlas_driftcheck.reconcile"),
	}
	defer s.close()
	interval := cmp.Or(res.Spec.Interval.Duration, defaultDriftInterval)
	err := s.run(ctx)
	e, isCheckErr := errors.AsType[*checkError](err)
	switch {
	case err == nil:
		// The check already updated the status.
		return ctrl.Result{RequeueAfter: r.jitter(interval)}
	case errors.Is(err, errTargetBusy):
		res.SetTargetNotReady(err.Error())
		return ctrl.Result{RequeueAfter: r.jitter(busyDriftRetry)}
	case errors.Is(err, errLocked):
		// Keep the previous result and retry soon.
		s.log.Info("database is locked by another client, skipping this check")
		return ctrl.Result{RequeueAfter: r.jitter(busyDriftRetry)}
	case isCheckErr && e.permanent:
		res.SetCheckFailed(e.reason, e.message, true)
		return ctrl.Result{RequeueAfter: r.jitter(interval)}
	case isCheckErr:
		res.SetCheckFailed(e.reason, e.message, false)
		return ctrl.Result{RequeueAfter: r.jitter(min(interval, transientDriftRetry))}
	default:
		res.SetCheckFailed(dbv1alpha1.ReasonCheckFailed, err.Error(), false)
		return ctrl.Result{RequeueAfter: r.jitter(min(interval, transientDriftRetry))}
	}
}

// run runs the steps of a single drift check.
func (s *driftRun) run(ctx context.Context) error {
	if err := s.loadTarget(ctx); err != nil {
		return err
	}
	if err := s.extractData(ctx); err != nil {
		return err
	}
	if err := s.workingDir(ctx); err != nil {
		return err
	}
	if err := s.atlasClient(ctx); err != nil {
		return err
	}
	if err := s.login(ctx); err != nil {
		return err
	}
	return s.check(ctx)
}

// close closes the working directory if one was created.
func (s *driftRun) close() {
	if s.wd != nil {
		s.wd.Close()
	}
}

// loadTarget reads the AtlasMigration being checked.
func (s *driftRun) loadTarget(ctx context.Context) error {
	target := &dbv1alpha1.AtlasMigration{}
	key := types.NamespacedName{Namespace: s.res.Namespace, Name: s.res.Spec.TargetRef.Name}
	switch err := s.r.Get(ctx, key, target); {
	case apierrors.IsNotFound(err):
		return &checkError{
			reason:    dbv1alpha1.ReasonTargetNotFound,
			permanent: true,
			message: fmt.Sprintf("AtlasMigration %q not found in namespace %q; note that an operator "+
				"installed with --label-selector only sees the targets carrying those labels", key.Name, key.Namespace),
		}
	case err != nil:
		return &checkError{reason: dbv1alpha1.ReasonCheckFailed, message: err.Error()}
	}
	// A stalled target is still checked because its database may have drifted.
	if target.IsReconciling() {
		return errTargetBusy
	}
	s.target = target
	return nil
}

// extractData reads the migration data and checks that the expected state can be resolved.
func (s *driftRun) extractData(ctx context.Context) error {
	data, err := extractMigrationData(ctx, s.r, s.target, s.r.allowCustomConfig)
	if err != nil {
		return &checkError{
			reason:    dbv1alpha1.ReasonCheckFailed,
			message:   err.Error(),
			permanent: !isTransient(err),
		}
	}
	// A remote directory contains the expected state. A local directory needs a dev database to build it.
	if !data.hasRemoteDir() && !data.hasConfigRepo() && !data.hasDevURL() {
		return &checkError{
			reason:    dbv1alpha1.ReasonCheckFailed,
			permanent: true,
			message: fmt.Sprintf("local migration directory requires a dev database to replay: "+
				"set spec.devURL or spec.devURLFrom on AtlasMigration/%s, or push the directory to the Atlas Registry",
				s.target.Name),
		}
	}
	s.data = data
	return nil
}

// workingDir writes the config and migrations to a temporary directory.
func (s *driftRun) workingDir(context.Context) error {
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

// atlasClient creates an Atlas client with its own HOME to avoid sharing the
// login token store with the migration controller.
func (s *driftRun) atlasClient(context.Context) error {
	cli, err := s.r.atlasClient(s.wd.Path(), s.data.Cloud, filepath.Join(s.res.Namespace, s.res.Name))
	if err != nil {
		return err
	}
	s.cli = cli
	return nil
}

// login logs in to Atlas Cloud if a token is set.
func (s *driftRun) login(ctx context.Context) error {
	if s.data.Cloud == nil || s.data.Cloud.Token == "" {
		return nil
	}
	return s.cli.Login(ctx, &atlasexec.LoginParams{Token: s.data.Cloud.Token, GrantOnly: true})
}

// check runs `atlas migrate drift` and records the result.
func (s *driftRun) check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(s.res.Spec.Timeout.Duration, defaultDriftTimeout))
	defer cancel()
	var stderr bytes.Buffer
	s.cli.SetStderr(&stderr)
	defer s.cli.SetStderr(nil)
	s.log.Info("checking for drift", "env", s.data.EnvName, "target", s.target.Name)
	reports, err := s.cli.MigrateDriftSlice(ctx, &atlasexec.MigrateDriftParams{
		Env:     s.data.EnvName,
		Vars:    s.data.Vars,
		DevURL:  s.data.DevURL,
		Exclude: s.exclude(),
	})
	// Some Atlas warnings are written to stderr without failing the command.
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		s.log.Info("atlas wrote to stderr", "stderr", msg)
	}
	if err != nil {
		return classifyDriftError(ctx, err)
	}
	if len(reports) != 1 {
		return &checkError{
			reason:    dbv1alpha1.ReasonCheckFailed,
			permanent: true,
			message:   fmt.Sprintf("unexpected number of reports: %d; for_each targets are not supported", len(reports)),
		}
	}
	if rep := reports[0]; rep.Drifted {
		s.res.SetDrifted(rep, cmp.Or(s.res.Spec.OnDrift, dbv1alpha1.DriftActionReport))
	} else {
		s.res.SetChecked(rep)
	}
	return nil
}

// exclude returns the objects to ignore. If none are set, it uses the target's drift policy.
func (s *driftRun) exclude() []string {
	if len(s.res.Spec.Exclude) > 0 {
		return s.res.Spec.Exclude
	}
	if p := s.target.Spec.Policy; p.HasDrift() {
		return p.Drift.Exclude
	}
	return nil
}

// permanentDriftErrors contains errors that will not change by retrying.
var permanentDriftErrors = []string{
	"no state found for version",
	"has no hash to resolve",
	"parsing expected state",
}

// classifyDriftError classifies a failed check. Unknown errors are treated as
// temporary so the check keeps retrying.
func classifyDriftError(ctx context.Context, err error) error {
	msg := cmp.Or(strings.TrimSpace(driftErrMessage(err)), "the drift check could not be completed")
	switch {
	case ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded):
		return &checkError{
			reason:  dbv1alpha1.ReasonCheckFailed,
			message: fmt.Sprintf("the drift check timed out: %s", msg),
		}
	case strings.Contains(msg, errLocked.Error()):
		return errLocked
	case errors.Is(err, atlasexec.ErrRequireLogin):
		return &checkError{reason: dbv1alpha1.ReasonCheckFailed, message: msg, permanent: true}
	case strings.Contains(msg, "no migration history found"):
		return &checkError{reason: dbv1alpha1.ReasonNoMigrationHistory, message: msg, permanent: true}
	}
	for _, p := range permanentDriftErrors {
		if strings.Contains(msg, p) {
			return &checkError{reason: dbv1alpha1.ReasonCheckFailed, message: msg, permanent: true}
		}
	}
	return &checkError{reason: dbv1alpha1.ReasonCheckFailed, message: msg}
}

// driftErrMessage returns the error message to report. Some early CLI failures
// are available only on stderr.
func driftErrMessage(err error) string {
	if e, ok := errors.AsType[*atlasexec.MigrateDriftError](err); ok {
		for _, r := range e.Result {
			if r.Error != "" {
				return r.Error
			}
		}
		if e.Stderr != "" {
			return e.Stderr
		}
	}
	return err.Error()
}

// recordTransitions emits events when drift or a check failure changes.
func (r *AtlasDriftCheckReconciler) recordTransitions(res *dbv1alpha1.AtlasDriftCheck, prev *dbv1alpha1.AtlasDriftCheckStatus) {
	var (
		was = meta.FindStatusCondition(prev.Conditions, driftedCondition)
		now = meta.FindStatusCondition(res.Status.Conditions, driftedCondition)
		// Unknown does not mean the drift was resolved.
		wasDrifted = was != nil && was.Status == metav1.ConditionTrue
		nowDrifted = now != nil && now.Status == metav1.ConditionTrue
	)
	switch {
	case !wasDrifted && nowDrifted:
		r.recorder.Event(res, corev1.EventTypeWarning, dbv1alpha1.ReasonDriftDetected, now.Message)
	case wasDrifted && nowDrifted && prev.Fingerprint != res.Status.Fingerprint:
		r.recorder.Event(res, corev1.EventTypeWarning, "DriftChanged", now.Message)
	case wasDrifted && now != nil && now.Status == metav1.ConditionFalse:
		r.recorder.Event(res, corev1.EventTypeNormal, "DriftResolved", now.Message)
	}
	readyNow := meta.FindStatusCondition(res.Status.Conditions, "Ready")
	if readyNow == nil || readyNow.Status != metav1.ConditionFalse ||
		// Drift failures already have an event.
		readyNow.Reason == dbv1alpha1.ReasonDriftDetected {
		return
	}
	readyWas := meta.FindStatusCondition(prev.Conditions, "Ready")
	if readyWas != nil && readyWas.Status == metav1.ConditionFalse &&
		readyWas.Reason == readyNow.Reason && readyWas.Message == readyNow.Message {
		// The same failure was already reported.
		return
	}
	r.recorder.Event(res, corev1.EventTypeWarning, dbv1alpha1.ReasonCheckFailed, readyNow.Message)
}
