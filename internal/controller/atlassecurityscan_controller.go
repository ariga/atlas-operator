// Copyright 2026 The Atlas Operator Authors.
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
	"time"
	// The released image is built on Alpine, which ships no tzdata, so the zone
	// database is embedded rather than read from the filesystem.
	_ "time/tzdata"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"ariga.io/atlas/atlasexec"
	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
	"github.com/ariga/atlas-operator/internal/controller/watch"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/robfig/cron/v3"
	"github.com/zclconf/go-cty/cty"
)

//+kubebuilder:rbac:groups=core,resources=configmaps;secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasmigrations;atlasschemas,verbs=get;list;watch
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlassecurityscans,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlassecurityscans/finalizers,verbs=update
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlassecurityscans/status,verbs=get;update;patch

type (
	// AtlasSecurityScanReconciler reconciles an AtlasSecurityScan object
	AtlasSecurityScanReconciler struct {
		client.Client
		atlasClient      AtlasExecFn
		secretWatcher    *watch.ResourceWatcher
		schemaWatcher    *watch.ResourceWatcher
		migrationWatcher *watch.ResourceWatcher
		recorder         record.EventRecorder
		// AllowCustomConfig allows the controller to use custom atlas.hcl config.
		allowCustomConfig bool
		watchSecrets      bool
	}
	// scanData is the input of a scan: the database to scan, and what to report from it.
	scanData struct {
		EnvName     string
		URL         *url.URL
		Cloud       *Cloud
		Config      *hclwrite.File
		Vars        atlasexec.Vars2
		MinSeverity string
		FailOn      string
		Ignore      []string
		Schedule    cron.Schedule
		TriggerOn   []dbv1alpha1.TriggerRef
	}
)

func NewAtlasSecurityScanReconciler(mgr Manager) *AtlasSecurityScanReconciler {
	return &AtlasSecurityScanReconciler{
		Client:           mgr.GetClient(),
		secretWatcher:    watch.New(),
		schemaWatcher:    watch.New(),
		migrationWatcher: watch.New(),
		recorder:         mgr.GetEventRecorderFor("atlassecurityscan-controller"),
	}
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *AtlasSecurityScanReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, err error) {
	var (
		log = log.FromContext(ctx)
		res = &dbv1alpha1.AtlasSecurityScan{}
	)
	if err = r.Get(ctx, req.NamespacedName, res); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	defer func() {
		if uerr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			latest := &dbv1alpha1.AtlasSecurityScan{}
			if err := r.Get(ctx, req.NamespacedName, latest); err != nil {
				return err
			}
			latest.Status = res.Status
			return r.Status().Update(ctx, latest)
		}); uerr != nil {
			// A scan with no interval is not requeued, so a status left unwritten
			// stays stale until the resource or a referenced Secret changes. Fail
			// the reconcile instead, and let the controller retry it.
			err = errors.Join(err, fmt.Errorf("updating resource status: %w", uerr))
		}
		// After updating the status, watch the dependent resources
		r.watchRefs(res)
	}()
	// When the resource is first created, create the "Ready" condition. The real
	// work waits for the next pass so observers see the resource is being handled.
	if len(res.Status.Conditions) == 0 {
		res.SetReconciling("Reconciling")
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	data, err := r.extractData(ctx, res)
	if se := (*scheduleError)(nil); errors.As(err, &se) {
		// Retrying cannot parse an expression that does not parse.
		res.SetNotReady(dbv1alpha1.ReasonInvalidSchedule, err.Error())
		r.recordErrEvent(res, err)
		return ctrl.Result{}, nil
	} else if err != nil {
		return r.resultErr(res, err, dbv1alpha1.ReasonReadingScanData)
	}
	hash := data.hash()
	trigger, due := r.scanDue(ctx, res, data, hash)
	res.Status.NextScanTime = data.nextScanTime(res.Status.LastScanTime)
	if !due {
		return nextScan(res), nil
	}
	// Create a working directory for the Atlas CLI
	// The working directory contains the atlas.hcl config.
	wd, err := atlasexec.NewWorkingDir(atlasexec.WithAtlasHCL(data.render))
	if err != nil {
		return r.resultErr(res, err, dbv1alpha1.ReasonCreatingWorkingDir)
	}
	defer wd.Close()
	cli, err := r.atlasClient(wd.Path(), data.Cloud, filepath.Join(res.Namespace, res.Name))
	if err != nil {
		return r.resultErr(res, err, dbv1alpha1.ReasonCreatingAtlasClient)
	}
	if data.Cloud != nil && data.Cloud.Token != "" {
		if err := cli.Login(ctx, &atlasexec.LoginParams{Token: data.Cloud.Token, GrantOnly: true}); err != nil {
			return r.resultErr(res, err, dbv1alpha1.ReasonLogin)
		}
	}
	log.Info("scanning the database for security issues", "env", data.EnvName)
	scan, err := cli.SecurityScan(ctx, &atlasexec.SecurityScanParams{
		Env:         data.EnvName,
		Vars:        data.Vars,
		MinSeverity: data.MinSeverity,
		FailOn:      data.FailOn,
		Ignore:      data.Ignore,
	})
	// The command prints its report before failing on it, or on a step that
	// follows it. e.g., a notify block. A nil report means it printed none.
	if scan == nil {
		return r.resultCLIErr(res, err, dbv1alpha1.ReasonScanning)
	}
	recordScan(res, scan, hash, trigger)
	res.Status.NextScanTime = data.nextScanTime(res.Status.LastScanTime)
	summary := scanSummary(scan)
	switch {
	// Anything but a failing result failed the command itself.
	case err != nil && !errors.Is(err, atlasexec.ErrSecurityScan):
		return r.resultCLIErr(res, err, dbv1alpha1.ReasonScanning)
	// A database that was not scanned leaves its state unknown, which is the one
	// case where the scan itself did not do its job.
	case len(scan.Failures()) > 0:
		var msgs []string
		for _, t := range scan.Failures() {
			msgs = append(msgs, fmt.Sprintf("%s: %s", t.URL, t.Error))
		}
		return r.resultErr(res, errors.New(strings.Join(msgs, "\n")), dbv1alpha1.ReasonScanning)
	}
	// The scan ran, so the resource is ready whatever it found. Only the verdict
	// on failOn belongs to the Secure condition, and only when one is set.
	res.SetReady(summary)
	if data.FailOn != "" {
		res.SetSecure(err == nil, summary)
	}
	switch {
	case err != nil:
		r.recorder.Event(res, corev1.EventTypeWarning, dbv1alpha1.ReasonSecurityIssues, summary)
	case scan.Count() > 0:
		r.recorder.Event(res, corev1.EventTypeWarning, dbv1alpha1.ReasonSecurityIssues, summary)
	default:
		r.recorder.Event(res, corev1.EventTypeNormal, dbv1alpha1.ReasonScanned, summary)
	}
	log.Info("scanned the database for security issues", "issues", scan.Count(), "trigger", trigger)
	return nextScan(res), nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AtlasSecurityScanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: runtime.NumCPU(),
		}).
		For(&dbv1alpha1.AtlasSecurityScan{}, builder.WithPredicates(predicate.GenerationChangedPredicate{}))
	if r.watchSecrets {
		b = b.Watches(&corev1.Secret{}, r.secretWatcher)
	}
	// Unfiltered on purpose: an apply shows up as a status write, which the
	// generation predicate on the scan itself would drop.
	return b.
		Watches(&dbv1alpha1.AtlasSchema{}, r.schemaWatcher).
		Watches(&dbv1alpha1.AtlasMigration{}, r.migrationWatcher).
		Complete(r)
}

// WatchSecrets enables the Secret informer so the controller re-reconciles
// when a referenced Secret changes. When disabled, the controller skips the
// cluster-wide Secret LIST/WATCH, avoiding RBAC errors in environments that
// provide credentials through other means (e.g. file-based injection).
func (r *AtlasSecurityScanReconciler) WatchSecrets() {
	r.watchSecrets = true
}

func (r *AtlasSecurityScanReconciler) watchRefs(res *dbv1alpha1.AtlasSecurityScan) {
	for _, s := range []*corev1.SecretKeySelector{
		res.Spec.Cloud.TokenFrom.SecretKeyRef,
		res.Spec.URLFrom.SecretKeyRef,
		res.Spec.Credentials.PasswordFrom.SecretKeyRef,
		res.Spec.ConfigFrom.SecretKeyRef,
	} {
		if s != nil {
			r.secretWatcher.Watch(
				types.NamespacedName{Name: s.Name, Namespace: res.Namespace},
				res.NamespacedName(),
			)
		}
	}
	for _, t := range res.Spec.TriggerOn {
		if w := r.triggerWatcher(t.Kind); w != nil {
			w.Watch(
				types.NamespacedName{Name: t.Name, Namespace: res.Namespace},
				res.NamespacedName(),
			)
		}
	}
}

// triggerWatcher returns the watcher for a trigger kind, nil for an unknown one.
func (r *AtlasSecurityScanReconciler) triggerWatcher(kind string) *watch.ResourceWatcher {
	switch kind {
	case "AtlasSchema":
		return r.schemaWatcher
	case "AtlasMigration":
		return r.migrationWatcher
	default:
		return nil
	}
}

// scanDue reports whether a scan should run, and what caused it. The order is
// most specific first, so the reported trigger is the one a reader would name.
func (r *AtlasSecurityScanReconciler) scanDue(
	ctx context.Context, res *dbv1alpha1.AtlasSecurityScan, data *scanData, hash string,
) (dbv1alpha1.ScanTrigger, bool) {
	last := res.Status.LastScanTime
	switch {
	case last == nil, res.IsHashModified(hash):
		return dbv1alpha1.TriggerSpecChange, true
	// A scan that could not run leaves the database unknown, so retry it.
	case !res.IsReady():
		return res.Status.LastScanTrigger, true
	}
	if r.appliedSince(ctx, res, last.Time) {
		return dbv1alpha1.TriggerChange, true
	}
	if n := data.nextScanTime(last); n != nil && !n.Time.After(time.Now()) {
		return dbv1alpha1.TriggerSchedule, true
	}
	return "", false
}

// appliedSince reports whether any watched resource applied after the given time.
// A trigger that does not exist is a configuration mistake, not a scan failure.
//
// The apply timestamps have second granularity while the scan end does not, so an
// apply landing in the same second as a scan is treated as covered by it. Widening
// that would risk a resource whose scan finishes inside one second re-triggering
// itself forever, and the schedule catches the case anyway.
func (r *AtlasSecurityScanReconciler) appliedSince(
	ctx context.Context, res *dbv1alpha1.AtlasSecurityScan, since time.Time,
) bool {
	for _, t := range res.Spec.TriggerOn {
		key := types.NamespacedName{Name: t.Name, Namespace: res.Namespace}
		var applied int64
		switch t.Kind {
		case "AtlasSchema":
			o := &dbv1alpha1.AtlasSchema{}
			if err := r.Get(ctx, key, o); err != nil {
				r.recordTriggerErr(res, t, err)
				continue
			}
			applied = o.Status.LastApplied
		case "AtlasMigration":
			o := &dbv1alpha1.AtlasMigration{}
			if err := r.Get(ctx, key, o); err != nil {
				r.recordTriggerErr(res, t, err)
				continue
			}
			applied = o.Status.LastApplied
		}
		if applied > 0 && time.Unix(applied, 0).After(since) {
			return true
		}
	}
	return false
}

func (r *AtlasSecurityScanReconciler) recordTriggerErr(res *dbv1alpha1.AtlasSecurityScan, t dbv1alpha1.TriggerRef, err error) {
	r.recorder.Eventf(res, corev1.EventTypeWarning, "TriggerNotFound",
		"Cannot read %s %q: %v", t.Kind, t.Name, err)
}

// SetAtlasClient sets the Atlas client function.
func (r *AtlasSecurityScanReconciler) SetAtlasClient(fn AtlasExecFn) {
	r.atlasClient = fn
}

// AllowCustomConfig allows the controller to use custom atlas.hcl config.
func (r *AtlasSecurityScanReconciler) AllowCustomConfig() {
	r.allowCustomConfig = true
}

// extractData extracts the input of the scan from the resource.
func (r *AtlasSecurityScanReconciler) extractData(ctx context.Context, res *dbv1alpha1.AtlasSecurityScan) (_ *scanData, err error) {
	var (
		s    = res.Spec
		data = &scanData{
			EnvName:     defaultEnvName,
			MinSeverity: string(s.MinSeverity),
			FailOn:      string(s.FailOn),
			Ignore:      s.Ignore,
			TriggerOn:   s.TriggerOn,
		}
	)
	if data.Schedule, err = parseSchedule(s.Schedule, s.TimeZone); err != nil {
		return nil, err
	}
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
	if s := s.EnvName; s != "" {
		data.EnvName = s
	}
	if s := s.Cloud.TokenFrom.SecretKeyRef; s != nil {
		token, err := getSecretValue(ctx, r, res.Namespace, s)
		if err != nil {
			return nil, err
		}
		data.Cloud = &Cloud{Token: token}
	}
	data.URL, err = s.DatabaseURL(ctx, r, res.Namespace)
	if err != nil {
		return nil, transient(err)
	}
	if !hasConfig && data.URL == nil {
		return nil, transient(errors.New("no target database defined"))
	}
	data.Vars, err = s.GetVars(ctx, r, res.Namespace)
	if err != nil {
		return nil, transient(err)
	}
	return data, nil
}

// recordScan stores the report of the scan in the status of the resource.
func recordScan(res *dbv1alpha1.AtlasSecurityScan, scan *atlasexec.SecurityScan, hash string, trigger dbv1alpha1.ScanTrigger) {
	s := &res.Status
	s.ObservedHash = hash
	s.LastScanTime = &metav1.Time{Time: scan.End}
	s.LastScanTrigger = trigger
	s.Issues = scan.Count()
	s.Levels = nil
	for _, l := range scan.Levels() {
		s.Levels = append(s.Levels, dbv1alpha1.SecurityScanLevel{Level: l.Level, Count: l.Count})
	}
	s.Targets = make([]dbv1alpha1.SecurityScanTarget, len(scan.Targets))
	for i, t := range scan.Targets {
		target := dbv1alpha1.SecurityScanTarget{
			URL:        t.URL,
			Driver:     t.Driver,
			Version:    t.Version,
			Extensions: t.Extensions,
			Error:      t.Error,
		}
		for _, v := range t.Vulnerabilities {
			target.Vulnerabilities = append(target.Vulnerabilities, dbv1alpha1.SecurityVulnerability{
				ID:         v.ID,
				Name:       v.Name,
				Version:    v.Version,
				Level:      strings.ToUpper(v.Level),
				Severity:   strings.ToUpper(v.Severity),
				Title:      v.Title,
				Suggestion: v.Suggestion,
			})
		}
		s.Targets[i] = target
	}
}

// scanSummary describes the result of the scan the way the CLI report does.
// e.g., "3 issues found: 1 high, 2 elevated".
func scanSummary(scan *atlasexec.SecurityScan) string {
	if n := scan.Count(); n > 0 {
		levels := scan.Levels()
		counts := make([]string, len(levels))
		for i, l := range levels {
			counts[i] = fmt.Sprintf("%d %s", l.Count, strings.ToLower(l.Level))
		}
		return fmt.Sprintf("%d %s found: %s", n, plural(n, "issue"), strings.Join(counts, ", "))
	}
	var n int
	for _, t := range scan.Targets {
		n += len(t.Extensions)
	}
	return fmt.Sprintf("no issues found in %d %s", n, plural(n, "extension"))
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// nextScan asks to be woken when the next scheduled window is due. It is a hint,
// not the schedule itself: the reconcile that follows re-derives whether a scan is
// due, so being woken early, late, or by an unrelated event is all fine.
func nextScan(res *dbv1alpha1.AtlasSecurityScan) ctrl.Result {
	n := res.Status.NextScanTime
	if n == nil {
		return ctrl.Result{}
	}
	if d := time.Until(n.Time); d > 0 {
		return ctrl.Result{RequeueAfter: d}
	}
	// The window came round again while the scan was running, which a schedule
	// finer than the scan takes can do. Come back promptly rather than not at all.
	return ctrl.Result{RequeueAfter: time.Second}
}

// scheduleError is a schedule or time zone that cannot be parsed.
type scheduleError struct{ err error }

func (e *scheduleError) Error() string { return e.err.Error() }
func (e *scheduleError) Unwrap() error { return e.err }

// parseSchedule reads the cron expression in the given time zone. Both are
// optional, and an empty expression means the resource has no schedule.
func parseSchedule(expr, tz string) (cron.Schedule, error) {
	if expr == "" {
		return nil, nil
	}
	loc := time.UTC
	if tz != "" {
		var err error
		if loc, err = time.LoadLocation(tz); err != nil {
			return nil, &scheduleError{fmt.Errorf("invalid timeZone %q: %w", tz, err)}
		}
	}
	s, err := cron.ParseStandard(expr)
	if err != nil {
		return nil, &scheduleError{fmt.Errorf("invalid schedule %q: %w", expr, err)}
	}
	return &inLocation{Schedule: s, loc: loc}, nil
}

// inLocation evaluates a schedule in a fixed time zone, whatever zone the time it
// is given carries.
type inLocation struct {
	cron.Schedule
	loc *time.Location
}

func (s *inLocation) Next(t time.Time) time.Time {
	return s.Schedule.Next(t.In(s.loc))
}

// nextScanTime is when the schedule is next due after the given scan, nil when the
// resource has no schedule. A window missed while the operator was down is returned
// as-is, in the past, so it is caught up with a single scan.
func (d *scanData) nextScanTime(last *metav1.Time) *metav1.Time {
	if d.Schedule == nil {
		return nil
	}
	from := time.Now()
	if last != nil {
		from = last.Time
	}
	return &metav1.Time{Time: d.Schedule.Next(from)}
}

func (r *AtlasSecurityScanReconciler) recordErrEvent(res *dbv1alpha1.AtlasSecurityScan, err error) {
	reason := "Error"
	if isTransient(err) {
		reason = "TransientErr"
	}
	r.recorder.Event(res, corev1.EventTypeWarning, reason, strings.TrimSpace(err.Error()))
}

// resultErr reports a transient error. It is retried with a backoff until the
// limit is reached, after which the resource waits for its next scan.
func (r *AtlasSecurityScanReconciler) resultErr(
	res *dbv1alpha1.AtlasSecurityScan, err error, reason string,
) (ctrl.Result, error) {
	err = transient(err)
	res.SetNotReady(reason, err.Error())
	r.recordErrEvent(res, err)
	if res.IsExceedBackoffLimit() {
		r.recorder.Event(res, corev1.EventTypeWarning, "BackoffLimitExceeded", "backoff limit exceeded")
		return nextScan(res), nil
	}
	return result(err, backoffDelayAt(res.Status.Failed))
}

// resultCLIErr reports an error of the Atlas CLI. It is not retried, as the
// error is in the input rather than transient; the resource waits for its next scan.
func (r *AtlasSecurityScanReconciler) resultCLIErr(
	res *dbv1alpha1.AtlasSecurityScan, err error, reason string,
) (ctrl.Result, error) {
	res.SetNotReady(reason, err.Error())
	r.recordErrEvent(res, err)
	return nextScan(res), nil
}

// hash identifies the input of the scan, to tell if it changed since the last one.
func (d *scanData) hash() string {
	fields := []string{d.EnvName, d.MinSeverity, d.FailOn, strings.Join(d.Ignore, ",")}
	if d.URL != nil {
		fields = append(fields, d.URL.String())
	}
	if d.Config != nil {
		fields = append(fields, string(d.Config.Bytes()))
	}
	if d.Cloud != nil {
		fields = append(fields, d.Cloud.Token)
	}
	for k, v := range mapsSorted(d.Vars) {
		fields = append(fields, fmt.Sprintf("%s=%v", k, v))
	}
	h := sha256.Sum256([]byte(strings.Join(fields, "\n")))
	return hex.EncodeToString(h[:])
}

// render renders the atlas.hcl file the scan runs with.
func (d *scanData) render(w io.Writer) error {
	f := hclwrite.NewFile()
	if c := d.Cloud; c != nil && c.Token != "" {
		atlas := f.Body().AppendNewBlock("atlas", nil)
		atlas.Body().AppendNewBlock("cloud", nil).Body().
			SetAttributeValue("token", cty.StringVal(c.Token))
	}
	env := f.Body().AppendNewBlock("env", []string{d.EnvName})
	if d.URL != nil {
		env.Body().SetAttributeValue("url", cty.StringVal(d.URL.String()))
	}
	// Merge config into the atlas.hcl file.
	if d.Config != nil {
		mergeBlocks(f.Body(), d.Config.Body(), d.EnvName)
	}
	env = searchBlock(f.Body(), hclwrite.NewBlock("env", []string{d.EnvName}))
	if env == nil {
		return fmt.Errorf("env block %q is not found", d.EnvName)
	}
	if env.Body().GetAttribute("url") == nil {
		return errors.New("database url is not set")
	}
	_, err := f.WriteTo(w)
	return err
}
