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
	"bytes"
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"ariga.io/atlas/atlasexec"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/robfig/cron/v3"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
)

// scanTest drives one AtlasSecurityScan through a fake cluster, a mocked Atlas
// client and an injected clock.
type scanTest struct {
	*testing.T
	h     *helper
	run   runner
	r     *AtlasSecurityScanReconciler
	mock  *mockAtlasExec
	obj   *dbv1alpha1.AtlasSecurityScan
	clock time.Time
	// elapse advances the clock on every read, so a scan can be made to outlast
	// a slot or a waiver of its own snapshot.
	elapse time.Duration
}

func newScanTest(t *testing.T, obj *dbv1alpha1.AtlasSecurityScan, extra ...client.Object) *scanTest {
	t.Helper()
	st := &scanTest{T: t, mock: &mockAtlasExec{}, obj: obj, clock: time.Date(2026, 9, 10, 1, 30, 0, 0, time.UTC)}
	st.h, st.run = newRunner(func(m Manager, _ bool) *AtlasSecurityScanReconciler {
		st.r = NewAtlasSecurityScanReconciler(m)
		st.r.now = func() time.Time {
			t := st.clock
			st.clock = t.Add(st.elapse)
			return t
		}
		return st.r
	}, func(cb *fake.ClientBuilder) {
		cb.WithStatusSubresource(obj)
		cb.WithObjects(obj)
		cb.WithObjects(extra...)
	}, st.mock)
	return st
}

func (st *scanTest) reconcile() (ctrl.Result, *dbv1alpha1.AtlasSecurityScan) {
	st.Helper()
	var result ctrl.Result
	st.run(st.obj, func(r ctrl.Result, err error) {
		require.NoError(st, err)
		result = r
	})
	return result, st.get()
}

func (st *scanTest) get() *dbv1alpha1.AtlasSecurityScan {
	st.Helper()
	res := &dbv1alpha1.AtlasSecurityScan{ObjectMeta: metav1.ObjectMeta{Name: st.obj.Name, Namespace: st.obj.Namespace}}
	st.h.get(st.T, res)
	return res
}

func (st *scanTest) report() *dbv1alpha1.AtlasSecurityReport {
	st.Helper()
	rep := &dbv1alpha1.AtlasSecurityReport{ObjectMeta: metav1.ObjectMeta{Name: st.obj.Name, Namespace: st.obj.Namespace}}
	st.h.get(st.T, rep)
	return rep
}

func cond(t *testing.T, res *dbv1alpha1.AtlasSecurityScan, typ string) metav1.Condition {
	t.Helper()
	c := apimeta.FindStatusCondition(res.Status.Conditions, typ)
	require.NotNil(t, c, "condition %s not found", typ)
	return *c
}

var (
	// The target the fake CLI reports. Its URL is redacted by the CLI, and its
	// host must still never reach status or Events.
	scanTarget = func(vulns ...*atlasexec.SecurityVulnerability) *atlasexec.SecurityScan {
		return &atlasexec.SecurityScan{Targets: []*atlasexec.SecurityScanTarget{{
			URL:             "postgres://root:xxxxx@db.internal:5432/app?sslmode=disable",
			Driver:          "postgres",
			Version:         "16.4",
			Extensions:      []string{"postgis", "hstore"},
			Vulnerabilities: vulns,
		}}}
	}
	hstoreCVE = &atlasexec.SecurityVulnerability{
		Name: "hstore", Version: "1.3", ID: "CVE-2014-2669", Level: "ELEVATED", Severity: "MEDIUM",
		Description: strings.Repeat("x", 2000),
		Suggestion:  "Upgrade the database engine to version 9.3.3 or later",
	}
	postgisCVE = &atlasexec.SecurityVulnerability{
		Name: "postgis", Version: "2.3.1", ID: "CVE-2017-18359", Level: "HIGH", Severity: "HIGH",
		Title: "Buffer overflow in postgis", Suggestion: "Upgrade extension postgis to version 2.3.3 or later",
	}
)

func scanObject() *dbv1alpha1.AtlasSecurityScan {
	failOn := dbv1alpha1.SecurityLevelHigh
	return &dbv1alpha1.AtlasSecurityScan{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "test", Generation: 1, Labels: map[string]string{"team": "payments"}},
		Spec: dbv1alpha1.AtlasSecurityScanSpec{
			TargetSpec: dbv1alpha1.TargetSpec{URLFrom: dbv1alpha1.Secret{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"}, Key: "url",
			}}},
			Cloud: dbv1alpha1.Cloud{TokenFrom: dbv1alpha1.TokenFrom{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "atlas-token"}, Key: "token",
			}}},
			Schedule: "0 3 * * *",
			TimeZone: "UTC",
			Triggers: []dbv1alpha1.ScanTriggerRef{{Kind: dbv1alpha1.TriggerKindSchema, Name: "myapp"}},
			Policy: &dbv1alpha1.ScanPolicy{
				MinSeverity: dbv1alpha1.SecurityLevelElevated,
				FailOn:      &failOn,
				Ignore: []dbv1alpha1.IgnoredVulnerability{{
					ID: "CVE-2014-2669", Waiver: dbv1alpha1.Waiver{Reason: "hstore not reachable from the app role"},
				}},
			},
			BackoffLimit: 20,
		},
	}
}

func scanSecrets() []client.Object {
	return []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db-creds", Namespace: "test"},
			Data: map[string][]byte{"url": []byte("postgres://root:pass@db.internal:5432/app?sslmode=disable")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "atlas-token", Namespace: "test"},
			Data: map[string][]byte{"token": []byte("aci_token")}},
	}
}

func TestSecurityScan_Reconcile(t *testing.T) {
	schema := &dbv1alpha1.AtlasSchema{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "test", UID: types.UID("uid-1")},
		Status:     dbv1alpha1.AtlasSchemaStatus{ObservedHash: "h1", LastApplied: 100},
	}
	st := newScanTest(t, scanObject(), append(scanSecrets(), schema)...)

	// The first visit only creates the conditions.
	result, res := st.reconcile()
	require.Equal(t, ctrl.Result{RequeueAfter: time.Second}, result)
	require.Equal(t, metav1.ConditionUnknown, cond(t, res, "Ready").Status)
	require.Equal(t, dbv1alpha1.ReasonNotScanned, cond(t, res, "Compliant").Reason)
	require.Empty(t, st.mock.securityScans)

	// The first scan is the resource's own. One finding is waived, one violates the policy.
	st.mock.securityScan.res = scanTarget(hstoreCVE, postgisCVE)
	result, res = st.reconcile()
	require.Equal(t, ctrl.Result{RequeueAfter: 90 * time.Minute}, result, "woken at the 03:00 slot")
	require.Equal(t, []*atlasexec.SecurityScanParams{{Env: "kubernetes", Vars: atlasexec.Vars2{}, MinSeverity: "ELEVATED"}},
		st.mock.securityScans, "the operator grades the report itself: no --fail-on, no --ignore")
	ready := cond(t, res, "Ready")
	require.Equal(t, metav1.ConditionTrue, ready.Status)
	require.Equal(t, dbv1alpha1.ReasonScanned, ready.Reason)
	require.Equal(t, "1 finding (1 HIGH) in 2 extensions; 1 waived", ready.Message)
	compliant := cond(t, res, "Compliant")
	require.Equal(t, metav1.ConditionFalse, compliant.Status)
	require.Equal(t, dbv1alpha1.ReasonPolicyViolated, compliant.Reason)
	require.Equal(t, "1 finding at or above HIGH, highest HIGH; see atlassecurityreport/app", compliant.Message)
	require.Equal(t, metav1.ConditionFalse, cond(t, res, "Reconciling").Status)
	last := res.Status.LastScan
	require.Equal(t, dbv1alpha1.TriggerSpec, last.Trigger)
	require.Equal(t, "generation 1", last.TriggeredBy)
	require.True(t, last.StartTime.Time.Equal(st.clock))
	require.True(t, last.CompletionTime.Time.Equal(st.clock))
	require.Equal(t, dbv1alpha1.ScanSucceeded, last.Result)
	require.Empty(t, last.Message)
	require.True(t, strings.HasPrefix(last.InputsHash, "sha256:"))
	require.Equal(t, int64(1), res.Status.ObservedGeneration)
	require.True(t, res.Status.LastSuccessfulTime.Equal(&metav1.Time{Time: st.clock}))
	require.Nil(t, res.Status.LastScheduleTime, "no slot between creation and now")
	require.Equal(t, time.Date(2026, 9, 10, 3, 0, 0, 0, time.UTC), res.Status.NextScheduleTime.UTC())
	require.Equal(t, []dbv1alpha1.ObservedTrigger{{Kind: "AtlasSchema", Name: "myapp", UID: "uid-1", Revision: "h1"}}, res.Status.Triggers)
	require.Equal(t, []string{"CVE-2014-2669"}, res.Status.ActiveWaivers)
	require.Equal(t, &dbv1alpha1.ScanSummary{
		Driver: "postgres", Extensions: 2, Total: 1, Waived: 1, HighestLevel: dbv1alpha1.SecurityLevelHigh,
		Levels: []dbv1alpha1.LevelCount{{Level: "CRITICAL"}, {Level: "HIGH", Count: 1}, {Level: "ELEVATED"}, {Level: "NORMAL"}},
	}, res.Status.Summary)
	require.Equal(t, &corev1.LocalObjectReference{Name: "app"}, res.Status.ReportRef)
	require.Equal(t, 0, res.Status.Failed)

	// The findings live in the report, which the scan owns.
	rep := st.report()
	require.Equal(t, map[string]string{"team": "payments", "db.atlasgo.io/scan": "app"}, rep.Labels)
	require.Len(t, rep.OwnerReferences, 1)
	require.Equal(t, "AtlasSecurityScan", rep.OwnerReferences[0].Kind)
	require.True(t, *rep.OwnerReferences[0].Controller)
	require.Equal(t, dbv1alpha1.TriggerSpec, rep.Report.Trigger)
	require.Equal(t, "16.4", rep.Report.ServerVersion)
	require.Equal(t, []string{"hstore", "postgis"}, rep.Report.Extensions)
	require.Equal(t, dbv1alpha1.GradedPolicy{MinSeverity: "ELEVATED", FailOn: new(dbv1alpha1.SecurityLevelHigh)}, rep.Report.Policy)
	require.Equal(t, []dbv1alpha1.ReportedVulnerability{
		{
			ID: "CVE-2014-2669", Extension: "hstore", Version: "1.3", Level: "ELEVATED", CVSSSeverity: "MEDIUM",
			Description: strings.Repeat("x", 1024), Suggestion: "Upgrade the database engine to version 9.3.3 or later",
			Waiver: &dbv1alpha1.Waiver{Reason: "hstore not reachable from the app role"},
		},
		{
			ID: "CVE-2017-18359", Extension: "postgis", Version: "2.3.1", Level: "HIGH", CVSSSeverity: "HIGH",
			Title: "Buffer overflow in postgis", Suggestion: "Upgrade extension postgis to version 2.3.3 or later",
		},
	}, rep.Report.Vulnerabilities)
	require.Equal(t, []string{
		"Normal Scanned trigger=Spec generation 1: 1 finding (1 HIGH) in 2 extensions; 1 waived; report atlassecurityreport/app",
		"Warning PolicyViolated 1 finding at or above HIGH, highest HIGH; see atlassecurityreport/app",
	}, st.h.events())

	// Nothing pending: no scan, the resource stays ready and is woken at the slot.
	result, res = st.reconcile()
	require.Equal(t, ctrl.Result{RequeueAfter: 90 * time.Minute}, result)
	require.Len(t, st.mock.securityScans, 1)
	require.True(t, res.IsReady())

	// The trigger applies a new desired schema. The revision moved, so the scan runs
	// for Apply, and the routine re-scan flips no condition status.
	schema.Status.ObservedHash = "h2"
	require.NoError(t, st.h.client.Update(context.Background(), schema))
	st.clock = st.clock.Add(5 * time.Minute)
	st.mock.securityScan.res = scanTarget()
	result, res = st.reconcile()
	require.Equal(t, ctrl.Result{RequeueAfter: 85 * time.Minute}, result)
	require.Len(t, st.mock.securityScans, 2)
	require.Equal(t, dbv1alpha1.TriggerApply, res.Status.LastScan.Trigger)
	require.Equal(t, "AtlasSchema/myapp", res.Status.LastScan.TriggeredBy)
	require.Equal(t, "h2", res.Status.Triggers[0].Revision)
	require.Equal(t, "no findings in 2 extensions", cond(t, res, "Ready").Message)
	require.Equal(t, dbv1alpha1.ReasonWithinPolicy, cond(t, res, "Compliant").Reason)
	require.Equal(t, "no findings at or above HIGH; see atlassecurityreport/app", cond(t, res, "Compliant").Message)
	require.Equal(t, []string{
		"Normal Scanned trigger=Apply AtlasSchema/myapp: no findings in 2 extensions; report atlassecurityreport/app",
	}, st.h.events())

	// A no-op re-apply bumps last_applied only. Nothing moves.
	schema.Status.LastApplied = 200
	require.NoError(t, st.h.client.Update(context.Background(), schema))
	st.reconcile()
	require.Len(t, st.mock.securityScans, 2)

	// The slot comes round: the scan runs for Schedule and the slot is recorded.
	st.clock = time.Date(2026, 9, 10, 3, 0, 5, 0, time.UTC)
	result, res = st.reconcile()
	require.Len(t, st.mock.securityScans, 3)
	require.Equal(t, dbv1alpha1.TriggerSchedule, res.Status.LastScan.Trigger)
	require.Equal(t, "2026-09-10T03:00:00Z", res.Status.LastScan.TriggeredBy)
	require.Equal(t, time.Date(2026, 9, 10, 3, 0, 0, 0, time.UTC), res.Status.LastScheduleTime.UTC())
	require.Equal(t, time.Date(2026, 9, 11, 3, 0, 0, 0, time.UTC), res.Status.NextScheduleTime.UTC())
	require.Equal(t, ctrl.Result{RequeueAfter: 24*time.Hour - 5*time.Second}, result)

	// An on-demand scan is a new annotation value, echoed to status on success.
	st.h.patch(t, &dbv1alpha1.AtlasSecurityScan{ObjectMeta: metav1.ObjectMeta{
		Name: "app", Namespace: "test", Annotations: map[string]string{dbv1alpha1.AnnotationScanRequestedAt: "req-1"},
	}})
	_, res = st.reconcile()
	require.Len(t, st.mock.securityScans, 4)
	require.Equal(t, dbv1alpha1.TriggerManual, res.Status.LastScan.Trigger)
	require.Equal(t, "req-1", res.Status.LastScan.TriggeredBy)
	require.Equal(t, "req-1", res.Status.LastHandledScanRequest)
	require.Equal(t, time.Date(2026, 9, 10, 3, 0, 0, 0, time.UTC), res.Status.LastScheduleTime.UTC(), "a scan for another trigger keeps the anchor")
	st.h.events()

	// The database is unreachable at the next slot. The attempt fails with fixed
	// text, the verdict and every watermark stay, and the retry is backed off.
	st.clock = time.Date(2026, 9, 11, 3, 0, 5, 0, time.UTC)
	st.mock.securityScan.res = &atlasexec.SecurityScan{Targets: []*atlasexec.SecurityScanTarget{{
		URL: "postgres://root:xxxxx@db.internal:5432/app?sslmode=disable", Error: "dial tcp 10.0.0.7:5432: connection refused",
	}}}
	st.mock.securityScan.err = atlasexec.ErrSecurityScan
	result, res = st.reconcile()
	require.Equal(t, ctrl.Result{RequeueAfter: retryDuration}, result)
	require.Equal(t, 1, res.Status.Failed)
	ready = cond(t, res, "Ready")
	require.Equal(t, metav1.ConditionFalse, ready.Status)
	require.Equal(t, dbv1alpha1.ReasonScanFailed, ready.Reason)
	require.Equal(t, "the database could not be scanned; attempt 1 of 20; see the operator log", ready.Message)
	require.Equal(t, dbv1alpha1.ReasonRetrying, cond(t, res, "Reconciling").Reason)
	require.Equal(t, metav1.ConditionFalse, cond(t, res, "Stalled").Status)
	require.Equal(t, dbv1alpha1.ReasonWithinPolicy, cond(t, res, "Compliant").Reason, "a failed scan leaves the verdict alone")
	require.Equal(t, dbv1alpha1.ScanFailed, res.Status.LastScan.Result)
	require.Equal(t, time.Date(2026, 9, 10, 3, 0, 0, 0, time.UTC), res.Status.LastScheduleTime.UTC(), "a failed scan advances nothing")
	require.Equal(t, []string{"Warning ScanFailed the database could not be scanned; attempt 1 of 20; see the operator log"}, st.h.events())
	// Neither the host, the resolved address nor the driver error may leak.
	for _, c := range res.Status.Conditions {
		for _, leak := range []string{"db.internal", "10.0.0.7", "connection refused", "xxxxx"} {
			require.NotContains(t, c.Message, leak)
		}
	}
	require.NotContains(t, res.Status.LastScan.Message, "connection refused")

	// The retry succeeds: failures clear and the missed slot is covered by one scan.
	st.mock.securityScan.res, st.mock.securityScan.err = scanTarget(), nil
	st.clock = st.clock.Add(retryDuration)
	result, res = st.reconcile()
	require.Equal(t, ctrl.Result{RequeueAfter: 24*time.Hour - 10*time.Second}, result, "woken at the next slot")
	require.Equal(t, 0, res.Status.Failed)
	require.True(t, res.IsReady())
	require.Equal(t, time.Date(2026, 9, 11, 3, 0, 0, 0, time.UTC), res.Status.LastScheduleTime.UTC())
	require.Equal(t, []string{
		"Normal Scanned trigger=Schedule 2026-09-11T03:00:00Z: no findings in 2 extensions; report atlassecurityreport/app",
	}, st.h.events(), "one slot behind is not a missed slot")

	// Suspend: no scan, no timer, no next slot; Ready and Compliant are kept.
	st.h.patch(t, &dbv1alpha1.AtlasSecurityScan{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "test"},
		Spec: dbv1alpha1.AtlasSecurityScanSpec{Suspend: new(true)}})
	result, res = st.reconcile()
	require.Equal(t, ctrl.Result{}, result)
	require.Len(t, st.mock.securityScans, 6)
	require.Nil(t, res.Status.NextScheduleTime)
	require.Equal(t, dbv1alpha1.ReasonSuspended, cond(t, res, "Reconciling").Reason)
	require.True(t, res.IsReady())
	require.Equal(t, []string{"Normal Suspended scanning suspended"}, st.h.events())

	// Resume is a spec change: one scan covers everything, and says it resumed.
	st.h.patch(t, &dbv1alpha1.AtlasSecurityScan{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "test", Generation: 2},
		Spec: dbv1alpha1.AtlasSecurityScanSpec{Suspend: new(false)}})
	_, res = st.reconcile()
	require.Len(t, st.mock.securityScans, 7)
	require.Equal(t, dbv1alpha1.TriggerSpec, res.Status.LastScan.Trigger)
	require.Equal(t, int64(2), res.Status.ObservedGeneration)
	require.NotNil(t, res.Status.NextScheduleTime)
	require.Equal(t, []string{
		"Normal Resumed scanning resumed",
		"Normal Scanned trigger=Spec generation 2: no findings in 2 extensions; report atlassecurityreport/app",
	}, st.h.events())
}

// Exhausted retries stall the resource, which then makes one attempt per new set
// of inputs while its conditions stay put.
func TestSecurityScan_BackoffLimit(t *testing.T) {
	obj := scanObject()
	obj.Spec.Schedule, obj.Spec.Triggers, obj.Spec.Policy = "", nil, nil
	obj.Spec.BackoffLimit = 1
	st := newScanTest(t, obj, scanSecrets()...)
	st.mock.securityScan.err = errors.New("Abort: atlas security scan is not enabled for your plan.")
	st.reconcile()

	result, res := st.reconcile()
	require.Equal(t, ctrl.Result{RequeueAfter: retryDuration}, result)
	require.Equal(t, 1, res.Status.Failed)
	require.Equal(t, dbv1alpha1.ReasonCLIError, cond(t, res, "Ready").Reason)
	require.Equal(t, "the Atlas CLI failed before producing a report; attempt 1 of 1; see the operator log", cond(t, res, "Ready").Message)

	// The second failure exceeds the limit: stalled, the verdict marked stale,
	// and with neither schedule nor waiver expiry there is no timer.
	result, res = st.reconcile()
	require.Equal(t, ctrl.Result{}, result)
	require.Equal(t, 2, res.Status.Failed)
	require.True(t, res.IsStalled(dbv1alpha1.ReasonBackoffLimitExceeded))
	require.Equal(t, metav1.ConditionFalse, cond(t, res, "Reconciling").Status)
	require.Equal(t, dbv1alpha1.ReasonReportStale, cond(t, res, "Compliant").Reason)
	require.Equal(t, int64(1), res.Status.ObservedGeneration)
	hash := res.Status.LastScan.InputsHash

	// The same inputs are not attempted again.
	st.reconcile()
	require.Len(t, st.mock.securityScans, 2)

	// A new generation is a new set of inputs: exactly one attempt, conditions unchanged.
	st.h.patch(t, &dbv1alpha1.AtlasSecurityScan{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "test", Generation: 2}})
	_, res = st.reconcile()
	require.Len(t, st.mock.securityScans, 3)
	require.Equal(t, 3, res.Status.Failed)
	require.NotEqual(t, hash, res.Status.LastScan.InputsHash)
	require.True(t, res.IsStalled(dbv1alpha1.ReasonBackoffLimitExceeded), "no flapping while stalled")
	st.reconcile()
	require.Len(t, st.mock.securityScans, 3)

	// The first success clears everything.
	st.h.patch(t, &dbv1alpha1.AtlasSecurityScan{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "test", Generation: 3}})
	st.mock.securityScan.res, st.mock.securityScan.err = scanTarget(), nil
	_, res = st.reconcile()
	require.Equal(t, 0, res.Status.Failed)
	require.True(t, res.IsReady())
	require.False(t, res.IsStalled(""))
	require.Equal(t, dbv1alpha1.ReasonNoThreshold, cond(t, res, "Compliant").Reason)
	require.Equal(t, []string{
		"Warning CLIError the Atlas CLI failed before producing a report; attempt 1 of 1; see the operator log",
		"Warning BackoffLimitExceeded backoff limit exceeded; one attempt per new slot, apply, request, spec change or waiver expiry",
		"Normal Scanned trigger=Spec generation 3: no findings in 2 extensions; report atlassecurityreport/app",
	}, st.h.events())
}

// Inputs no scan can fix stall the resource without an attempt or a timer.
func TestSecurityScan_InvalidInputs(t *testing.T) {
	for _, tt := range []struct {
		name, reason, message string
		mutate                func(*dbv1alpha1.AtlasSecurityScan)
		allowCustomConfig     bool
	}{
		{name: "unparsable schedule", reason: "InvalidSchedule", message: "schedule does not parse",
			mutate: func(s *dbv1alpha1.AtlasSecurityScan) { s.Spec.Schedule = "every tuesday" }},
		{name: "interval schedule", reason: "InvalidSchedule", message: "@every is interval-based and drifts",
			mutate: func(s *dbv1alpha1.AtlasSecurityScan) { s.Spec.Schedule = "@every 1h" }},
		{name: "zone prefix", reason: "InvalidSchedule", message: "use spec.timeZone instead of a TZ= prefix",
			mutate: func(s *dbv1alpha1.AtlasSecurityScan) { s.Spec.Schedule = "CRON_TZ=UTC 0 3 * * *" }},
		{name: "never fires", reason: "InvalidSchedule", message: "schedule does not fire",
			mutate: func(s *dbv1alpha1.AtlasSecurityScan) { s.Spec.Schedule = "0 3 30 2 *" }},
		{name: "unknown zone", reason: "InvalidTimeZone", message: "unknown time zone",
			mutate: func(s *dbv1alpha1.AtlasSecurityScan) { s.Spec.TimeZone = "Mars/Olympus" }},
		{name: "no target", reason: "InvalidTarget", message: "no target database or project configuration",
			mutate: func(s *dbv1alpha1.AtlasSecurityScan) { s.Spec.TargetSpec = dbv1alpha1.TargetSpec{} }},
		{name: "custom config disabled", reason: "InvalidTarget", message: "install the operator with allowCustomConfig=true to use a custom atlas.hcl",
			mutate: func(s *dbv1alpha1.AtlasSecurityScan) { s.Spec.Config = `env "prod" {}` }},
		{name: "custom config without env", reason: "InvalidTarget", message: "envName must be set when using a custom atlas.hcl",
			mutate: func(s *dbv1alpha1.AtlasSecurityScan) { s.Spec.Config = `env "prod" {}` }, allowCustomConfig: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			obj := scanObject()
			obj.Spec.Triggers = nil
			tt.mutate(obj)
			st := newScanTest(t, obj, scanSecrets()...)
			if tt.allowCustomConfig {
				st.r.AllowCustomConfig()
			}
			st.reconcile()
			result, res := st.reconcile()
			require.Equal(t, ctrl.Result{}, result)
			require.Empty(t, st.mock.securityScans)
			require.True(t, res.IsStalled(tt.reason))
			require.Equal(t, tt.message, cond(t, res, "Ready").Message)
			require.Equal(t, metav1.ConditionFalse, cond(t, res, "Ready").Status)
			require.Equal(t, int64(1), res.Status.ObservedGeneration, "the generation is observed, so kstatus reads Failed rather than InProgress")
			require.Equal(t, []string{"Warning " + tt.reason + " " + tt.message}, st.h.events())
		})
	}
}

// A config that yields several targets is one database per resource, violated.
func TestSecurityScan_MultipleTargets(t *testing.T) {
	obj := scanObject()
	obj.Spec.Triggers = nil
	st := newScanTest(t, obj, scanSecrets()...)
	st.mock.securityScan.res = &atlasexec.SecurityScan{Targets: []*atlasexec.SecurityScanTarget{
		{URL: "postgres://a", Driver: "postgres"}, {URL: "postgres://b", Driver: "postgres"},
	}}
	st.reconcile()
	_, res := st.reconcile()
	require.True(t, res.IsStalled(dbv1alpha1.ReasonInvalidTarget))
	require.Equal(t, "configuration yields 2 targets; one database per resource", cond(t, res, "Ready").Message)
	require.Equal(t, dbv1alpha1.ScanFailed, res.Status.LastScan.Result)
	require.Equal(t, 0, res.Status.Failed, "not a retryable failure")
}

// A waiver that lapses is a Policy trigger: the verdict is recomputed by a scan.
func TestSecurityScan_WaiverExpiry(t *testing.T) {
	obj := scanObject()
	obj.Spec.Schedule, obj.Spec.Triggers = "", nil
	expiry := time.Date(2026, 9, 10, 2, 30, 0, 0, time.UTC)
	obj.Spec.Policy.Ignore = []dbv1alpha1.IgnoredVulnerability{{
		ID: "CVE-2017-18359", Waiver: dbv1alpha1.Waiver{Reason: "SEC-1234", ExpirationTime: &metav1.Time{Time: expiry}},
	}}
	st := newScanTest(t, obj, scanSecrets()...)
	st.mock.securityScan.res = scanTarget(postgisCVE)
	st.reconcile()

	// Waived: within policy, and woken exactly when the waiver lapses.
	result, res := st.reconcile()
	require.Equal(t, ctrl.Result{RequeueAfter: time.Hour}, result)
	require.Equal(t, dbv1alpha1.ReasonWithinPolicy, cond(t, res, "Compliant").Reason)
	require.Equal(t, []string{"CVE-2017-18359"}, res.Status.ActiveWaivers)
	require.Equal(t, int32(1), res.Status.Summary.Waived)
	waiver := st.report().Report.Vulnerabilities[0].Waiver
	require.Equal(t, "SEC-1234", waiver.Reason)
	require.True(t, waiver.ExpirationTime.Time.Equal(expiry))

	// Lapsed: the set in force differs from the recorded one.
	st.clock = expiry.Add(time.Second)
	result, res = st.reconcile()
	require.Equal(t, ctrl.Result{}, result, "nothing left to wait for")
	require.Len(t, st.mock.securityScans, 2)
	require.Equal(t, dbv1alpha1.TriggerPolicy, res.Status.LastScan.Trigger)
	require.Equal(t, "waivers changed", res.Status.LastScan.TriggeredBy)
	require.Empty(t, res.Status.ActiveWaivers)
	require.Equal(t, dbv1alpha1.ReasonPolicyViolated, cond(t, res, "Compliant").Reason)
	require.Nil(t, st.report().Report.Vulnerabilities[0].Waiver)
}

// A trigger that does not exist is reported, not treated as a failure.
func TestSecurityScan_TriggerNotFound(t *testing.T) {
	obj := scanObject()
	obj.Spec.Schedule = ""
	st := newScanTest(t, obj, scanSecrets()...)
	st.mock.securityScan.res = scanTarget()
	st.reconcile()
	_, res := st.reconcile()
	require.True(t, res.IsReady())
	require.Empty(t, res.Status.Triggers, "nothing recorded for a missing trigger")
	require.Equal(t, []string{
		"Warning TriggerNotFound AtlasSchema/myapp not found in test; it cannot trigger scans until it exists and is managed by this operator instance",
		"Normal Scanned trigger=Spec generation 1: no findings in 2 extensions; report atlassecurityreport/app",
	}, st.h.events())
}

func TestSecurityScan_Schedule(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	require.NoError(t, err)
	daily, err := cron.ParseStandard("0 3 * * *")
	require.NoError(t, err)
	sched := &scanSchedule{Schedule: daily, loc: berlin}
	at := func(y int, m time.Month, d, h, min int) time.Time { return time.Date(y, m, d, h, min, 0, 0, time.UTC) }

	// Across the spring-forward night: 03:00 Berlin exists on every day, but it is
	// 02:00Z before the change and 01:00Z after it.
	slot := latestSlotAtOrBefore(sched, at(2026, 3, 28, 0, 0), at(2026, 3, 30, 12, 0))
	require.NotNil(t, slot)
	require.Equal(t, at(2026, 3, 30, 1, 0), slot.UTC())
	require.Equal(t, 1, slotsBetween(sched, at(2026, 3, 28, 2, 0), *slot), "the 29th was skipped")
	require.Nil(t, latestSlotAtOrBefore(sched, at(2026, 3, 30, 1, 0), at(2026, 3, 30, 12, 0)), "no slot yet")

	// A runaway anchor is capped and re-anchored at now, as the CronJob controller does.
	minutely, err := cron.ParseStandard("* * * * *")
	require.NoError(t, err)
	minute := &scanSchedule{Schedule: minutely, loc: time.UTC}
	now := at(2026, 9, 10, 0, 0)
	slot = latestSlotAtOrBefore(minute, now.AddDate(-1, 0, 0), now)
	require.Equal(t, now, *slot)

	// The capped value is truncated to the second: it reaches the inputs hash, and
	// a wall clock with nanoseconds would differ on every pass.
	sub := now.Add(123 * time.Millisecond)
	slot = latestSlotAtOrBefore(minute, sub.AddDate(-1, 0, 0), sub)
	require.Equal(t, now, *slot)
	require.Zero(t, slot.Nanosecond())

	// Descriptors parse; @every and zone prefixes are refused before parsing.
	r := &AtlasSecurityScanReconciler{}
	for _, expr := range []string{"@daily", "@hourly", "30 6-16/4 * * 1-5"} {
		s, err := r.validate(&dbv1alpha1.AtlasSecurityScan{Spec: dbv1alpha1.AtlasSecurityScanSpec{
			TargetSpec: dbv1alpha1.TargetSpec{URL: "sqlite://x"}, Schedule: expr,
		}}, now)
		require.NoError(t, err, expr)
		require.False(t, s.Next(now).IsZero())
	}
}

// The index fans an apply out to exactly the scans that name the resource.
func TestSecurityScan_Fanout(t *testing.T) {
	scan := func(name string, triggers ...dbv1alpha1.ScanTriggerRef) *dbv1alpha1.AtlasSecurityScan {
		return &dbv1alpha1.AtlasSecurityScan{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test"},
			Spec:       dbv1alpha1.AtlasSecurityScanSpec{Triggers: triggers},
		}
	}
	var (
		a   = dbv1alpha1.ScanTriggerRef{Kind: dbv1alpha1.TriggerKindSchema, Name: "a"}
		b   = dbv1alpha1.ScanTriggerRef{Kind: dbv1alpha1.TriggerKindSchema, Name: "b"}
		mig = dbv1alpha1.ScanTriggerRef{Kind: dbv1alpha1.TriggerKindMigration, Name: "a"}
	)
	var rec *AtlasSecurityScanReconciler
	newRunner(func(m Manager, _ bool) *AtlasSecurityScanReconciler {
		rec = NewAtlasSecurityScanReconciler(m)
		return rec
	}, func(cb *fake.ClientBuilder) {
		cb.WithIndex(&dbv1alpha1.AtlasSecurityScan{}, triggerIndex, triggerKeys)
		cb.WithObjects(scan("s1", a), scan("s2", a, b), scan("s3", b), scan("s4", mig),
			scan("other-ns", a))
		cb.WithObjects(&dbv1alpha1.AtlasSecurityScan{
			ObjectMeta: metav1.ObjectMeta{Name: "s5", Namespace: "elsewhere"},
			Spec:       dbv1alpha1.AtlasSecurityScanSpec{Triggers: []dbv1alpha1.ScanTriggerRef{a}},
		})
	}, &mockAtlasExec{})
	ctx := context.Background()
	names := func(reqs []ctrl.Request) []string {
		var out []string
		for _, r := range reqs {
			out = append(out, r.Name)
		}
		return out
	}
	schemaA := &dbv1alpha1.AtlasSchema{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "test"}}
	require.ElementsMatch(t, []string{"s1", "s2", "other-ns"}, names(rec.scansTriggeredBy(dbv1alpha1.TriggerKindSchema)(ctx, schemaA)))
	require.ElementsMatch(t, []string{"s4"}, names(rec.scansTriggeredBy(dbv1alpha1.TriggerKindMigration)(ctx,
		&dbv1alpha1.AtlasMigration{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "test"}})))
	require.Empty(t, rec.scansTriggeredBy(dbv1alpha1.TriggerKindSchema)(ctx,
		&dbv1alpha1.AtlasSchema{ObjectMeta: metav1.ObjectMeta{Name: "zzz", Namespace: "test"}}))

	// Only a revision change passes, so a last_applied-only update costs nothing.
	rev := func(o client.Object) string { return o.(*dbv1alpha1.AtlasSchema).Status.ObservedHash }
	p := revisionChanged(rev)
	old := &dbv1alpha1.AtlasSchema{Status: dbv1alpha1.AtlasSchemaStatus{ObservedHash: "h1", LastApplied: 1}}
	same := &dbv1alpha1.AtlasSchema{Status: dbv1alpha1.AtlasSchemaStatus{ObservedHash: "h1", LastApplied: 2}}
	changed := &dbv1alpha1.AtlasSchema{Status: dbv1alpha1.AtlasSchemaStatus{ObservedHash: "h2", LastApplied: 2}}
	unapplied := &dbv1alpha1.AtlasSchema{}
	require.False(t, p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: same}))
	require.True(t, p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: changed}))
	require.False(t, p.Delete(event.DeleteEvent{Object: changed}))
	require.True(t, p.Create(event.CreateEvent{Object: changed}))
	require.False(t, p.Create(event.CreateEvent{Object: unapplied}), "nothing has been applied yet")
}

// A config that decides what is reported stalls the resource before the CLI
// runs, and keeps its timer: the config lives outside the spec, so a fix to it
// is not a generation change and nothing else would wake the resource.
func TestSecurityScan_CustomConfigStalls(t *testing.T) {
	obj := scanObject()
	obj.Spec.Triggers = nil
	obj.Spec.EnvName = "kubernetes"
	obj.Spec.Config = `env "kubernetes" {
  security {
    cve {
      ignore = ["CVE-2014-2669"]
    }
  }
}`
	st := newScanTest(t, obj, scanSecrets()...)
	st.r.AllowCustomConfig()
	st.reconcile()
	result, res := st.reconcile()
	const msg = "security.cve.ignore is set by spec.policy and must not appear in the custom atlas.hcl"
	require.Empty(t, st.mock.securityScans, "the CLI never ran")
	require.True(t, res.IsStalled(dbv1alpha1.ReasonInvalidTarget))
	require.Equal(t, msg, cond(t, res, "Ready").Message)
	require.Equal(t, metav1.ConditionFalse, cond(t, res, "Ready").Status)
	require.Equal(t, int64(1), res.Status.ObservedGeneration)
	require.Equal(t, dbv1alpha1.ScanFailed, res.Status.LastScan.Result)
	require.Equal(t, msg, res.Status.LastScan.Message)
	require.Equal(t, 0, res.Status.Failed, "a permanent input error is not retried")
	require.Equal(t, ctrl.Result{RequeueAfter: 90 * time.Minute}, result, "woken at the 03:00 slot")
	require.Equal(t, []string{"Warning InvalidTarget " + msg}, st.h.events())

	// The next slot scans with the policy of the resource.
	st.h.patch(t, &dbv1alpha1.AtlasSecurityScan{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "test"},
		Spec: dbv1alpha1.AtlasSecurityScanSpec{ProjectConfigSpec: dbv1alpha1.ProjectConfigSpec{
			EnvName: "kubernetes", Config: `env "kubernetes" {}`,
		}}})
	st.clock = time.Date(2026, 9, 10, 3, 0, 5, 0, time.UTC)
	st.mock.securityScan.res = scanTarget(hstoreCVE)
	_, res = st.reconcile()
	require.Equal(t, []*atlasexec.SecurityScanParams{{Env: "kubernetes", Vars: atlasexec.Vars2{}, MinSeverity: "ELEVATED"}},
		st.mock.securityScans)
	require.True(t, res.IsReady())
	require.False(t, res.IsStalled(""))
}

// The CLI appends configured ignores to its own and treats a configured
// cve.min_severity as a floor, so a custom config that sets one would filter
// findings before the operator grades them, and the report would describe a
// policy the scan did not run with.
func TestSecurityScan_CustomConfigPolicy(t *testing.T) {
	for _, tt := range []struct {
		name, config, err string
	}{
		{name: "top-level min_severity", err: "security.min_severity is set by spec.policy and must not appear in the custom atlas.hcl",
			config: `security {
  min_severity = CRITICAL
}`},
		{name: "top-level fail_on", err: "security.fail_on is set by spec.policy and must not appear in the custom atlas.hcl",
			config: `security {
  fail_on = NORMAL
}`},
		{name: "env cve ignore", err: "security.cve.ignore is set by spec.policy and must not appear in the custom atlas.hcl",
			config: `env "kubernetes" {
  security {
    cve {
      ignore = ["CVE-2014-2669"]
    }
  }
}`},
		{name: "unlabeled env is the env that runs", err: "security.cve.min_severity is set by spec.policy and must not appear in the custom atlas.hcl",
			config: `env {
  security {
    cve {
      min_severity = CRITICAL
    }
  }
}`},
		{name: "notify is not a policy", config: `env "kubernetes" {
  security {
    notify {
      http "slack" {
        url = var.slack_webhook
      }
    }
  }
}`},
		{name: "query timeout is not a policy", config: `security {
  cve {
    timeout = "10s"
  }
}`},
		{name: "exclude hides extensions from the scan", err: "exclude in the custom atlas.hcl would hide extensions from the scan",
			config: `env "kubernetes" {
  exclude = ["*"]
}`},
		{name: "another env is not the one that runs", config: `env "ci" {
  exclude = ["*"]
  security {
    fail_on = NORMAL
    cve {
      ignore = ["CVE-2014-2669"]
    }
  }
}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := checkPolicy(must(parseConfig(tt.config)), "kubernetes")
			if tt.err == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tt.err)
			perr := (*permanentError)(nil)
			require.True(t, errors.As(err, &perr), "a policy in the config is permanent, not retried")
			require.Equal(t, dbv1alpha1.ReasonInvalidTarget, perr.reason)
		})
	}
	require.NoError(t, checkPolicy(nil, "kubernetes"))
}

// A scan still running when a slot passes covers it: it observed the database
// across that instant. Committing the slot it started from instead left the
// anchor behind, put nextScheduleTime in the past, and on a yearly schedule lost
// the slot for a year. It has to hold before any slot has ever been committed.
func TestSecurityScan_SlotDuringScan(t *testing.T) {
	for _, tt := range []struct {
		name   string
		primed bool
		scans  int
	}{
		{name: "no slot committed yet", scans: 2},
		{name: "after a scheduled scan", primed: true, scans: 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			schema := &dbv1alpha1.AtlasSchema{
				ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "test", UID: types.UID("uid-1")},
				Status:     dbv1alpha1.AtlasSchemaStatus{ObservedHash: "h1"},
			}
			obj := scanObject()
			obj.Spec.Schedule = "0 3 1 1 *"
			st := newScanTest(t, obj, append(scanSecrets(), schema)...)
			st.mock.securityScan.res = scanTarget()
			st.clock = time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC)
			st.reconcile()
			_, res := st.reconcile()
			require.Nil(t, res.Status.LastScheduleTime)
			if tt.primed {
				st.clock = time.Date(2026, 1, 1, 3, 0, 5, 0, time.UTC)
				_, res = st.reconcile()
				require.Equal(t, time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC), res.Status.LastScheduleTime.UTC())
			}

			// An apply starts a scan two seconds before the 2027 slot, and the
			// scan is still running when it passes.
			st.h.patch(t, &dbv1alpha1.AtlasSchema{ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "test"},
				Status: dbv1alpha1.AtlasSchemaStatus{ObservedHash: "h2"}})
			st.clock, st.elapse = time.Date(2027, 1, 1, 2, 59, 58, 0, time.UTC), 5*time.Second
			result, res := st.reconcile()
			st.elapse = 0
			require.Len(t, st.mock.securityScans, tt.scans)
			require.Equal(t, dbv1alpha1.TriggerApply, res.Status.LastScan.Trigger)
			require.Equal(t, time.Date(2027, 1, 1, 3, 0, 0, 0, time.UTC), res.Status.LastScheduleTime.UTC(),
				"the slot the scan ran across is covered by it")
			require.Equal(t, time.Date(2028, 1, 1, 3, 0, 0, 0, time.UTC), res.Status.NextScheduleTime.UTC(),
				"never behind the completion")
			completion := time.Date(2027, 1, 1, 3, 0, 3, 0, time.UTC)
			require.Equal(t, ctrl.Result{RequeueAfter: time.Date(2028, 1, 1, 3, 0, 0, 0, time.UTC).Sub(completion)}, result)

			// The covered slot is not scanned a second time.
			st.clock = time.Date(2027, 1, 1, 3, 0, 20, 0, time.UTC)
			_, res = st.reconcile()
			require.Len(t, st.mock.securityScans, tt.scans)
			require.True(t, res.IsReady())
		})
	}
}

// A failed attempt commits no watermark, so its timer is measured from the
// snapshot. Measuring it from the end would skip a slot the attempt ran across,
// and the stalled resource is promised one attempt per new slot.
func TestSecurityScan_SlotDuringFailedScan(t *testing.T) {
	obj := scanObject()
	obj.Spec.Triggers = nil
	st := newScanTest(t, obj, scanSecrets()...)
	st.mock.securityScan.res = &atlasexec.SecurityScan{Targets: []*atlasexec.SecurityScanTarget{
		{URL: "postgres://a", Driver: "postgres"}, {URL: "postgres://b", Driver: "postgres"},
	}}
	st.reconcile()
	st.clock, st.elapse = time.Date(2026, 9, 10, 2, 59, 58, 0, time.UTC), 5*time.Second
	result, res := st.reconcile()
	st.elapse = 0
	require.True(t, res.IsStalled(dbv1alpha1.ReasonInvalidTarget))
	require.Equal(t, ctrl.Result{RequeueAfter: 2 * time.Second}, result, "the 03:00 slot passed while the attempt ran")
}

// A schedule finer than the scan must pace itself, not queue a scan per slot:
// every catch-up scan would outlast a period again and never settle.
func TestSecurityScan_ScheduleFinerThanScan(t *testing.T) {
	obj := scanObject()
	obj.Spec.Triggers, obj.Spec.Schedule = nil, "*/5 * * * *"
	st := newScanTest(t, obj, scanSecrets()...)
	st.mock.securityScan.res = scanTarget()
	st.clock = time.Date(2026, 9, 10, 10, 10, 0, 0, time.UTC)
	st.reconcile()

	// Each scan takes six minutes, so a slot always passes while it runs.
	st.elapse = 6 * time.Minute
	for range 3 {
		result, res := st.reconcile()
		require.Equal(t, ctrl.Result{RequeueAfter: 4 * time.Minute}, result, "paced by the schedule, not a rescan")
		require.True(t, res.Status.NextScheduleTime.After(res.Status.LastScan.CompletionTime.Time),
			"the next slot is always ahead of the scan that just ended")
		st.clock = res.Status.NextScheduleTime.Time
	}
	require.Len(t, st.mock.securityScans, 3, "one scan per pass, never a rescan of the slot just covered")
	for _, e := range st.h.events() {
		require.NotContains(t, e, dbv1alpha1.EventMissedSchedule,
			"a slot that passes while the scan runs is covered by it, not missed")
	}
}

// A catch-up reports the slots that passed while nothing was scanning, and not
// the ones the catch-up scan itself covered.
func TestSecurityScan_MissedSchedule(t *testing.T) {
	start := time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC)
	obj := scanObject()
	obj.Spec.Triggers, obj.Spec.Schedule = nil, "*/5 * * * *"
	obj.CreationTimestamp = metav1.NewTime(start.Add(-time.Hour))
	st := newScanTest(t, obj, scanSecrets()...)
	st.mock.securityScan.res = scanTarget()
	st.clock = start
	st.reconcile()
	st.h.events() // the first scan catches up the hour since creation
	_, res := st.reconcile()
	require.Equal(t, "2026-09-10T10:00:00Z", res.Status.LastScheduleTime.UTC().Format(time.RFC3339))

	// The operator is away until 10:32, and the catch-up scan itself runs across
	// 10:35. Five slots went by unserved (10:05 to 10:25); 10:30 is the one this
	// scan answers and 10:35 is covered by it, so neither is missed.
	st.h.events() // drain
	st.clock, st.elapse = st.clock.Add(32*time.Minute), 6*time.Minute
	_, res = st.reconcile()
	require.Contains(t, st.h.events(),
		"Normal MissedSchedule caught up 5 missed slots; latest covered 2026-09-10T10:35:00Z")
	require.Equal(t, "2026-09-10T10:35:00Z", res.Status.LastScheduleTime.UTC().Format(time.RFC3339),
		"the anchor still covers the slot that passed during the scan")
}

// The same race on a resource with no schedule: nothing but the expiry itself
// would ever wake it, and wake has already looked past it.
func TestSecurityScan_WaiverExpiryDuringScan(t *testing.T) {
	schema := &dbv1alpha1.AtlasSchema{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "test", UID: types.UID("uid-1")},
		Status:     dbv1alpha1.AtlasSchemaStatus{ObservedHash: "h1"},
	}
	expiry := time.Date(2026, 9, 10, 2, 0, 0, 0, time.UTC)
	obj := scanObject()
	obj.Spec.Schedule = ""
	obj.Spec.Policy.Ignore[0].ExpirationTime = &metav1.Time{Time: expiry}
	st := newScanTest(t, obj, append(scanSecrets(), schema)...)
	st.mock.securityScan.res = scanTarget(hstoreCVE)
	st.reconcile()
	_, res := st.reconcile()
	require.Equal(t, []string{"CVE-2014-2669"}, res.Status.ActiveWaivers)
	require.Equal(t, int32(1), res.Status.Summary.Waived)

	// An apply starts a scan four seconds before the waiver expires.
	st.h.patch(t, &dbv1alpha1.AtlasSchema{ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "test"},
		Status: dbv1alpha1.AtlasSchemaStatus{ObservedHash: "h2"}})
	st.clock, st.elapse = expiry.Add(-4*time.Second), 5*time.Second
	result, res := st.reconcile()
	st.elapse = 0
	require.Equal(t, []string{"CVE-2014-2669"}, res.Status.ActiveWaivers, "in force when the findings were graded")
	require.Equal(t, ctrl.Result{RequeueAfter: time.Second}, result, "the waiver expired while the scan ran")

	// The next pass regrades without it, and the finding now counts.
	result, res = st.reconcile()
	require.Equal(t, dbv1alpha1.TriggerPolicy, res.Status.LastScan.Trigger)
	require.Empty(t, res.Status.ActiveWaivers)
	require.Equal(t, int32(0), res.Status.Summary.Waived)
	require.Equal(t, int32(1), res.Status.Summary.Total)
	require.Equal(t, ctrl.Result{}, result, "nothing left to wait for")
}

// The count the scan carries and the list the report stores are one list, so a
// repeated extension cannot make the scalar exceed it. listType=set would reject
// the duplicate on write anyway.
func TestSecurityScan_DuplicateExtensions(t *testing.T) {
	obj := scanObject()
	obj.Spec.Triggers, obj.Spec.Policy = nil, nil
	st := newScanTest(t, obj, scanSecrets()...)
	st.mock.securityScan.res = &atlasexec.SecurityScan{Targets: []*atlasexec.SecurityScanTarget{{
		Driver: "postgres", Extensions: []string{"hstore", "postgis", "hstore"},
	}}}
	st.reconcile()
	_, res := st.reconcile()
	require.Equal(t, int32(2), res.Status.Summary.Extensions)
	require.Equal(t, []string{"hstore", "postgis"}, st.report().Report.Extensions)
	require.Equal(t, "no findings in 2 extensions", cond(t, res, "Ready").Message)
}

// A resource with no policy still has an effective severity, and the scan runs
// with it, so a configured min_severity is never the one that decides.
func TestSecurityScan_DefaultMinSeverity(t *testing.T) {
	obj := scanObject()
	obj.Spec.Policy, obj.Spec.Triggers = nil, nil
	st := newScanTest(t, obj, scanSecrets()...)
	st.mock.securityScan.res = scanTarget(postgisCVE)
	st.reconcile()
	_, res := st.reconcile()
	require.Equal(t, []*atlasexec.SecurityScanParams{{Env: "kubernetes", Vars: atlasexec.Vars2{}, MinSeverity: "NORMAL"}}, st.mock.securityScans)
	require.Equal(t, dbv1alpha1.GradedPolicy{MinSeverity: dbv1alpha1.SecurityLevelNormal}, st.report().Report.Policy)
	require.True(t, res.IsReady())
	require.Equal(t, dbv1alpha1.ReasonNoThreshold, cond(t, res, "Compliant").Reason)
}

func TestSecurityScan_Render(t *testing.T) {
	var (
		buf  bytes.Buffer
		data = &scanData{
			EnvName: "kubernetes",
			URL:     must(url.Parse("postgres://root:pass@db:5432/app?sslmode=disable")),
			Cloud:   &Cloud{Token: "aci_token"},
		}
	)
	require.NoError(t, data.render(&buf))
	require.Equal(t, `atlas {
  cloud {
    token = "aci_token"
  }
}
env "kubernetes" {
  url = "postgres://root:pass@db:5432/app?sslmode=disable"
}
`, buf.String())

	// A custom config is merged into the env of the scan.
	buf.Reset()
	data.Config = must(parseConfig(`
env "kubernetes" {
  security {
    notify {
      http "slack" {
        url  = var.slack_webhook
        body = jsonencode({ text = "${scan.count} vulnerable extensions found" })
      }
    }
  }
}`))
	require.NoError(t, data.render(&buf))
	require.Equal(t, `atlas {
  cloud {
    token = "aci_token"
  }
}
env "kubernetes" {
  url = "postgres://root:pass@db:5432/app?sslmode=disable"
  security {
    notify {
      http "slack" {
        url  = var.slack_webhook
        body = jsonencode({ text = "${scan.count} vulnerable extensions found" })
      }
    }
  }
}
`, buf.String())

	data = &scanData{EnvName: "prod", Config: must(parseConfig(`env "prod" {}`))}
	require.EqualError(t, data.render(&buf), "database url is not set")
}

// A stall on an input the user has since fixed must not swallow the next
// attempt's retry: a trigger-only resource has only the backoff to wake it.
func TestSecurityScan_RetryAfterInputStall(t *testing.T) {
	schema := &dbv1alpha1.AtlasSchema{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "test", UID: types.UID("uid-1")},
		Status:     dbv1alpha1.AtlasSchemaStatus{ObservedHash: "h1"},
	}
	obj := scanObject()
	obj.Spec.Schedule = ""
	obj.Spec.Policy.Ignore = nil
	st := newScanTest(t, obj, append(scanSecrets(), schema)...)

	// A configuration yielding two targets stalls the resource with no timer.
	st.mock.securityScan.res = &atlasexec.SecurityScan{Targets: []*atlasexec.SecurityScanTarget{
		{URL: "postgres://a", Driver: "postgres"}, {URL: "postgres://b", Driver: "postgres"},
	}}
	st.reconcile()
	result, res := st.reconcile()
	require.True(t, res.IsStalled(dbv1alpha1.ReasonInvalidTarget))
	require.Equal(t, ctrl.Result{}, result)
	st.h.events()

	// The target is fixed, an apply makes it due, and this attempt fails transiently.
	st.mock.securityScan.res = &atlasexec.SecurityScan{Targets: []*atlasexec.SecurityScanTarget{
		{URL: "postgres://a", Driver: "postgres", Error: "dial tcp: connection refused"},
	}}
	st.mock.securityScan.err = atlasexec.ErrSecurityScan
	st.h.patch(t, &dbv1alpha1.AtlasSchema{ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "test"},
		Status: dbv1alpha1.AtlasSchemaStatus{ObservedHash: "h2"}})
	result, res = st.reconcile()
	require.Equal(t, 1, res.Status.Failed)
	require.False(t, res.IsStalled(""), "a transient failure is not a stall")
	require.Equal(t, dbv1alpha1.ReasonRetrying, cond(t, res, "Reconciling").Reason)
	require.Equal(t, dbv1alpha1.ReasonScanFailed, cond(t, res, "Ready").Reason)
	require.Equal(t, ctrl.Result{RequeueAfter: retryDuration}, result, "the backoff is the only thing that wakes it")
	require.Equal(t, []string{"Warning ScanFailed the database could not be scanned; attempt 1 of 20; see the operator log"}, st.h.events())
}

// Swept at every offset, so a cut inside a three or four byte character is covered.
func TestSecurityScan_Truncate(t *testing.T) {
	// "l" is one byte, "ỗ" is three, "🔒" is four.
	const s = "lỗ hổng bảo mật 🔒"
	require.Equal(t, s, truncate(s, len(s)))
	require.Equal(t, s, truncate(s, len(s)+10))
	for n := range len(s) {
		got := truncate(s, n)
		require.LessOrEqual(t, len(got), n)
		require.True(t, utf8.ValidString(got), "cut at %d is not valid UTF-8: %q", n, got)
		require.True(t, strings.HasPrefix(s, got), "cut at %d is not a prefix: %q", n, got)
	}
	require.Equal(t, "l", truncate(s, 2), "the second byte is inside ỗ")
	require.Equal(t, "lỗ hổng bảo mật ", truncate(s, len(s)-1), "the trailing emoji goes whole")
	require.Equal(t, "", truncate("🔒", 3))
}

// A trigger recreated under the same name is a different object, so its scan is
// due even when the revision it applied is unchanged.
func TestSecurityScan_TriggerRecreated(t *testing.T) {
	schema := &dbv1alpha1.AtlasSchema{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "test", UID: types.UID("uid-1")},
		Status:     dbv1alpha1.AtlasSchemaStatus{ObservedHash: "h1"},
	}
	obj := scanObject()
	obj.Spec.Schedule = ""
	st := newScanTest(t, obj, append(scanSecrets(), schema)...)
	st.mock.securityScan.res = scanTarget()
	st.reconcile()
	_, res := st.reconcile()
	require.Equal(t, []dbv1alpha1.ObservedTrigger{{Kind: "AtlasSchema", Name: "myapp", UID: "uid-1", Revision: "h1"}},
		res.Status.Triggers)
	require.Len(t, st.mock.securityScans, 1)

	st.h.patch(t, &dbv1alpha1.AtlasSchema{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "test", UID: types.UID("uid-2")},
		Status:     dbv1alpha1.AtlasSchemaStatus{ObservedHash: "h1"},
	})
	_, res = st.reconcile()
	require.Len(t, st.mock.securityScans, 2, "a different object is a pending trigger")
	require.Equal(t, dbv1alpha1.TriggerApply, res.Status.LastScan.Trigger)
	require.Equal(t, []dbv1alpha1.ObservedTrigger{{Kind: "AtlasSchema", Name: "myapp", UID: "uid-2", Revision: "h1"}},
		res.Status.Triggers)
}

// A crash between the two status writes must not spend the one attempt a stalled
// resource earns for its inputs.
func TestSecurityScan_InterruptedStalledAttempt(t *testing.T) {
	obj := scanObject()
	obj.Spec.Triggers, obj.Spec.Schedule, obj.Spec.BackoffLimit = nil, "", 1
	obj.Spec.Policy.Ignore = nil
	mock := &mockAtlasExec{}
	mock.securityScan.res = &atlasexec.SecurityScan{Targets: []*atlasexec.SecurityScanTarget{
		{URL: "postgres://a", Driver: "postgres", Error: "connection refused"},
	}}
	mock.securityScan.err = atlasexec.ErrSecurityScan
	var crash bool
	h, reconcile := newRunner(func(m Manager, _ bool) *AtlasSecurityScanReconciler {
		return NewAtlasSecurityScanReconciler(m)
	}, func(cb *fake.ClientBuilder) {
		cb.WithStatusSubresource(obj)
		cb.WithObjects(obj)
		cb.WithObjects(scanSecrets()...)
		cb.WithInterceptorFuncs(interceptor.Funcs{
			// Lose the write that closes the attempt, keeping the one that opened it.
			SubResourceUpdate: func(ctx context.Context, c client.Client, s string, o client.Object, opts ...client.SubResourceUpdateOption) error {
				if sc, ok := o.(*dbv1alpha1.AtlasSecurityScan); ok && crash &&
					sc.Status.LastScan != nil && sc.Status.LastScan.CompletionTime != nil {
					return errors.New("etcdserver: request timed out")
				}
				return c.Status().Update(ctx, o, opts...)
			},
		})
	}, mock)
	get := func() *dbv1alpha1.AtlasSecurityScan {
		res := &dbv1alpha1.AtlasSecurityScan{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "test"}}
		h.get(t, res)
		return res
	}
	// Two failures exhaust a backoffLimit of one.
	for range 3 {
		reconcile(obj, func(ctrl.Result, error) {})
	}
	require.True(t, get().IsStalled(dbv1alpha1.ReasonBackoffLimitExceeded))
	require.Len(t, mock.securityScans, 2)

	// A spec change earns one new attempt, whose closing write is lost.
	h.patch(t, &dbv1alpha1.AtlasSecurityScan{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "test", Generation: 2},
		Spec:       dbv1alpha1.AtlasSecurityScanSpec{TimeZone: "UTC", BackoffLimit: 1},
	})
	crash = true
	reconcile(obj, func(_ ctrl.Result, err error) {
		require.EqualError(t, err, "updating resource status: etcdserver: request timed out")
	})
	crash = false
	require.Len(t, mock.securityScans, 3)
	require.Nil(t, get().Status.LastScan.CompletionTime, "the attempt is left open")

	reconcile(obj, func(ctrl.Result, error) {})
	require.Len(t, mock.securityScans, 4, "the interrupted attempt did not count")
	require.NotNil(t, get().Status.LastScan.CompletionTime)
	for range 3 {
		reconcile(obj, func(result ctrl.Result, err error) {
			require.NoError(t, err)
			require.Equal(t, ctrl.Result{}, result)
		})
	}
	require.Len(t, mock.securityScans, 4, "one attempt per input set, once it finishes")
}

// Only NotFound proves a trigger is absent; any other error reaches the workqueue.
func TestSecurityScan_TriggerGetFails(t *testing.T) {
	obj := scanObject()
	obj.Spec.Schedule = ""
	mock := &mockAtlasExec{}
	h, reconcile := newRunner(func(m Manager, _ bool) *AtlasSecurityScanReconciler {
		return NewAtlasSecurityScanReconciler(m)
	}, func(cb *fake.ClientBuilder) {
		cb.WithStatusSubresource(obj)
		cb.WithObjects(obj)
		cb.WithObjects(scanSecrets()...)
		cb.WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
				if _, ok := o.(*dbv1alpha1.AtlasSchema); ok {
					return errors.New("failed to get informer from cache")
				}
				return c.Get(ctx, key, o, opts...)
			},
		})
	}, mock)
	reconcile(obj, func(_ ctrl.Result, err error) { require.NoError(t, err) })
	reconcile(obj, func(result ctrl.Result, err error) {
		require.EqualError(t, err, "failed to get informer from cache")
		require.Equal(t, ctrl.Result{}, result, "the workqueue retries it, not a timer")
	})
	require.Empty(t, mock.securityScans, "no scan ran")
	require.Empty(t, h.events(), "an unreachable API server is not a missing trigger")
}

// A status the API never received is not left stale: the write error fails the
// reconcile so the workqueue retries it.
func TestSecurityScan_StatusUpdateFails(t *testing.T) {
	obj := scanObject()
	_, reconcile := newRunner(func(m Manager, _ bool) *AtlasSecurityScanReconciler {
		return NewAtlasSecurityScanReconciler(m)
	}, func(cb *fake.ClientBuilder) {
		cb.WithStatusSubresource(obj)
		cb.WithObjects(obj)
		cb.WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return errors.New("etcdserver: request timed out")
			},
		})
	}, &mockAtlasExec{})
	reconcile(obj, func(_ ctrl.Result, err error) {
		require.EqualError(t, err, "updating resource status: etcdserver: request timed out")
	})
}

func parseConfig(s string) (*hclwrite.File, error) {
	f, diags := hclwrite.ParseConfig([]byte(s), "", hcl.InitialPos)
	if diags.HasErrors() {
		return nil, diags
	}
	return f, nil
}
