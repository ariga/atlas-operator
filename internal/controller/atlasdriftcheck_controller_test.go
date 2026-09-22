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
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"ariga.io/atlas/atlasexec"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
)

const driftInterval = 5 * time.Minute

// driftTarget returns an AtlasMigration in registry mode that applied version 2
// and is not currently applying anything.
func driftTarget() *dbv1alpha1.AtlasMigration {
	return &dbv1alpha1.AtlasMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "migration", Namespace: "default"},
		Spec: dbv1alpha1.AtlasMigrationSpec{
			TargetSpec: dbv1alpha1.TargetSpec{URL: "sqlite://file?mode=memory"},
			Cloud: dbv1alpha1.CloudV0{TokenFrom: dbv1alpha1.TokenFrom{
				SecretKeyRef: &corev1.SecretKeySelector{
					Key:                  "token",
					LocalObjectReference: corev1.LocalObjectReference{Name: "my-secret"},
				},
			}},
			Dir: dbv1alpha1.Dir{Remote: dbv1alpha1.Remote{Name: "my-dir", Tag: "v2"}},
		},
		Status: dbv1alpha1.AtlasMigrationStatus{
			LastAppliedVersion: "2",
			Conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue, Reason: dbv1alpha1.ReasonApplied, LastTransitionTime: metav1.Now()},
				{Type: "Reconciling", Status: metav1.ConditionFalse, Reason: dbv1alpha1.ReasonApplied, LastTransitionTime: metav1.Now()},
				{Type: "Stalled", Status: metav1.ConditionFalse, Reason: dbv1alpha1.ReasonApplied, LastTransitionTime: metav1.Now()},
			},
		},
	}
}

// driftCheckObj returns an AtlasDriftCheck pointing at driftTarget.
func driftCheckObj() *dbv1alpha1.AtlasDriftCheck {
	return &dbv1alpha1.AtlasDriftCheck{
		ObjectMeta: metav1.ObjectMeta{Name: "check", Namespace: "default", Generation: 1},
		Spec: dbv1alpha1.AtlasDriftCheckSpec{
			TargetRef: dbv1alpha1.DriftCheckTarget{Name: "migration"},
			Interval:  metav1.Duration{Duration: driftInterval},
			Timeout:   metav1.Duration{Duration: time.Minute},
			OnDrift:   dbv1alpha1.DriftActionReport,
		},
	}
}

func driftTokenSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("my-token")},
	}
}

// newDriftRunner wires an AtlasDriftCheckReconciler whose requeue delays are
// not jittered, so the tests can assert on them exactly.
func newDriftRunner(check *dbv1alpha1.AtlasDriftCheck, mock *mockAtlasExec, objs ...client.Object) (*helper, runner) {
	return newRunner(func(mgr Manager, prewarmDevDB bool) *AtlasDriftCheckReconciler {
		r := NewAtlasDriftCheckReconciler(mgr, prewarmDevDB)
		r.jitter = func(d time.Duration) time.Duration { return d }
		return r
	}, func(cb *fake.ClientBuilder) {
		cb.WithStatusSubresource(check)
		cb.WithObjects(append([]client.Object{check}, objs...)...)
	}, mock)
}

// driftCond returns the condition of the given type, failing when it is absent.
func driftCond(t *testing.T, res *dbv1alpha1.AtlasDriftCheck, typ string) metav1.Condition {
	t.Helper()
	cond := meta.FindStatusCondition(res.Status.Conditions, typ)
	require.NotNil(t, cond, "condition %s not found", typ)
	return *cond
}

// requireCond asserts the status and reason of a condition.
func requireCond(t *testing.T, res *dbv1alpha1.AtlasDriftCheck, typ string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	cond := driftCond(t, res, typ)
	require.Equal(t, status, cond.Status, "condition %s: %s", typ, cond.Message)
	require.Equal(t, reason, cond.Reason, "condition %s: %s", typ, cond.Message)
}

func TestDriftCheck_Clean(t *testing.T) {
	var (
		check = driftCheckObj()
		mock  = &mockAtlasExec{}
	)
	mock.drift.res = []*atlasexec.MigrateDrift{{Mode: "registry", Version: "2"}}
	h, reconcile := newDriftRunner(check, mock, driftTarget(), driftTokenSecret())
	reconcile(check, func(result ctrl.Result, err error) {
		require.NoError(t, err)
		require.Equal(t, ctrl.Result{RequeueAfter: driftInterval}, result)
	})
	res := &dbv1alpha1.AtlasDriftCheck{ObjectMeta: check.ObjectMeta}
	h.get(t, res)
	requireCond(t, res, "Ready", metav1.ConditionTrue, dbv1alpha1.ReasonChecked)
	requireCond(t, res, "Reconciling", metav1.ConditionFalse, dbv1alpha1.ReasonChecked)
	requireCond(t, res, "Stalled", metav1.ConditionFalse, dbv1alpha1.ReasonChecked)
	requireCond(t, res, "Drifted", metav1.ConditionFalse, dbv1alpha1.ReasonNoDrift)
	require.Equal(t, "2", res.Status.Version)
	require.Equal(t, "registry", res.Status.Mode)
	require.Empty(t, res.Status.Fingerprint)
	require.Nil(t, res.Status.Summary)
	require.NotNil(t, res.Status.LastCheckTime)
	require.Equal(t, int64(1), res.Status.ObservedGeneration)
	require.Empty(t, h.events())
	// The stderr writer is detached from the client after every run.
	require.Nil(t, mock.stderrW)
}

func TestDriftCheck_DriftLifecycle(t *testing.T) {
	var (
		check = driftCheckObj()
		mock  = &mockAtlasExec{}
		res   = &dbv1alpha1.AtlasDriftCheck{ObjectMeta: check.ObjectMeta}
	)
	report := func(fingerprint string) *atlasexec.MigrateDrift {
		return &atlasexec.MigrateDrift{
			Mode: "registry", Version: "2", Drifted: true, Fingerprint: fingerprint,
			Summary: &atlasexec.MigrateDriftSummary{Total: 1, Extra: 1, Types: map[string]int{"table": 1}},
		}
	}
	h, reconcile := newDriftRunner(check, mock, driftTarget(), driftTokenSecret())
	run := func() {
		t.Helper()
		reconcile(check, func(result ctrl.Result, err error) {
			t.Helper()
			require.NoError(t, err)
			require.Equal(t, ctrl.Result{RequeueAfter: driftInterval}, result)
		})
		h.get(t, res)
	}
	// Drift is found: the check stays ready and reports it once.
	mock.drift.res = []*atlasexec.MigrateDrift{report("fp1")}
	run()
	requireCond(t, res, "Ready", metav1.ConditionTrue, dbv1alpha1.ReasonChecked)
	requireCond(t, res, "Stalled", metav1.ConditionFalse, dbv1alpha1.ReasonChecked)
	requireCond(t, res, "Drifted", metav1.ConditionTrue, dbv1alpha1.ReasonDriftDetected)
	require.Equal(t, "fp1", res.Status.Fingerprint)
	require.Equal(t, &dbv1alpha1.DriftSummary{Total: 1, Extra: 1, Types: map[string]int{"table": 1}}, res.Status.Summary)
	const msg = "1 drifted object (extra 1) at version 2: table 1"
	require.Equal(t, []string{"Warning DriftDetected " + msg}, h.events())
	// The same drift on the next run is not reported again.
	run()
	require.Empty(t, h.events())
	// Drift that changed is reported again.
	mock.drift.res = []*atlasexec.MigrateDrift{report("fp2")}
	run()
	require.Equal(t, "fp2", res.Status.Fingerprint)
	require.Equal(t, []string{"Warning DriftChanged " + msg}, h.events())
	// The drift is gone.
	mock.drift.res = []*atlasexec.MigrateDrift{{Mode: "registry", Version: "2"}}
	run()
	requireCond(t, res, "Drifted", metav1.ConditionFalse, dbv1alpha1.ReasonNoDrift)
	require.Empty(t, res.Status.Fingerprint)
	require.Nil(t, res.Status.Summary)
	require.Equal(t, []string{"Normal DriftResolved no drift detected at version 2"}, h.events())
}

func TestDriftCheck_OnDriftFail(t *testing.T) {
	var (
		check = driftCheckObj()
		mock  = &mockAtlasExec{}
	)
	check.Spec.OnDrift = dbv1alpha1.DriftActionFail
	mock.drift.res = []*atlasexec.MigrateDrift{{
		Mode: "registry", Version: "2", Drifted: true, Fingerprint: "fp1",
		Summary: &atlasexec.MigrateDriftSummary{Total: 2, Missing: 1, Modified: 1, Types: map[string]int{"table": 2}},
	}}
	h, reconcile := newDriftRunner(check, mock, driftTarget(), driftTokenSecret())
	reconcile(check, func(result ctrl.Result, err error) {
		require.NoError(t, err)
		// A failing check still runs on its interval; it is not a dead end.
		require.Equal(t, ctrl.Result{RequeueAfter: driftInterval}, result)
	})
	res := &dbv1alpha1.AtlasDriftCheck{ObjectMeta: check.ObjectMeta}
	h.get(t, res)
	requireCond(t, res, "Ready", metav1.ConditionFalse, dbv1alpha1.ReasonDriftDetected)
	requireCond(t, res, "Reconciling", metav1.ConditionFalse, dbv1alpha1.ReasonDriftDetected)
	requireCond(t, res, "Stalled", metav1.ConditionTrue, dbv1alpha1.ReasonDriftDetected)
	requireCond(t, res, "Drifted", metav1.ConditionTrue, dbv1alpha1.ReasonDriftDetected)
	// The drift event covers it; no second event for the Ready condition.
	require.Equal(t, []string{
		"Warning DriftDetected 2 drifted objects (missing 1, modified 1) at version 2: table 2",
	}, h.events())
}

func TestDriftCheck_Contended(t *testing.T) {
	var (
		check = driftCheckObj()
		mock  = &mockAtlasExec{}
		res   = &dbv1alpha1.AtlasDriftCheck{ObjectMeta: check.ObjectMeta}
	)
	mock.drift.res = []*atlasexec.MigrateDrift{{
		Mode: "registry", Version: "2", Drifted: true, Fingerprint: "fp1",
		Summary: &atlasexec.MigrateDriftSummary{Total: 1, Extra: 1},
	}}
	h, reconcile := newDriftRunner(check, mock, driftTarget(), driftTokenSecret())
	reconcile(check, func(_ ctrl.Result, err error) { require.NoError(t, err) })
	h.get(t, res)
	before := res.Status
	require.Len(t, h.events(), 1)
	// A run that could not take the lock leaves the previous result alone.
	mock.drift.err = &atlasexec.MigrateDriftError{
		Result: []*atlasexec.MigrateDrift{{Error: "acquiring database lock: timeout exceeded"}},
	}
	reconcile(check, func(result ctrl.Result, err error) {
		require.NoError(t, err)
		require.Equal(t, ctrl.Result{RequeueAfter: busyDriftRetry}, result)
	})
	h.get(t, res)
	require.Equal(t, before, res.Status)
	require.Empty(t, h.events())
}

func TestDriftCheck_PermanentFailures(t *testing.T) {
	var (
		check = driftCheckObj()
		mock  = &mockAtlasExec{}
		res   = &dbv1alpha1.AtlasDriftCheck{ObjectMeta: check.ObjectMeta}
	)
	h, reconcile := newDriftRunner(check, mock, driftTarget(), driftTokenSecret())
	run := func(err error) {
		t.Helper()
		mock.drift.err = err
		reconcile(check, func(result ctrl.Result, err error) {
			t.Helper()
			require.NoError(t, err)
			require.Equal(t, ctrl.Result{RequeueAfter: driftInterval}, result)
		})
		h.get(t, res)
	}
	const noHistory = "Error: no migration history found on the connected database"
	run(&atlasexec.Error{Stderr: noHistory})
	requireCond(t, res, "Ready", metav1.ConditionFalse, dbv1alpha1.ReasonNoMigrationHistory)
	requireCond(t, res, "Stalled", metav1.ConditionTrue, dbv1alpha1.ReasonNoMigrationHistory)
	requireCond(t, res, "Reconciling", metav1.ConditionFalse, dbv1alpha1.ReasonNoMigrationHistory)
	requireCond(t, res, "Drifted", metav1.ConditionUnknown, dbv1alpha1.ReasonNoMigrationHistory)
	require.Equal(t, []string{"Warning CheckFailed " + noHistory}, h.events())
	// The same failure is not reported twice.
	run(&atlasexec.Error{Stderr: noHistory})
	require.Empty(t, h.events())
	// The Pro gate.
	const pro = "Abort: command 'atlas migrate drift' is available only to Atlas Pro users"
	run(&atlasexec.Error{Stderr: pro})
	requireCond(t, res, "Ready", metav1.ConditionFalse, dbv1alpha1.ReasonCheckFailed)
	requireCond(t, res, "Stalled", metav1.ConditionTrue, dbv1alpha1.ReasonCheckFailed)
	require.Equal(t, []string{"Warning CheckFailed " + pro}, h.events())
	// An Atlas CLI that predates the command.
	const unknown = `unknown command "drift" for "atlas migrate"`
	run(errors.New(unknown))
	requireCond(t, res, "Ready", metav1.ConditionFalse, dbv1alpha1.ReasonCheckFailed)
	requireCond(t, res, "Stalled", metav1.ConditionTrue, dbv1alpha1.ReasonCheckFailed)
	require.Equal(t, []string{"Warning CheckFailed " + unknown}, h.events())
}

func TestDriftCheck_TransientFailures(t *testing.T) {
	var (
		check = driftCheckObj()
		mock  = &mockAtlasExec{}
		res   = &dbv1alpha1.AtlasDriftCheck{ObjectMeta: check.ObjectMeta}
	)
	h, reconcile := newDriftRunner(check, mock, driftTarget(), driftTokenSecret())
	run := func(err error) {
		t.Helper()
		mock.drift.err = err
		reconcile(check, func(result ctrl.Result, err error) {
			t.Helper()
			require.NoError(t, err)
			// A transient failure is retried sooner than the interval.
			require.Equal(t, ctrl.Result{RequeueAfter: transientDriftRetry}, result)
		})
		h.get(t, res)
	}
	run(errors.New("dial tcp: connection refused"))
	requireCond(t, res, "Ready", metav1.ConditionFalse, dbv1alpha1.ReasonCheckFailed)
	requireCond(t, res, "Reconciling", metav1.ConditionTrue, dbv1alpha1.ReasonCheckFailed)
	requireCond(t, res, "Stalled", metav1.ConditionFalse, dbv1alpha1.ReasonCheckFailed)
	requireCond(t, res, "Drifted", metav1.ConditionUnknown, dbv1alpha1.ReasonCheckFailed)
	require.Equal(t, []string{"Warning CheckFailed dial tcp: connection refused"}, h.events())
	// A run that ran out of time is transient too.
	run(context.DeadlineExceeded)
	requireCond(t, res, "Ready", metav1.ConditionFalse, dbv1alpha1.ReasonCheckFailed)
	requireCond(t, res, "Stalled", metav1.ConditionFalse, dbv1alpha1.ReasonCheckFailed)
	require.Contains(t, driftCond(t, res, "Ready").Message, "the drift check timed out")
}

func TestDriftCheck_MultiTarget(t *testing.T) {
	var (
		check = driftCheckObj()
		mock  = &mockAtlasExec{}
	)
	// Two reports: the environment expanded into more than one target.
	mock.drift.res = []*atlasexec.MigrateDrift{{Version: "2"}, {Version: "2"}}
	h, reconcile := newDriftRunner(check, mock, driftTarget(), driftTokenSecret())
	reconcile(check, func(result ctrl.Result, err error) {
		require.NoError(t, err)
		require.Equal(t, ctrl.Result{RequeueAfter: driftInterval}, result)
	})
	res := &dbv1alpha1.AtlasDriftCheck{ObjectMeta: check.ObjectMeta}
	h.get(t, res)
	requireCond(t, res, "Ready", metav1.ConditionFalse, dbv1alpha1.ReasonCheckFailed)
	requireCond(t, res, "Stalled", metav1.ConditionTrue, dbv1alpha1.ReasonCheckFailed)
	require.Equal(t, "unexpected number of reports: 2; for_each targets are not supported",
		driftCond(t, res, "Ready").Message)
}

func TestDriftCheck_TargetReconciling(t *testing.T) {
	var (
		check  = driftCheckObj()
		target = driftTarget()
		mock   = &mockAtlasExec{}
		res    = &dbv1alpha1.AtlasDriftCheck{ObjectMeta: check.ObjectMeta}
		ctx    = context.Background()
	)
	mock.drift.res = []*atlasexec.MigrateDrift{{
		Mode: "registry", Version: "2", Drifted: true, Fingerprint: "fp1",
		Summary: &atlasexec.MigrateDriftSummary{Total: 1, Extra: 1},
	}}
	h, reconcile := newDriftRunner(check, mock, target, driftTokenSecret())
	reconcile(check, func(_ ctrl.Result, err error) { require.NoError(t, err) })
	h.get(t, res)
	requireCond(t, res, "Drifted", metav1.ConditionTrue, dbv1alpha1.ReasonDriftDetected)
	require.Len(t, h.events(), 1)
	// An apply is now in flight: the check is skipped, not failed.
	target.SetReconciling("applying")
	require.NoError(t, h.client.Update(ctx, target))
	reconcile(check, func(result ctrl.Result, err error) {
		require.NoError(t, err)
		require.Equal(t, ctrl.Result{RequeueAfter: busyDriftRetry}, result)
	})
	h.get(t, res)
	requireCond(t, res, "Reconciling", metav1.ConditionTrue, dbv1alpha1.ReasonTargetNotReady)
	// The previous finding stays visible.
	requireCond(t, res, "Drifted", metav1.ConditionTrue, dbv1alpha1.ReasonDriftDetected)
	requireCond(t, res, "Ready", metav1.ConditionTrue, dbv1alpha1.ReasonChecked)
	require.Equal(t, "fp1", res.Status.Fingerprint)
	require.Empty(t, h.events())
}

func TestDriftCheck_TargetStalled(t *testing.T) {
	var (
		check  = driftCheckObj()
		target = driftTarget()
		mock   = &mockAtlasExec{}
	)
	// A migration that cannot be applied is exactly when drift matters, so a
	// stalled target is still checked.
	target.SetNotReady(dbv1alpha1.ReasonMigrating, "boom")
	mock.drift.res = []*atlasexec.MigrateDrift{{Mode: "registry", Version: "2"}}
	h, reconcile := newDriftRunner(check, mock, target, driftTokenSecret())
	reconcile(check, func(result ctrl.Result, err error) {
		require.NoError(t, err)
		require.Equal(t, ctrl.Result{RequeueAfter: driftInterval}, result)
	})
	res := &dbv1alpha1.AtlasDriftCheck{ObjectMeta: check.ObjectMeta}
	h.get(t, res)
	requireCond(t, res, "Ready", metav1.ConditionTrue, dbv1alpha1.ReasonChecked)
	requireCond(t, res, "Drifted", metav1.ConditionFalse, dbv1alpha1.ReasonNoDrift)
}

func TestDriftCheck_TargetNotFound(t *testing.T) {
	check := driftCheckObj()
	h, reconcile := newDriftRunner(check, &mockAtlasExec{})
	reconcile(check, func(result ctrl.Result, err error) {
		require.NoError(t, err)
		require.Equal(t, ctrl.Result{RequeueAfter: driftInterval}, result)
	})
	res := &dbv1alpha1.AtlasDriftCheck{ObjectMeta: check.ObjectMeta}
	h.get(t, res)
	requireCond(t, res, "Ready", metav1.ConditionFalse, dbv1alpha1.ReasonTargetNotFound)
	requireCond(t, res, "Stalled", metav1.ConditionTrue, dbv1alpha1.ReasonTargetNotFound)
	requireCond(t, res, "Drifted", metav1.ConditionUnknown, dbv1alpha1.ReasonTargetNotFound)
	require.Contains(t, driftCond(t, res, "Ready").Message, `AtlasMigration "migration" not found`)
	require.Contains(t, driftCond(t, res, "Ready").Message, "--label-selector")
	require.Len(t, h.events(), 1)
}

func TestDriftCheck_Suspended(t *testing.T) {
	check := driftCheckObj()
	check.Spec.Suspend = true
	h, reconcile := newDriftRunner(check, &mockAtlasExec{}, driftTarget(), driftTokenSecret())
	reconcile(check, func(result ctrl.Result, err error) {
		require.NoError(t, err)
		require.Equal(t, ctrl.Result{}, result)
	})
	res := &dbv1alpha1.AtlasDriftCheck{ObjectMeta: check.ObjectMeta}
	h.get(t, res)
	require.Equal(t, check.Generation, res.Status.ObservedGeneration)
	requireCond(t, res, "Reconciling", metav1.ConditionFalse, dbv1alpha1.ReasonSuspended)
	require.Nil(t, meta.FindStatusCondition(res.Status.Conditions, "Drifted"))
	require.Empty(t, h.events())
}

func TestDriftCheck_LocalDirWithoutDevURL(t *testing.T) {
	var (
		check  = driftCheckObj()
		target = driftTarget()
	)
	target.Spec.Cloud = dbv1alpha1.CloudV0{}
	target.Spec.Dir = dbv1alpha1.Dir{Local: map[string]string{
		"1.sql":              "CREATE TABLE t (id int);",
		"atlas.sum":          "h1:MOCK=\n1.sql h1:MOCK=\n",
		"20230412003626.sql": "CREATE TABLE t2 (id int);",
	}}
	h, reconcile := newDriftRunner(check, &mockAtlasExec{}, target)
	reconcile(check, func(result ctrl.Result, err error) {
		require.NoError(t, err)
		require.Equal(t, ctrl.Result{RequeueAfter: driftInterval}, result)
	})
	res := &dbv1alpha1.AtlasDriftCheck{ObjectMeta: check.ObjectMeta}
	h.get(t, res)
	requireCond(t, res, "Ready", metav1.ConditionFalse, dbv1alpha1.ReasonCheckFailed)
	requireCond(t, res, "Stalled", metav1.ConditionTrue, dbv1alpha1.ReasonCheckFailed)
	require.Equal(t,
		"local migration directory requires a dev database to replay: set spec.devURL or spec.devURLFrom "+
			"on AtlasMigration/migration, or push the directory to the Atlas Registry",
		driftCond(t, res, "Ready").Message)
}

func TestDriftCheck_MissingSecret(t *testing.T) {
	check := driftCheckObj()
	// The token secret is missing: a transient failure, it may still show up.
	h, reconcile := newDriftRunner(check, &mockAtlasExec{}, driftTarget())
	reconcile(check, func(result ctrl.Result, err error) {
		require.NoError(t, err)
		require.Equal(t, ctrl.Result{RequeueAfter: transientDriftRetry}, result)
	})
	res := &dbv1alpha1.AtlasDriftCheck{ObjectMeta: check.ObjectMeta}
	h.get(t, res)
	requireCond(t, res, "Ready", metav1.ConditionFalse, dbv1alpha1.ReasonCheckFailed)
	requireCond(t, res, "Reconciling", metav1.ConditionTrue, dbv1alpha1.ReasonCheckFailed)
	requireCond(t, res, "Stalled", metav1.ConditionFalse, dbv1alpha1.ReasonCheckFailed)
	require.Contains(t, driftCond(t, res, "Ready").Message, `secrets "my-secret" not found`)
}

func TestDriftCheck_ExcludeDefaultsFromTargetPolicy(t *testing.T) {
	for _, tt := range []struct {
		name    string
		exclude []string
		expect  []string
	}{
		{"from the target policy", nil, []string{"public.audit_*"}},
		{"overridden by the check", []string{"*[type=extension]"}, []string{"*[type=extension]"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var (
				check  = driftCheckObj()
				target = driftTarget()
				mock   = &mockAtlasExec{}
			)
			check.Spec.Exclude = tt.exclude
			target.Spec.Policy = &dbv1alpha1.MigrationPolicy{
				Drift: &dbv1alpha1.DriftPolicy{Exclude: []string{"public.audit_*"}},
			}
			mock.drift.res = []*atlasexec.MigrateDrift{{Mode: "registry", Version: "2"}}
			_, reconcile := newDriftRunner(check, mock, target, driftTokenSecret())
			reconcile(check, func(_ ctrl.Result, err error) { require.NoError(t, err) })
			require.NotNil(t, mock.drift.params)
			require.Equal(t, tt.expect, mock.drift.params.Exclude)
			require.Equal(t, defaultEnvName, mock.drift.params.Env)
			// The check never skips the lock: it must not race a deployment.
			require.False(t, mock.drift.params.SkipLock)
		})
	}
}

func TestClassifyDriftError(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	for _, tt := range []struct {
		name      string
		ctx       context.Context
		err       error
		locked    bool
		reason    string
		permanent bool
		message   string
	}{
		{
			name:   "contended",
			err:    &atlasexec.MigrateDriftError{Result: []*atlasexec.MigrateDrift{{Error: "acquiring database lock: timeout exceeded"}}},
			locked: true,
		},
		{
			name:      "no migration history",
			err:       &atlasexec.Error{Stderr: "Error: no migration history found on the connected database"},
			reason:    dbv1alpha1.ReasonNoMigrationHistory,
			permanent: true,
			message:   "Error: no migration history found on the connected database",
		},
		{
			name:      "atlas pro gate",
			err:       &atlasexec.Error{Stderr: "Abort: command 'atlas migrate drift' is available only to Atlas Pro users"},
			reason:    dbv1alpha1.ReasonCheckFailed,
			permanent: true,
			message:   "Abort: command 'atlas migrate drift' is available only to Atlas Pro users",
		},
		{
			name:      "login required",
			err:       fmt.Errorf("running drift: %w", atlasexec.ErrRequireLogin),
			reason:    dbv1alpha1.ReasonCheckFailed,
			permanent: true,
			message:   "running drift: command requires 'atlas login'",
		},
		{
			name:      "unknown command",
			err:       errors.New(`unknown command "drift" for "atlas migrate"`),
			reason:    dbv1alpha1.ReasonCheckFailed,
			permanent: true,
			message:   `unknown command "drift" for "atlas migrate"`,
		},
		{
			name:      "partially applied",
			err:       errors.New(`version "2" was partially applied`),
			reason:    dbv1alpha1.ReasonCheckFailed,
			permanent: true,
			message:   `version "2" was partially applied`,
		},
		{
			name:      "checksum mismatch",
			err:       &atlasexec.Error{Stderr: "You have a checksum error in your migration directory.\nchecksum mismatch"},
			reason:    dbv1alpha1.ReasonCheckFailed,
			permanent: true,
			message:   "You have a checksum error in your migration directory.\nchecksum mismatch",
		},
		{
			name:      "no registry repository",
			err:       errors.New("drift check requires migration.repo.name or an atlas:// directory URL to be set"),
			reason:    dbv1alpha1.ReasonCheckFailed,
			permanent: true,
			message:   "drift check requires migration.repo.name or an atlas:// directory URL to be set",
		},
		{
			name:      "bad expected state",
			err:       errors.New(`parsing expected state HCL: missing ")"`),
			reason:    dbv1alpha1.ReasonCheckFailed,
			permanent: true,
			message:   `parsing expected state HCL: missing ")"`,
		},
		{
			name:    "timed out",
			ctx:     expired,
			err:     errors.New("signal: killed"),
			reason:  dbv1alpha1.ReasonCheckFailed,
			message: "the drift check timed out: signal: killed",
		},
		{
			name:    "unclassified",
			err:     errors.New("dial tcp: connection refused"),
			reason:  dbv1alpha1.ReasonCheckFailed,
			message: "dial tcp: connection refused",
		},
		{
			name:    "reported on stderr only",
			err:     &atlasexec.MigrateDriftError{Stderr: "Error: something went wrong"},
			reason:  dbv1alpha1.ReasonCheckFailed,
			message: "Error: something went wrong",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			got := classifyDriftError(ctx, tt.err)
			if tt.locked {
				require.ErrorIs(t, got, errLocked)
				return
			}
			e, ok := errors.AsType[*checkError](got)
			require.True(t, ok, "expected a checkError, got %T", got)
			require.Equal(t, tt.reason, e.reason)
			require.Equal(t, tt.permanent, e.permanent)
			require.Equal(t, tt.message, e.message)
		})
	}
}
