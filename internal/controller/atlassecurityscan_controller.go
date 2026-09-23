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
	"maps"
	"net/url"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
	// The released image is built on Alpine, which ships no tzdata, so the zone
	// database is embedded rather than read from the filesystem.
	_ "time/tzdata"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"ariga.io/atlas/atlasexec"
	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/robfig/cron/v3"
	"github.com/zclconf/go-cty/cty"
)

//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlassecurityscans,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlassecurityscans/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlassecurityscans/finalizers,verbs=update
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlassecurityreports,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasschemas;atlasmigrations,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=secrets;configmaps,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

const (
	// triggerIndex indexes scans by the "<Kind>/<name>" of each trigger, so an
	// apply fans out to exactly the scans that reference the resource.
	triggerIndex = ".spec.triggers"
	// scanTimeout bounds one CLI run.
	scanTimeout = 10 * time.Minute
	// maxSlotSteps caps the search for missed schedule slots, as the CronJob
	// controller does: a bad clock could otherwise make it run for decades.
	maxSlotSteps = 100000
	// descriptionLimit bounds a vulnerability description in the report.
	descriptionLimit = 1024
)

type (
	// AtlasSecurityScanReconciler reconciles an AtlasSecurityScan object.
	AtlasSecurityScanReconciler struct {
		client.Client
		// reader bypasses the cache, which is label-scoped and would miss a report
		// left by another operator instance or an older version.
		reader      client.Reader
		scheme      *k8sruntime.Scheme
		atlasClient AtlasExecFn
		recorder    record.EventRecorder
		// AllowCustomConfig allows the controller to use custom atlas.hcl config.
		allowCustomConfig bool
		now               func() time.Time
	}
	// scanData is the input of the CLI run: the database and how to report on it.
	scanData struct {
		EnvName     string
		URL         *url.URL
		Cloud       *Cloud
		Config      *hclwrite.File
		Vars        atlasexec.Vars2
		MinSeverity string
	}
	// scanSnapshot is everything that can make a scan due, observed when the scan
	// starts and committed to status only when it succeeds.
	scanSnapshot struct {
		start       time.Time
		generation  int64
		requestedAt string
		triggers    []dbv1alpha1.ObservedTrigger
		slot        *time.Time
		waivers     []string
		hash        string
	}
	// scanSchedule evaluates a cron schedule in a fixed time zone, whatever zone
	// the time it is given carries.
	scanSchedule struct {
		cron.Schedule
		loc *time.Location
	}
	// permanentError is an input no scan can fix. The resource stalls on it.
	permanentError struct {
		reason, message string
	}
)

func (e *permanentError) Error() string { return e.message }

func (s *scanSchedule) Next(t time.Time) time.Time {
	return s.Schedule.Next(t.In(s.loc))
}

func NewAtlasSecurityScanReconciler(mgr Manager) *AtlasSecurityScanReconciler {
	return &AtlasSecurityScanReconciler{
		Client:   mgr.GetClient(),
		reader:   mgr.GetAPIReader(),
		scheme:   mgr.GetScheme(),
		recorder: mgr.GetEventRecorderFor("atlassecurityscan-controller"),
		now:      time.Now,
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
	// A write that fails is returned, so the workqueue retries it: a resource with
	// no schedule is otherwise never woken, and its status would stay stale.
	defer func() {
		if uerr := r.writeStatus(ctx, res); uerr != nil {
			err = errors.Join(err, uerr)
		}
	}()
	// When the resource is first created, create the conditions. The real work
	// waits for the next pass so observers see the resource is being handled.
	if len(res.Status.Conditions) == 0 {
		res.SetFirstVisit()
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	now := r.now()
	// Suspend comes before anything else: no scan, no validation, no timer.
	// Resuming is a spec change, and its scan covers everything that became due.
	if res.IsSuspended() {
		if !res.WasSuspended() {
			r.recorder.Event(res, corev1.EventTypeNormal, dbv1alpha1.ReasonSuspended, "scanning suspended")
		}
		closeOpenAttempt(res, now, "suspended")
		res.SetSuspended()
		res.Status.NextScheduleTime = nil
		return ctrl.Result{}, nil
	}
	sched, err := r.validate(res, now)
	if perr := (*permanentError)(nil); errors.As(err, &perr) {
		closeOpenAttempt(res, now, perr.message)
		res.SetStalled(perr.reason, perr.message)
		// A schedule that does not resolve publishes no next slot: a stale one
		// reads as overdue on a resource whose schedule is the broken part.
		switch perr.reason {
		case dbv1alpha1.ReasonInvalidSchedule, dbv1alpha1.ReasonInvalidTimeZone:
			res.Status.NextScheduleTime = nil
		}
		r.recorder.Event(res, corev1.EventTypeWarning, perr.reason, perr.message)
		return ctrl.Result{}, nil
	}
	snap, err := r.snapshot(ctx, res, sched, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	trigger, by := dueCheck(res, snap)
	if trigger == "" {
		// A crash between start and end whose cause has meanwhile disappeared.
		closeOpenAttempt(res, now, "interrupted")
		if !res.IsStalled("") {
			res.SetIdle()
		}
		return r.wake(res, sched, now, 0), nil
	}
	// One finished attempt per distinct set of inputs: an attempt lost between the
	// two status writes must not spend the one its inputs earned.
	if a := res.Status.LastScan; res.IsStalled(dbv1alpha1.ReasonBackoffLimitExceeded) &&
		a != nil && a.CompletionTime != nil && snap.hash == a.InputsHash {
		return r.wake(res, sched, now, 0), nil
	}
	if res.WasSuspended() {
		r.recorder.Event(res, corev1.EventTypeNormal, dbv1alpha1.EventResumed, "scanning resumed")
	}
	res.Status.LastScan = &dbv1alpha1.ScanAttempt{
		Trigger:     trigger,
		TriggeredBy: by,
		StartTime:   metav1.NewTime(now),
		InputsHash:  snap.hash,
	}
	if !res.IsStalled("") && (res.Status.LastSuccessfulTime == nil || trigger == dbv1alpha1.TriggerSpec) {
		res.SetScanning()
	}
	if err := r.writeStatus(ctx, res); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("scanning the database for security issues", "trigger", trigger, "by", by)
	data, err := r.extractData(ctx, res)
	if perr := (*permanentError)(nil); errors.As(err, &perr) {
		return r.stall(res, sched, snap, perr.reason, perr.message), nil
	}
	if err != nil {
		return r.fail(ctx, res, sched, snap, dbv1alpha1.ReasonReadingInputs, err), nil
	}
	wd, err := atlasexec.NewWorkingDir(atlasexec.WithAtlasHCL(data.render))
	if err != nil {
		return r.fail(ctx, res, sched, snap, dbv1alpha1.ReasonReadingInputs, err), nil
	}
	defer wd.Close()
	cli, err := r.atlasClient(wd.Path(), data.Cloud, filepath.Join(res.Namespace, res.Name))
	if err != nil {
		return r.fail(ctx, res, sched, snap, dbv1alpha1.ReasonCLIError, err), nil
	}
	if data.Cloud != nil && data.Cloud.Token != "" {
		if err := cli.Login(ctx, &atlasexec.LoginParams{Token: data.Cloud.Token, GrantOnly: true}); err != nil {
			return r.fail(ctx, res, sched, snap, dbv1alpha1.ReasonLoginFailed, err), nil
		}
	}
	scanCtx, cancel := context.WithTimeout(ctx, scanTimeout)
	defer cancel()
	// The operator grades the report itself: --fail-on would fold "threshold
	// reached" and "target not scanned" into one exit code, and --ignore would
	// drop waived findings from the report instead of marking them.
	scan, serr := cli.SecurityScan(scanCtx, &atlasexec.SecurityScanParams{
		Env:         data.EnvName,
		Vars:        data.Vars,
		MinSeverity: data.MinSeverity,
	})
	switch {
	case scan == nil:
		return r.fail(ctx, res, sched, snap, dbv1alpha1.ReasonCLIError, serr), nil
	case len(scan.Targets) == 0:
		return r.fail(ctx, res, sched, snap, dbv1alpha1.ReasonScanFailed, errors.New("the report names no target")), nil
	case len(scan.Targets) > 1:
		msg := fmt.Sprintf("configuration yields %d targets; one database per resource", len(scan.Targets))
		return r.stall(res, sched, snap, dbv1alpha1.ReasonInvalidTarget, msg), nil
	case len(scan.Failures()) > 0 || scan.Targets[0].Error != "":
		return r.fail(ctx, res, sched, snap, dbv1alpha1.ReasonScanFailed, errors.New(scan.Targets[0].Error)), nil
	case serr != nil:
		// The target was scanned, so the report stands and the findings are graded.
		log.Error(serr, "the CLI reported an error after producing the report")
		r.recorder.Event(res, corev1.EventTypeWarning, dbv1alpha1.EventScanWarning,
			"the CLI reported an error after producing the report; see the operator log")
	}
	var (
		target                  = scan.Targets[0]
		policy                  = res.Spec.Policy
		findings, exts, summary = grade(target, policy, snap.waivers)
		verdict, reason, vmsg   = verdictOf(findings, policy, res.Name)
		completion              = r.now()
		report                  = r.buildReport(res, target, findings, exts, summary, now, completion, trigger)
	)
	if err := r.storeReport(ctx, res, report); err != nil {
		return r.fail(ctx, res, sched, snap, dbv1alpha1.ReasonStoringReport, err), nil
	}
	// Commit every watermark from the snapshot, with the results, in one write.
	st := &res.Status
	prevSlot := st.LastScheduleTime
	st.ObservedGeneration = snap.generation
	st.LastHandledScanRequest = snap.requestedAt
	st.Triggers = snap.triggers
	st.ActiveWaivers = snap.waivers
	// A slot that passed while the scan ran is covered by it: the scan observed
	// the database across it, and a finer schedule would else scan back to back.
	slot := snap.slot
	if sched != nil {
		if covered := latestSlotAtOrBefore(sched, anchor(res, now), completion); covered != nil {
			slot = covered
		}
	}
	// The anchor moves only to a newer slot; a scan for another trigger keeps it,
	// and removing the schedule clears it.
	switch {
	case sched == nil:
		st.LastScheduleTime = nil
	case slot != nil:
		st.LastScheduleTime = &metav1.Time{Time: *slot}
	}
	closeAttempt(res, completion, dbv1alpha1.ScanSucceeded, "")
	st.LastSuccessfulTime = &metav1.Time{Time: completion}
	st.NextScheduleTime = nextSchedule(sched, anchor(res, now))
	st.Summary = &summary
	st.ReportRef = &corev1.LocalObjectReference{Name: res.Name}
	rmsg := readyMessage(summary)
	res.SetScanned(rmsg)
	res.SetCompliant(verdict, reason, vmsg)
	r.recorder.Eventf(res, corev1.EventTypeNormal, dbv1alpha1.ReasonScanned,
		"trigger=%s %s: %s; report atlassecurityreport/%s", trigger, by, rmsg, res.Name)
	if verdict == metav1.ConditionFalse {
		r.recorder.Event(res, corev1.EventTypeWarning, dbv1alpha1.ReasonPolicyViolated, vmsg)
	}
	// A schedule edit or a resume covers its gap by design, so only a routine
	// catch-up reports the slots it skipped. The count runs to the slot observed
	// at the start: the ones that passed while the scan ran are covered by it, so
	// a schedule finer than the scan does not report a backlog it never had.
	if trigger != dbv1alpha1.TriggerSpec && prevSlot != nil && snap.slot != nil {
		if n := slotsBetween(sched, prevSlot.Time, *snap.slot); n > 0 {
			r.recorder.Eventf(res, corev1.EventTypeNormal, dbv1alpha1.EventMissedSchedule,
				"caught up %d missed %s; latest covered %s",
				n, plural(n, "slot"), slot.UTC().Format(time.RFC3339))
		}
	}
	log.Info("scanned the database for security issues", "findings", summary.Total, "waived", summary.Waived)
	// A waiver that lapsed while the scan ran was still applied to these findings,
	// so the verdict is already out of date. Nothing else would wake a resource
	// that has no schedule, and wake has looked past the expiry by now.
	if !slices.Equal(snap.waivers, activeWaivers(res, completion)) {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	return r.wake(res, sched, completion, 0), nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AtlasSecurityScanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &dbv1alpha1.AtlasSecurityScan{}, triggerIndex, triggerKeys); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: runtime.NumCPU(),
		}).
		For(&dbv1alpha1.AtlasSecurityScan{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		))).
		// No Owns on the report: a deleted report is recreated by the next scan.
		// No Secret or ConfigMap watch: a rotation must not cause a scan, and the
		// controller behaves the same under WATCH_SECRETS=true and false.
		Watches(&dbv1alpha1.AtlasSchema{},
			handler.EnqueueRequestsFromMapFunc(r.scansTriggeredBy(dbv1alpha1.TriggerKindSchema)),
			builder.WithPredicates(revisionChanged(func(o client.Object) string {
				return o.(*dbv1alpha1.AtlasSchema).Status.ObservedHash
			}))).
		Watches(&dbv1alpha1.AtlasMigration{},
			handler.EnqueueRequestsFromMapFunc(r.scansTriggeredBy(dbv1alpha1.TriggerKindMigration)),
			builder.WithPredicates(revisionChanged(func(o client.Object) string {
				return o.(*dbv1alpha1.AtlasMigration).Status.LastAppliedVersion
			}))).
		Complete(r)
}

// SetAtlasClient sets the Atlas client function.
func (r *AtlasSecurityScanReconciler) SetAtlasClient(fn AtlasExecFn) {
	r.atlasClient = fn
}

// AllowCustomConfig allows the controller to use custom atlas.hcl config.
func (r *AtlasSecurityScanReconciler) AllowCustomConfig() {
	r.allowCustomConfig = true
}

// triggerKeys indexes a scan by its triggers.
func triggerKeys(o client.Object) []string {
	s := o.(*dbv1alpha1.AtlasSecurityScan)
	keys := make([]string, 0, len(s.Spec.Triggers))
	for _, t := range s.Spec.Triggers {
		keys = append(keys, triggerKey(t.Kind, t.Name))
	}
	return keys
}

func triggerKey(kind dbv1alpha1.ScanTriggerKind, name string) string {
	return string(kind) + "/" + name
}

// revisionChanged passes an update whose applied revision moved, and a create
// that carries one: a resource recreated or newly matching the operator's labels
// arrives as an add, and its uid alone makes it pending.
func revisionChanged(rev func(client.Object) string) predicate.Funcs {
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return rev(e.Object) != "" },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc:  func(e event.UpdateEvent) bool { return rev(e.ObjectOld) != rev(e.ObjectNew) },
	}
}

// scansTriggeredBy enqueues the scans that name the given resource as a trigger.
func (r *AtlasSecurityScanReconciler) scansTriggeredBy(kind dbv1alpha1.ScanTriggerKind) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		var list dbv1alpha1.AtlasSecurityScanList
		if err := r.List(ctx, &list, client.InNamespace(o.GetNamespace()),
			client.MatchingFields{triggerIndex: triggerKey(kind, o.GetName())}); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(list.Items))
		for i := range list.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
		return reqs
	}
}

// validate checks the inputs that no scan can fix. It returns the parsed
// schedule, nil when the resource has none.
func (r *AtlasSecurityScanReconciler) validate(res *dbv1alpha1.AtlasSecurityScan, now time.Time) (*scanSchedule, error) {
	s := res.Spec
	tz := s.TimeZone
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, &permanentError{dbv1alpha1.ReasonInvalidTimeZone, "unknown time zone"}
	}
	var sched *scanSchedule
	if s.Schedule != "" {
		switch {
		case strings.HasPrefix(s.Schedule, "TZ="), strings.HasPrefix(s.Schedule, "CRON_TZ="):
			return nil, &permanentError{dbv1alpha1.ReasonInvalidSchedule, "use spec.timeZone instead of a TZ= prefix"}
		case strings.HasPrefix(s.Schedule, "@every"):
			return nil, &permanentError{dbv1alpha1.ReasonInvalidSchedule, "@every is interval-based and drifts"}
		}
		parsed, err := cron.ParseStandard(s.Schedule)
		if err != nil {
			return nil, &permanentError{dbv1alpha1.ReasonInvalidSchedule, "schedule does not parse"}
		}
		sched = &scanSchedule{Schedule: parsed, loc: loc}
		if sched.Next(now).IsZero() {
			return nil, &permanentError{dbv1alpha1.ReasonInvalidSchedule, "schedule does not fire"}
		}
	}
	hasTarget := s.URL != "" || s.URLFrom.SecretKeyRef != nil || s.Credentials.Scheme != ""
	hasConfig := s.Config != "" || s.ConfigFrom.SecretKeyRef != nil
	switch {
	case !hasTarget && !hasConfig:
		return nil, &permanentError{dbv1alpha1.ReasonInvalidTarget, "no target database or project configuration"}
	case hasConfig && !r.allowCustomConfig:
		return nil, &permanentError{dbv1alpha1.ReasonInvalidTarget, "install the operator with allowCustomConfig=true to use a custom atlas.hcl"}
	case hasConfig && s.EnvName == "":
		return nil, &permanentError{dbv1alpha1.ReasonInvalidTarget, "envName must be set when using a custom atlas.hcl"}
	}
	return sched, nil
}

// snapshot observes everything that can make a scan due. Nothing in memory
// survives a restart, so it is re-derived from the cluster on every pass.
func (r *AtlasSecurityScanReconciler) snapshot(ctx context.Context, res *dbv1alpha1.AtlasSecurityScan, sched *scanSchedule, now time.Time) (*scanSnapshot, error) {
	snap := &scanSnapshot{
		start:       now,
		generation:  res.Generation,
		requestedAt: res.Annotations[dbv1alpha1.AnnotationScanRequestedAt],
	}
	for _, t := range res.Spec.Triggers {
		key := client.ObjectKey{Namespace: res.Namespace, Name: t.Name}
		var (
			obj client.Object
			rev func() string
		)
		switch t.Kind {
		case dbv1alpha1.TriggerKindSchema:
			o := &dbv1alpha1.AtlasSchema{}
			obj, rev = o, func() string { return o.Status.ObservedHash }
		case dbv1alpha1.TriggerKindMigration:
			o := &dbv1alpha1.AtlasMigration{}
			obj, rev = o, func() string { return o.Status.LastAppliedVersion }
		default:
			continue
		}
		// Only NotFound means absent, which a label-scoped cache also reports for a
		// resource this instance does not manage. Any other error is returned: an
		// omitted trigger is never pending, so swallowing one loses the scan.
		switch err := r.Get(ctx, key, obj); {
		case apierrors.IsNotFound(err):
			r.recorder.Eventf(res, corev1.EventTypeWarning, dbv1alpha1.EventTriggerNotFound,
				"%s/%s not found in %s; it cannot trigger scans until it exists and is managed by this operator instance",
				t.Kind, t.Name, res.Namespace)
			continue
		case err != nil:
			return nil, err
		}
		snap.triggers = append(snap.triggers, dbv1alpha1.ObservedTrigger{
			Kind: t.Kind, Name: t.Name, UID: obj.GetUID(), Revision: rev(),
		})
	}
	if sched != nil {
		snap.slot = latestSlotAtOrBefore(sched, anchor(res, now), now)
	}
	snap.waivers = activeWaivers(res, now)
	h := sha256.New()
	fmt.Fprintln(h, snap.generation, snap.requestedAt)
	for _, t := range snap.triggers {
		fmt.Fprintln(h, t.Kind, t.Name, t.UID, t.Revision)
	}
	if snap.slot != nil {
		fmt.Fprintln(h, snap.slot.UTC().Format(time.RFC3339))
	}
	fmt.Fprintln(h, strings.Join(snap.waivers, ","))
	snap.hash = "sha256:" + hex.EncodeToString(h.Sum(nil))
	return snap, nil
}

// activeWaivers are the ids of the waivers in force at the given time, sorted so
// two observations compare.
func activeWaivers(res *dbv1alpha1.AtlasSecurityScan, at time.Time) []string {
	var ids []string
	if p := res.Spec.Policy; p != nil {
		for _, w := range p.Ignore {
			if w.ExpirationTime == nil || w.ExpirationTime.After(at) {
				ids = append(ids, w.ID)
			}
		}
		slices.Sort(ids)
	}
	return ids
}

// dueCheck reports what the scan is due for, and "" when nothing is pending. The
// precedence decides only the label; one scan satisfies everything pending.
func dueCheck(res *dbv1alpha1.AtlasSecurityScan, snap *scanSnapshot) (dbv1alpha1.ScanTrigger, string) {
	st := res.Status
	switch {
	case st.LastSuccessfulTime == nil, res.Generation != st.ObservedGeneration:
		return dbv1alpha1.TriggerSpec, fmt.Sprintf("generation %d", res.Generation)
	case snap.requestedAt != "" && snap.requestedAt != st.LastHandledScanRequest:
		return dbv1alpha1.TriggerManual, snap.requestedAt
	}
	if pending := pendingTriggers(snap.triggers, st.Triggers); len(pending) > 0 {
		return dbv1alpha1.TriggerApply, strings.Join(pending, ", ")
	}
	if snap.slot != nil && (st.LastScheduleTime == nil || snap.slot.After(st.LastScheduleTime.Time)) {
		return dbv1alpha1.TriggerSchedule, snap.slot.UTC().Format(time.RFC3339)
	}
	if !slices.Equal(snap.waivers, slices.Sorted(slices.Values(st.ActiveWaivers))) {
		return dbv1alpha1.TriggerPolicy, "waivers changed"
	}
	return "", ""
}

// pendingTriggers names the triggers whose revision differs from the recorded
// one, or that were never recorded. Triggers that were not found are absent
// from the snapshot and hence never pending.
func pendingTriggers(observed, recorded []dbv1alpha1.ObservedTrigger) []string {
	var pending []string
	for _, o := range observed {
		i := slices.IndexFunc(recorded, func(r dbv1alpha1.ObservedTrigger) bool {
			return r.Kind == o.Kind && r.Name == o.Name
		})
		if i == -1 || recorded[i].UID != o.UID || recorded[i].Revision != o.Revision {
			pending = append(pending, triggerKey(o.Kind, o.Name))
		}
	}
	return pending
}

// anchor is the point the schedule is evaluated from: the last covered slot, or
// before the first one the later of creation and the last success.
func anchor(res *dbv1alpha1.AtlasSecurityScan, now time.Time) time.Time {
	if t := res.Status.LastScheduleTime; t != nil {
		return t.Time
	}
	a := res.CreationTimestamp.Time
	if t := res.Status.LastSuccessfulTime; t != nil && t.After(a) {
		a = t.Time
	}
	if a.IsZero() {
		a = now
	}
	return a
}

// latestSlotAtOrBefore returns the last slot after the anchor that is not after
// now, or nil when there is none. Past the step cap it anchors at now.
func latestSlotAtOrBefore(sched *scanSchedule, from, now time.Time) *time.Time {
	var last *time.Time
	t := sched.Next(from)
	for i := 0; !t.IsZero() && !t.After(now); i++ {
		if i >= maxSlotSteps {
			// Every real slot lands on a whole second, and the inputs hash covers
			// this value: a wall clock with nanoseconds would differ on every pass
			// and spend a stalled resource's one attempt per input set forever.
			capped := now.Truncate(time.Second)
			return &capped
		}
		slot := t
		last = &slot
		t = sched.Next(t)
	}
	return last
}

// slotsBetween counts the slots strictly between two times.
func slotsBetween(sched *scanSchedule, from, to time.Time) int {
	n := 0
	for t := sched.Next(from); !t.IsZero() && t.Before(to) && n < maxSlotSteps; t = sched.Next(t) {
		n++
	}
	return n
}

// nextSchedule is the slot after the anchor, nil without a schedule or a slot.
func nextSchedule(sched *scanSchedule, from time.Time) *metav1.Time {
	if sched == nil {
		return nil
	}
	n := sched.Next(from)
	if n.IsZero() {
		return nil
	}
	return &metav1.Time{Time: n}
}

// wake asks to be woken at the earliest of the next slot, the earliest waiver
// expiry and the retry delay. It is computed from now, never from
// nextScheduleTime, which may sit in the past while a slot is uncommitted.
func (r *AtlasSecurityScanReconciler) wake(res *dbv1alpha1.AtlasSecurityScan, sched *scanSchedule, now time.Time, retry time.Duration) ctrl.Result {
	var (
		d   time.Duration
		set bool
	)
	consider := func(dd time.Duration) {
		if !set || dd < d {
			d, set = dd, true
		}
	}
	if sched != nil {
		if n := sched.Next(now); !n.IsZero() {
			consider(n.Sub(now))
		}
	}
	if p := res.Spec.Policy; p != nil {
		for _, w := range p.Ignore {
			if w.ExpirationTime != nil && w.ExpirationTime.After(now) {
				consider(w.ExpirationTime.Sub(now))
			}
		}
	}
	if retry > 0 {
		consider(retry)
	}
	if !set {
		return ctrl.Result{}
	}
	return ctrl.Result{RequeueAfter: max(d, time.Second)}
}

// fail records a failed attempt, retried with linear backoff until the limit.
// The cause goes to the log only: CLI and driver errors carry the resolved
// address of a Secret-held host, which status and Events must not.
func (r *AtlasSecurityScanReconciler) fail(ctx context.Context, res *dbv1alpha1.AtlasSecurityScan, sched *scanSchedule, snap *scanSnapshot, reason string, cause error) ctrl.Result {
	res.Status.Failed++
	msg := failureText(reason, res.Status.Failed, res.Spec.BackoffLimit)
	closeAttempt(res, r.now(), dbv1alpha1.ScanFailed, msg)
	log.FromContext(ctx).Error(cause, "security scan failed", "reason", reason, "attempt", res.Status.Failed)
	switch {
	// Conditions do not move once retries are exhausted, so a database down for a
	// week reads as Stalled once rather than flapping at every slot.
	case res.IsStalled(dbv1alpha1.ReasonBackoffLimitExceeded):
		return r.wake(res, sched, snap.start, 0)
	case res.Spec.BackoffLimit > 0 && res.Status.Failed > res.Spec.BackoffLimit:
		const exceeded = "backoff limit exceeded; one attempt per new slot, apply, request, spec change or waiver expiry"
		res.SetStalled(dbv1alpha1.ReasonBackoffLimitExceeded, exceeded)
		res.SetCompliant(metav1.ConditionUnknown, dbv1alpha1.ReasonReportStale,
			"retries exhausted; the last report may no longer reflect the database")
		r.recorder.Event(res, corev1.EventTypeWarning, dbv1alpha1.ReasonBackoffLimitExceeded, exceeded)
		return r.wake(res, sched, snap.start, 0)
	}
	res.SetRetrying(reason, msg)
	r.recorder.Event(res, corev1.EventTypeWarning, reason, msg)
	return r.wake(res, sched, snap.start, backoffDelayAt(res.Status.Failed))
}

// failureText is the fixed message of a failure class.
func failureText(reason string, failed, limit int) string {
	what := map[string]string{
		dbv1alpha1.ReasonScanFailed:    "the database could not be scanned",
		dbv1alpha1.ReasonLoginFailed:   "Atlas Cloud login failed",
		dbv1alpha1.ReasonCLIError:      "the Atlas CLI failed before producing a report",
		dbv1alpha1.ReasonReadingInputs: "the scan inputs could not be read",
		dbv1alpha1.ReasonStoringReport: "the report could not be stored",
	}[reason]
	if limit > 0 {
		return fmt.Sprintf("%s; attempt %d of %d; see the operator log", what, failed, limit)
	}
	return fmt.Sprintf("%s; attempt %d; see the operator log", what, failed)
}

// stall records an attempt that failed on an input no retry can fix. A schedule
// still wakes it, as the config is read on every scan and may be fixed outside
// the spec; with triggers alone it waits for an apply or a scan request.
func (r *AtlasSecurityScanReconciler) stall(res *dbv1alpha1.AtlasSecurityScan, sched *scanSchedule, snap *scanSnapshot, reason, message string) ctrl.Result {
	closeAttempt(res, r.now(), dbv1alpha1.ScanFailed, message)
	res.SetStalled(reason, message)
	r.recorder.Event(res, corev1.EventTypeWarning, reason, message)
	return r.wake(res, sched, snap.start, 0)
}

// closeAttempt ends the attempt in progress with the given result.
func closeAttempt(res *dbv1alpha1.AtlasSecurityScan, at time.Time, result dbv1alpha1.ScanResult, message string) {
	if a := res.Status.LastScan; a != nil {
		a.CompletionTime = &metav1.Time{Time: at}
		a.Result, a.Message = result, message
	}
}

// closeOpenAttempt fails an attempt that never completed, so a crash mid-scan
// does not leave one open forever.
func closeOpenAttempt(res *dbv1alpha1.AtlasSecurityScan, at time.Time, message string) {
	if a := res.Status.LastScan; a != nil && a.CompletionTime == nil {
		closeAttempt(res, at, dbv1alpha1.ScanFailed, message)
	}
}

// extractData resolves the inputs of the CLI run from the cluster.
func (r *AtlasSecurityScanReconciler) extractData(ctx context.Context, res *dbv1alpha1.AtlasSecurityScan) (_ *scanData, err error) {
	var (
		s    = res.Spec
		data = &scanData{EnvName: defaultEnvName, MinSeverity: string(res.MinSeverity())}
	)
	if s.EnvName != "" {
		data.EnvName = s.EnvName
	}
	if data.Config, err = s.GetConfig(ctx, r, res.Namespace); err != nil {
		return nil, err
	}
	if err := checkPolicy(data.Config, data.EnvName); err != nil {
		return nil, err
	}
	if ref := s.Cloud.TokenFrom.SecretKeyRef; ref != nil {
		token, err := getSecretValue(ctx, r, res.Namespace, ref)
		if err != nil {
			return nil, err
		}
		data.Cloud = &Cloud{Token: token}
	}
	if data.URL, err = s.DatabaseURL(ctx, r, res.Namespace); err != nil {
		return nil, err
	}
	if data.Vars, err = s.GetVars(ctx, r, res.Namespace); err != nil {
		return nil, err
	}
	return data, nil
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

// policyAttrs are the project-config attributes that grade a scan, keyed by the
// block path they sit in. spec.policy is the only policy, so a config that sets
// one is rejected rather than silently obeyed: the CLI appends configured
// ignores to its own, and a configured cve.min_severity is a floor that
// --min-severity cannot lower, so a finding would vanish before it is graded.
var policyAttrs = map[string][]string{
	"security":     {"min_severity", "fail_on"},
	"security.cve": {"min_severity", "ignore"},
}

// checkPolicy rejects a custom config that decides what the scan reports, for
// the env the scan runs with. Another env is left alone, as the CLI reads only
// the selected one and the top-level block it extends.
func checkPolicy(cfg *hclwrite.File, envName string) error {
	if cfg == nil {
		return nil
	}
	for _, b := range cfg.Body().Blocks() {
		switch {
		// The top-level block extends the one of the env that runs.
		case b.Type() == "security":
			if err := checkPolicyBlock(b, "security"); err != nil {
				return err
			}
		// An unlabeled env block may still become the one that runs: mergeBlocks
		// relabels it when it carries name = atlas.env.
		case isEnvBlock(b) && (len(b.Labels()) == 0 || slices.Contains(b.Labels(), envName)):
			// exclude reaches the realm inspection, so an excluded extension is
			// never sent to the Security Graph: the scan then reports nothing and
			// still succeeds, which no failure path would catch.
			if b.Body().GetAttribute("exclude") != nil {
				return &permanentError{
					dbv1alpha1.ReasonInvalidTarget,
					"exclude in the custom atlas.hcl would hide extensions from the scan",
				}
			}
			for _, n := range b.Body().Blocks() {
				if n.Type() != "security" {
					continue
				}
				if err := checkPolicyBlock(n, "security"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// checkPolicyBlock reports the first policy attribute set in the block or in a
// nested block that holds any. The path it names is one of policyAttrs, never
// text read from the configuration.
func checkPolicyBlock(b *hclwrite.Block, path string) error {
	for _, name := range policyAttrs[path] {
		if b.Body().GetAttribute(name) != nil {
			return &permanentError{
				dbv1alpha1.ReasonInvalidTarget,
				fmt.Sprintf("%s.%s is set by spec.policy and must not appear in the custom atlas.hcl", path, name),
			}
		}
	}
	for _, n := range b.Body().Blocks() {
		nested := path + "." + n.Type()
		if _, ok := policyAttrs[nested]; !ok {
			continue
		}
		if err := checkPolicyBlock(n, nested); err != nil {
			return err
		}
	}
	return nil
}

// grade attaches waivers to the findings, names the extensions and counts the
// rest per level. One list feeds both, so the count cannot exceed the names.
func grade(t *atlasexec.SecurityScanTarget, policy *dbv1alpha1.ScanPolicy, waivers []string) ([]dbv1alpha1.ReportedVulnerability, []string, dbv1alpha1.ScanSummary) {
	var (
		exts     = slices.Compact(slices.Sorted(slices.Values(t.Extensions)))
		findings []dbv1alpha1.ReportedVulnerability
		counts   = make(map[dbv1alpha1.SecurityLevel]int32, len(dbv1alpha1.SecurityLevels))
		summary  = dbv1alpha1.ScanSummary{Driver: t.Driver, Extensions: int32(len(exts))}
		seen     = make(map[[2]string]bool)
	)
	for _, v := range t.Vulnerabilities {
		if key := [2]string{v.ID, v.Name}; seen[key] {
			continue
		} else {
			seen[key] = true
		}
		f := dbv1alpha1.ReportedVulnerability{
			ID:           v.ID,
			Extension:    v.Name,
			Version:      v.Version,
			Level:        dbv1alpha1.SecurityLevel(strings.ToUpper(v.Level)),
			CVSSSeverity: strings.ToUpper(v.Severity),
			Title:        v.Title,
			Description:  truncate(v.Description, descriptionLimit),
			Suggestion:   v.Suggestion,
		}
		if slices.Contains(waivers, v.ID) {
			if w := waiverFor(policy, v.ID); w != nil {
				f.Waiver = w
			}
			summary.Waived++
		} else {
			counts[f.Level]++
			summary.Total++
			if dbv1alpha1.LevelIndex(f.Level) > dbv1alpha1.LevelIndex(summary.HighestLevel) {
				summary.HighestLevel = f.Level
			}
		}
		findings = append(findings, f)
	}
	// Every level, highest first, so metric series never disappear.
	for i := len(dbv1alpha1.SecurityLevels) - 1; i >= 0; i-- {
		l := dbv1alpha1.SecurityLevels[i]
		summary.Levels = append(summary.Levels, dbv1alpha1.LevelCount{Level: l, Count: counts[l]})
	}
	return findings, exts, summary
}

func waiverFor(policy *dbv1alpha1.ScanPolicy, id string) *dbv1alpha1.Waiver {
	if policy == nil {
		return nil
	}
	for _, w := range policy.Ignore {
		if w.ID == id {
			return &dbv1alpha1.Waiver{Reason: w.Reason, ExpirationTime: w.ExpirationTime}
		}
	}
	return nil
}

// verdictOf grades the non-waived findings against failOn.
func verdictOf(findings []dbv1alpha1.ReportedVulnerability, policy *dbv1alpha1.ScanPolicy, report string) (metav1.ConditionStatus, string, string) {
	if policy == nil || policy.FailOn == nil {
		return metav1.ConditionUnknown, dbv1alpha1.ReasonNoThreshold, "spec.policy.failOn is not set; findings are reported only"
	}
	var (
		failOn  = *policy.FailOn
		n       int
		highest dbv1alpha1.SecurityLevel
	)
	for _, f := range findings {
		if f.Waiver != nil || dbv1alpha1.LevelIndex(f.Level) < dbv1alpha1.LevelIndex(failOn) {
			continue
		}
		n++
		if dbv1alpha1.LevelIndex(f.Level) > dbv1alpha1.LevelIndex(highest) {
			highest = f.Level
		}
	}
	if n == 0 {
		return metav1.ConditionTrue, dbv1alpha1.ReasonWithinPolicy,
			fmt.Sprintf("no findings at or above %s; see atlassecurityreport/%s", failOn, report)
	}
	return metav1.ConditionFalse, dbv1alpha1.ReasonPolicyViolated,
		fmt.Sprintf("%d %s at or above %s, highest %s; see atlassecurityreport/%s", n, plural(n, "finding"), failOn, highest, report)
}

// readyMessage describes the summary. e.g., "3 findings (1 CRITICAL, 2 HIGH) in 4 extensions; 1 waived".
func readyMessage(s dbv1alpha1.ScanSummary) string {
	var b strings.Builder
	if s.Total == 0 {
		fmt.Fprintf(&b, "no findings in %d %s", s.Extensions, plural(int(s.Extensions), "extension"))
	} else {
		var levels []string
		for _, l := range s.Levels {
			if l.Count > 0 {
				levels = append(levels, fmt.Sprintf("%d %s", l.Count, l.Level))
			}
		}
		fmt.Fprintf(&b, "%d %s (%s) in %d %s", s.Total, plural(int(s.Total), "finding"),
			strings.Join(levels, ", "), s.Extensions, plural(int(s.Extensions), "extension"))
	}
	if s.Waived > 0 {
		fmt.Fprintf(&b, "; %d waived", s.Waived)
	}
	return b.String()
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// truncate cuts s to at most n bytes, ending on a rune boundary: a cut through a
// multibyte character would reach the report as U+FFFD.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// buildReport assembles the report of a successful scan. It never carries the
// target URL in any form, nor any error text.
func (r *AtlasSecurityScanReconciler) buildReport(
	res *dbv1alpha1.AtlasSecurityScan, t *atlasexec.SecurityScanTarget,
	findings []dbv1alpha1.ReportedVulnerability, exts []string, summary dbv1alpha1.ScanSummary,
	start, completion time.Time, trigger dbv1alpha1.ScanTrigger,
) *dbv1alpha1.AtlasSecurityReport {
	labels := make(map[string]string, len(res.Labels)+1)
	maps.Copy(labels, res.Labels)
	labels[dbv1alpha1.LabelSecurityScan] = res.Name
	policy := dbv1alpha1.GradedPolicy{MinSeverity: res.MinSeverity()}
	if p := res.Spec.Policy; p != nil {
		policy.FailOn = p.FailOn
	}
	return &dbv1alpha1.AtlasSecurityReport{
		ObjectMeta: metav1.ObjectMeta{Name: res.Name, Namespace: res.Namespace, Labels: labels},
		Report: dbv1alpha1.SecurityReport{
			StartTime:       metav1.NewTime(start),
			CompletionTime:  metav1.NewTime(completion),
			Trigger:         trigger,
			ServerVersion:   t.Version,
			Policy:          policy,
			Summary:         summary,
			Extensions:      exts,
			Vulnerabilities: findings,
		},
	}
}

// storeReport creates or replaces the report of the scan. It reads through the
// uncached API reader, so a report left by another instance or version is
// updated rather than collided with.
func (r *AtlasSecurityScanReconciler) storeReport(ctx context.Context, res *dbv1alpha1.AtlasSecurityScan, report *dbv1alpha1.AtlasSecurityReport) error {
	existing := &dbv1alpha1.AtlasSecurityReport{}
	err := r.reader.Get(ctx, client.ObjectKeyFromObject(report), existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := controllerutil.SetControllerReference(res, report, r.scheme); err != nil {
			return err
		}
		return r.Create(ctx, report)
	case err != nil:
		return err
	}
	existing.Labels, existing.Report = report.Labels, report.Report
	if err := controllerutil.SetControllerReference(res, existing, r.scheme); err != nil {
		return err
	}
	return r.Update(ctx, existing)
}

// writeStatus copies the status onto the latest object and writes it.
func (r *AtlasSecurityScanReconciler) writeStatus(ctx context.Context, res *dbv1alpha1.AtlasSecurityScan) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &dbv1alpha1.AtlasSecurityScan{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(res), latest); err != nil {
			return err
		}
		latest.Status = res.Status
		return r.Status().Update(ctx, latest)
	})
	if err != nil {
		return fmt.Errorf("updating resource status: %w", err)
	}
	return nil
}
