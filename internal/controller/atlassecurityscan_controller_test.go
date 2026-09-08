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
	"testing"
	"time"

	"ariga.io/atlas/atlasexec"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
)

// newSecurityScanReconciler adapts the constructor to the runner, which
// passes the prewarmDevDB flag that a scan has no use for.
func newSecurityScanReconciler(mgr Manager, _ bool) *AtlasSecurityScanReconciler {
	return NewAtlasSecurityScanReconciler(mgr)
}

func securityScanObjmeta() metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: "app", Namespace: "test"}
}

// securityScanSecrets are the secrets the scan resources of the tests refer to.
func securityScanSecrets() []*corev1.Secret {
	return []*corev1.Secret{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "db-creds", Namespace: "test"},
			Data:       map[string][]byte{"url": []byte("postgres://root:pass@db:5432/app?sslmode=disable")},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "atlas-token", Namespace: "test"},
			Data:       map[string][]byte{"token": []byte("aci_token")},
		},
	}
}

func TestSecurityScan_Reconcile(t *testing.T) {
	var (
		meta = securityScanObjmeta()
		obj  = &dbv1alpha1.AtlasSecurityScan{
			ObjectMeta: meta,
			Spec: dbv1alpha1.AtlasSecurityScanSpec{
				TargetSpec: dbv1alpha1.TargetSpec{
					URLFrom: dbv1alpha1.Secret{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"}, Key: "url",
					}},
				},
				Cloud: dbv1alpha1.SecurityScanCloud{TokenFrom: dbv1alpha1.TokenFrom{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "atlas-token"}, Key: "token",
					},
				}},
				Schedule:    "0 6 * * *",
				MinSeverity: dbv1alpha1.SecurityLevelElevated,
				FailOn:      dbv1alpha1.SecurityLevelHigh,
				Ignore:      []string{"CVE-2014-2669"},
			},
		}
		mock = &mockAtlasExec{}
		// A scan that just ended leaves the next daily window in the future, so
		// the resource is reliably not due whatever the wall clock says.
		clean = func(end time.Time) *atlasexec.SecurityScan {
			return &atlasexec.SecurityScan{
				Targets: []*atlasexec.SecurityScanTarget{{
					URL:        "postgres://root:xxxxx@db:5432/app?sslmode=disable",
					Driver:     "postgres",
					Version:    "16.2",
					Extensions: []string{"hstore", "pgcrypto"},
				}},
				End: end,
			}
		}
	)
	h, reconcile := newRunner(newSecurityScanReconciler, func(cb *fake.ClientBuilder) {
		cb.WithStatusSubresource(obj)
		cb.WithObjects(obj)
		for _, s := range securityScanSecrets() {
			cb.WithObjects(s)
		}
	}, mock)
	get := func() *dbv1alpha1.AtlasSecurityScan {
		res := &dbv1alpha1.AtlasSecurityScan{ObjectMeta: meta}
		h.get(t, res)
		return res
	}
	assert := func(ready bool, reason, message string) *dbv1alpha1.AtlasSecurityScan {
		t.Helper()
		var res *dbv1alpha1.AtlasSecurityScan
		reconcile(obj, func(_ ctrl.Result, err error) {
			require.NoError(t, err)
			res = get()
			require.Equal(t, ready, res.IsReady())
			cond := apimeta.FindStatusCondition(res.Status.Conditions, "Ready")
			require.NotNil(t, cond, "Ready condition not found")
			require.Equal(t, reason, cond.Reason)
			require.Equal(t, message, cond.Message)
		})
		return res
	}
	// The first reconcile only creates the conditions.
	reconcile(obj, func(result ctrl.Result, err error) {
		require.NoError(t, err)
		require.Equal(t, ctrl.Result{RequeueAfter: time.Second}, result)
	})
	require.Empty(t, mock.securityScans)

	// A clean scan. The resource is ready, and its next window is published.
	mock.securityScan.res = clean(time.Now())
	res := assert(true, "Scanned", "no issues found in 2 extensions")
	require.Equal(t, []*atlasexec.SecurityScanParams{{
		Env:         "kubernetes",
		Vars:        atlasexec.Vars2{},
		MinSeverity: "ELEVATED",
		FailOn:      "HIGH",
		Ignore:      []string{"CVE-2014-2669"},
	}}, mock.securityScans)
	require.Equal(t, dbv1alpha1.TriggerSpecChange, res.Status.LastScanTrigger)
	require.NotNil(t, res.Status.NextScanTime)
	require.True(t, res.Status.NextScanTime.After(time.Now()), "the daily window is ahead")
	require.True(t, res.IsSecure(), "nothing reached failOn")
	require.Equal(t, 0, res.Status.Failed)
	hash := res.Status.ObservedHash

	// Nothing changed and the window has not come round: no scan, just a wake-up.
	reconcile(obj, func(result ctrl.Result, err error) {
		require.NoError(t, err)
		require.Greater(t, result.RequeueAfter, time.Duration(0))
		require.LessOrEqual(t, result.RequeueAfter, 24*time.Hour)
	})
	require.Len(t, mock.securityScans, 1)

	// A change to the input is scanned right away, and this time an issue reaches
	// failOn: the CLI reports it and fails on it.
	h.patch(t, &dbv1alpha1.AtlasSecurityScan{
		ObjectMeta: meta,
		Spec:       dbv1alpha1.AtlasSecurityScanSpec{MinSeverity: dbv1alpha1.SecurityLevelNormal},
	})
	mock.securityScan.res = &atlasexec.SecurityScan{
		Targets: []*atlasexec.SecurityScanTarget{{
			URL:        "postgres://root:xxxxx@db:5432/app?sslmode=disable",
			Driver:     "postgres",
			Version:    "9.3.0",
			Extensions: []string{"hstore", "postgis"},
			Vulnerabilities: []*atlasexec.SecurityVulnerability{
				{
					Name: "hstore", Version: "1.3", ID: "CVE-2014-2669", Level: "ELEVATED", Severity: "MEDIUM",
					Suggestion: `Upgrade the database engine to version 9.3.3 or later to resolve CVE-2014-2669`,
				},
				{
					Name: "postgis", Version: "2.3.1", ID: "CVE-2017-18359", Level: "HIGH", Severity: "HIGH",
					Suggestion: `Upgrade extension "postgis" to version 2.3.3 or later to resolve CVE-2017-18359`,
				},
			},
		}},
		// Dated back so the daily window is overdue and the next reconcile is due.
		End: time.Now().Add(-48 * time.Hour),
	}
	mock.securityScan.err = atlasexec.ErrSecurityScan
	// The scan ran, so the resource stays ready. Only the verdict moves.
	res = assert(true, "Scanned", "2 issues found: 1 high, 1 elevated")
	require.False(t, res.IsSecure())
	secure := apimeta.FindStatusCondition(res.Status.Conditions, "Secure")
	require.NotNil(t, secure)
	require.Equal(t, "SecurityIssues", secure.Reason)
	require.Len(t, mock.securityScans, 2)
	require.Equal(t, "NORMAL", mock.securityScans[1].MinSeverity)
	require.NotEqual(t, hash, res.Status.ObservedHash)
	require.Equal(t, 2, res.Status.Issues)
	require.Equal(t, []dbv1alpha1.SecurityScanLevel{{Level: "HIGH", Count: 1}, {Level: "ELEVATED", Count: 1}}, res.Status.Levels)
	// A verdict is not a failure of the operator.
	require.Equal(t, 0, res.Status.Failed)
	require.True(t, apimeta.IsStatusConditionFalse(res.Status.Conditions, "Stalled"))

	// A database that could not be scanned is the one case that clears Ready.
	mock.securityScan.res = &atlasexec.SecurityScan{
		Targets: []*atlasexec.SecurityScanTarget{{
			URL:   "postgres://root:xxxxx@db:5432/app?sslmode=disable",
			Error: "connection refused",
		}},
		End: time.Now().Add(-48 * time.Hour),
	}
	res = assert(false, "Scanning", "postgres://root:xxxxx@db:5432/app?sslmode=disable: connection refused")
	require.Len(t, mock.securityScans, 3)
	require.Equal(t, 1, res.Status.Failed)
	require.False(t, res.IsSecure(), "the earlier verdict is left alone")
	require.True(t, apimeta.IsStatusConditionTrue(res.Status.Conditions, "Stalled"))

	// The command failed before reporting anything. The last report stays.
	mock.securityScan.res = nil
	mock.securityScan.err = errors.New("Abort: atlas security scan is not enabled for your plan.")
	res = assert(false, "Scanning", "Abort: atlas security scan is not enabled for your plan.")
	require.Equal(t, 2, res.Status.Failed)
	require.Equal(t, "connection refused", res.Status.Targets[0].Error)

	// A clean scan resets the failures and the verdict.
	mock.securityScan.res, mock.securityScan.err = clean(time.Now()), nil
	res = assert(true, "Scanned", "no issues found in 2 extensions")
	require.Equal(t, 0, res.Status.Failed)
	require.True(t, res.IsSecure())

	require.Equal(t, []string{
		"Normal Scanned no issues found in 2 extensions",
		"Warning SecurityIssues 2 issues found: 1 high, 1 elevated",
		"Warning TransientErr postgres://root:xxxxx@db:5432/app?sslmode=disable: connection refused",
		"Warning Error Abort: atlas security scan is not enabled for your plan.",
		"Normal Scanned no issues found in 2 extensions",
	}, h.events())
}

// Reported issues do not fail the resource unless it sets failOn.
func TestSecurityScan_ReconcileIssues(t *testing.T) {
	var (
		meta = securityScanObjmeta()
		obj  = &dbv1alpha1.AtlasSecurityScan{
			ObjectMeta: meta,
			Spec: dbv1alpha1.AtlasSecurityScanSpec{
				TargetSpec: dbv1alpha1.TargetSpec{URL: "postgres://root:pass@db:5432/app?sslmode=disable"},
			},
			Status: dbv1alpha1.AtlasSecurityScanStatus{
				Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse}},
			},
		}
		mock = &mockAtlasExec{}
	)
	mock.securityScan.res = &atlasexec.SecurityScan{
		Targets: []*atlasexec.SecurityScanTarget{{
			URL:        "postgres://root:xxxxx@db:5432/app?sslmode=disable",
			Driver:     "postgres",
			Version:    "13.23",
			Extensions: []string{"pgcrypto"},
			Vulnerabilities: []*atlasexec.SecurityVulnerability{
				{Name: "pgcrypto", Version: "1.3", ID: "CVE-2026-2005", Level: "HIGH", Severity: "HIGH"},
			},
		}},
		End: time.Now(),
	}
	h, reconcile := newRunner(newSecurityScanReconciler, func(cb *fake.ClientBuilder) {
		cb.WithStatusSubresource(obj)
		cb.WithObjects(obj)
	}, mock)
	res := &dbv1alpha1.AtlasSecurityScan{ObjectMeta: meta}
	reconcile(obj, func(result ctrl.Result, err error) {
		require.NoError(t, err)
		require.Equal(t, ctrl.Result{}, result)
	})
	h.get(t, res)
	require.True(t, res.IsReady())
	cond := apimeta.FindStatusCondition(res.Status.Conditions, "Ready")
	require.Equal(t, "Scanned", cond.Reason)
	require.Equal(t, "1 issue found: 1 high", cond.Message)
	require.Equal(t, 1, res.Status.Issues)
	require.Equal(t, []dbv1alpha1.SecurityScanLevel{{Level: "HIGH", Count: 1}}, res.Status.Levels)
	require.Equal(t, []*atlasexec.SecurityScanParams{{Env: "kubernetes", Vars: atlasexec.Vars2{}}}, mock.securityScans)
	// Without an interval, a ready resource is not scanned again.
	reconcile(obj, func(result ctrl.Result, err error) {
		require.NoError(t, err)
		require.Equal(t, ctrl.Result{}, result)
	})
	require.Len(t, mock.securityScans, 1)
	require.Equal(t, []string{
		"Warning SecurityIssues 1 issue found: 1 high",
	}, h.events())
}

// A failed reconciliation is retried with a backoff until the limit is
// reached, after which the resource waits for its next scan.
func TestSecurityScan_BackoffLimit(t *testing.T) {
	var (
		meta = securityScanObjmeta()
		obj  = &dbv1alpha1.AtlasSecurityScan{
			ObjectMeta: meta,
			Spec:       dbv1alpha1.AtlasSecurityScanSpec{BackoffLimit: 1},
			Status: dbv1alpha1.AtlasSecurityScanStatus{
				Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse}},
			},
		}
	)
	h, reconcile := newRunner(newSecurityScanReconciler, func(cb *fake.ClientBuilder) {
		cb.WithStatusSubresource(obj)
		cb.WithObjects(obj)
	}, &mockAtlasExec{})
	assert := func(expected ctrl.Result, failed int) {
		t.Helper()
		reconcile(obj, func(result ctrl.Result, err error) {
			require.NoError(t, err)
			require.Equal(t, expected, result)
		})
		res := &dbv1alpha1.AtlasSecurityScan{ObjectMeta: meta}
		h.get(t, res)
		require.False(t, res.IsReady())
		cond := apimeta.FindStatusCondition(res.Status.Conditions, "Ready")
		require.Equal(t, "ReadingScanData", cond.Reason)
		require.Equal(t, "no target database defined", cond.Message)
		require.Equal(t, failed, res.Status.Failed)
	}
	assert(ctrl.Result{RequeueAfter: retryDuration}, 1)
	// The limit is exceeded: the next attempt is the next scan, a day away by default.
	assert(ctrl.Result{}, 2)
	require.Equal(t, []string{
		"Warning TransientErr no target database defined",
		"Warning TransientErr no target database defined",
		"Warning BackoffLimitExceeded backoff limit exceeded",
	}, h.events())
}

func TestSecurityScan_ExtractData(t *testing.T) {
	var (
		ctx = context.Background()
		r   = &AtlasSecurityScanReconciler{Client: fake.NewClientBuilder().Build()}
		res = &dbv1alpha1.AtlasSecurityScan{
			ObjectMeta: securityScanObjmeta(),
			Spec: dbv1alpha1.AtlasSecurityScanSpec{
				TargetSpec: dbv1alpha1.TargetSpec{URL: "postgres://root:pass@db:5432/app?sslmode=disable"},
				ProjectConfigSpec: dbv1alpha1.ProjectConfigSpec{
					Config: `env "prod" {
  security {
    fail_on = HIGH
  }
}`,
				},
			},
		}
	)
	_, err := r.extractData(ctx, res)
	require.EqualError(t, err, `install the operator with "--set allowCustomConfig=true" to use custom atlas.hcl config`)
	r.allowCustomConfig = true
	_, err = r.extractData(ctx, res)
	require.EqualError(t, err, "env name must be set when using custom atlas.hcl config")
	res.Spec.EnvName = "prod"
	data, err := r.extractData(ctx, res)
	require.NoError(t, err)
	require.Equal(t, "prod", data.EnvName)
	require.Equal(t, "postgres://root:pass@db:5432/app?sslmode=disable", data.URL.String())
	require.Nil(t, data.Cloud)
	// The hash follows the input of the scan, not the schedule.
	hash := data.hash()
	res.Spec.Schedule = "0 6 * * *"
	data, err = r.extractData(ctx, res)
	require.NoError(t, err)
	require.NotNil(t, data.Schedule)
	require.Equal(t, hash, data.hash())
	res.Spec.FailOn = dbv1alpha1.SecurityLevelCritical
	data, err = r.extractData(ctx, res)
	require.NoError(t, err)
	require.NotEqual(t, hash, data.hash())

	// A custom config may define the database, and nothing else does.
	res.Spec.URL = ""
	data, err = r.extractData(ctx, res)
	require.NoError(t, err)
	require.Nil(t, data.URL)
	res.Spec.Config = ""
	_, err = r.extractData(ctx, res)
	require.EqualError(t, err, "no target database defined")
	require.True(t, isTransient(err))
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
variable "slack_webhook" {
  type = string
}
env "kubernetes" {
  security {
    fail_on = HIGH
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
    fail_on = HIGH
    notify {
      http "slack" {
        url  = var.slack_webhook
        body = jsonencode({ text = "${scan.count} vulnerable extensions found" })
      }
    }
  }
}
variable "slack_webhook" {
  type = string
}
`, buf.String())

	// The database may come from the config alone.
	buf.Reset()
	data = &scanData{
		EnvName: "prod",
		Config: must(parseConfig(`
env "prod" {
  url = "postgres://root:pass@db:5432/app?sslmode=disable"
}`)),
	}
	require.NoError(t, data.render(&buf))
	require.Equal(t, `env "prod" {
  url = "postgres://root:pass@db:5432/app?sslmode=disable"
}
`, buf.String())

	// The env of the scan must define the database.
	data = &scanData{
		EnvName: "prod",
		Config:  must(parseConfig(`env "prod" {}`)),
	}
	require.EqualError(t, data.render(&buf), "database url is not set")
	data = &scanData{
		EnvName: "prod",
		Config:  must(parseConfig(`env "dev" {}`)),
		URL:     must(url.Parse("postgres://root:pass@db:5432/app?sslmode=disable")),
	}
	require.NoError(t, data.render(&buf))
}

func TestSecurityScan_Summary(t *testing.T) {
	scan := &atlasexec.SecurityScan{
		Targets: []*atlasexec.SecurityScanTarget{
			{URL: "postgres://app", Extensions: []string{"hstore"}},
			{URL: "postgres://gone", Error: "connection refused"},
		},
	}
	require.Equal(t, "no issues found in 1 extension", scanSummary(scan))
	scan.Targets[0].Vulnerabilities = []*atlasexec.SecurityVulnerability{
		{ID: "CVE-2014-2669", Level: "elevated"},
	}
	require.Equal(t, "1 issue found: 1 elevated", scanSummary(scan))
	scan.Targets[0].Vulnerabilities = append(scan.Targets[0].Vulnerabilities,
		&atlasexec.SecurityVulnerability{ID: "CVE-2017-18359", Level: "HIGH"},
		&atlasexec.SecurityVulnerability{ID: "CVE-2026-2005", Level: "HIGH"},
		&atlasexec.SecurityVulnerability{ID: "CVE-2026-0001", Level: "CRITICAL"},
	)
	require.Equal(t, "4 issues found: 1 critical, 2 high, 1 elevated", scanSummary(scan))
}

func parseConfig(s string) (*hclwrite.File, error) {
	f, diags := hclwrite.ParseConfig([]byte(s), "", hcl.InitialPos)
	if diags.HasErrors() {
		return nil, diags
	}
	return f, nil
}

// A scan with no interval is not requeued, so a status the API never received
// would stay stale. The reconcile fails instead, and the controller retries it.
func TestSecurityScan_StatusUpdateFails(t *testing.T) {
	var (
		meta = securityScanObjmeta()
		obj  = &dbv1alpha1.AtlasSecurityScan{
			ObjectMeta: meta,
			Spec: dbv1alpha1.AtlasSecurityScanSpec{
				TargetSpec: dbv1alpha1.TargetSpec{URL: "postgres://root:pass@db:5432/app?sslmode=disable"},
			},
			Status: dbv1alpha1.AtlasSecurityScanStatus{
				Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse}},
			},
		}
		mock = &mockAtlasExec{}
	)
	mock.securityScan.res = &atlasexec.SecurityScan{
		Targets: []*atlasexec.SecurityScanTarget{{URL: "postgres://root:xxxxx@db:5432/app", Extensions: []string{"hstore"}}},
		End:     time.Now(),
	}
	_, reconcile := newRunner(newSecurityScanReconciler, func(cb *fake.ClientBuilder) {
		cb.WithStatusSubresource(obj)
		cb.WithObjects(obj)
		cb.WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return errors.New("etcdserver: request timed out")
			},
		})
	}, mock)
	reconcile(obj, func(_ ctrl.Result, err error) {
		require.EqualError(t, err, "updating resource status: etcdserver: request timed out")
	})
	// The scan itself ran; only writing its result back failed.
	require.Len(t, mock.securityScans, 1)
}

// An apply by a watched resource scans the database, without any schedule.
func TestSecurityScan_TriggerOn(t *testing.T) {
	var (
		meta   = securityScanObjmeta()
		schema = &dbv1alpha1.AtlasSchema{
			ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: "test"},
			Status:     dbv1alpha1.AtlasSchemaStatus{LastApplied: time.Now().Add(-72 * time.Hour).Unix()},
		}
		obj = &dbv1alpha1.AtlasSecurityScan{
			ObjectMeta: meta,
			Spec: dbv1alpha1.AtlasSecurityScanSpec{
				TargetSpec: dbv1alpha1.TargetSpec{URL: "postgres://root:pass@db:5432/app?sslmode=disable"},
				TriggerOn:  []dbv1alpha1.TriggerRef{{Kind: "AtlasSchema", Name: "myapp"}},
			},
			Status: dbv1alpha1.AtlasSecurityScanStatus{
				Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse}},
			},
		}
		mock = &mockAtlasExec{}
	)
	mock.securityScan.res = &atlasexec.SecurityScan{
		Targets: []*atlasexec.SecurityScanTarget{{URL: "postgres://root:xxxxx@db:5432/app", Extensions: []string{"hstore"}}},
		End:     time.Now().Add(-time.Hour),
	}
	h, reconcile := newRunner(newSecurityScanReconciler, func(cb *fake.ClientBuilder) {
		cb.WithStatusSubresource(obj)
		cb.WithObjects(obj, schema)
	}, mock)
	run := func() *dbv1alpha1.AtlasSecurityScan {
		t.Helper()
		reconcile(obj, func(_ ctrl.Result, err error) { require.NoError(t, err) })
		res := &dbv1alpha1.AtlasSecurityScan{ObjectMeta: meta}
		h.get(t, res)
		return res
	}
	// The first scan is the resource's own, and no schedule means no next window.
	res := run()
	require.Equal(t, dbv1alpha1.TriggerSpecChange, res.Status.LastScanTrigger)
	require.Nil(t, res.Status.NextScanTime)
	require.Len(t, mock.securityScans, 1)

	// Nothing applied since, so nothing to do.
	require.Equal(t, dbv1alpha1.TriggerSpecChange, run().Status.LastScanTrigger)
	require.Len(t, mock.securityScans, 1)

	// The watched resource applies after that scan: that alone is the trigger,
	// and the scan that follows covers it.
	schema.Status.LastApplied = time.Now().Add(-30 * time.Minute).Unix()
	require.NoError(t, h.client.Update(context.Background(), schema))
	mock.securityScan.res.End = time.Now()
	res = run()
	require.Equal(t, dbv1alpha1.TriggerChange, res.Status.LastScanTrigger)
	require.Len(t, mock.securityScans, 2)

	// An edit that never applies moves nothing.
	require.Len(t, run().Status.Targets, 1)
	require.Len(t, mock.securityScans, 2)
}

// A schedule that does not parse cannot be fixed by retrying it.
func TestSecurityScan_InvalidSchedule(t *testing.T) {
	for _, tt := range []struct {
		name, schedule, timeZone, message string
	}{
		{
			name: "schedule", schedule: "every tuesday",
			message: `invalid schedule "every tuesday": expected exactly 5 fields, found 2: [every tuesday]`,
		},
		{
			name: "time zone", schedule: "0 6 * * *", timeZone: "Mars/Olympus",
			message: `invalid timeZone "Mars/Olympus": unknown time zone Mars/Olympus`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var (
				meta = securityScanObjmeta()
				obj  = &dbv1alpha1.AtlasSecurityScan{
					ObjectMeta: meta,
					Spec: dbv1alpha1.AtlasSecurityScanSpec{
						TargetSpec: dbv1alpha1.TargetSpec{URL: "postgres://root:pass@db:5432/app?sslmode=disable"},
						Schedule:   tt.schedule,
						TimeZone:   tt.timeZone,
					},
					Status: dbv1alpha1.AtlasSecurityScanStatus{
						Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse}},
					},
				}
				mock = &mockAtlasExec{}
			)
			h, reconcile := newRunner(newSecurityScanReconciler, func(cb *fake.ClientBuilder) {
				cb.WithStatusSubresource(obj)
				cb.WithObjects(obj)
			}, mock)
			reconcile(obj, func(result ctrl.Result, err error) {
				require.NoError(t, err)
				// Not requeued: the next edit is the only thing that can help.
				require.Equal(t, ctrl.Result{}, result)
			})
			res := &dbv1alpha1.AtlasSecurityScan{ObjectMeta: meta}
			h.get(t, res)
			require.False(t, res.IsReady())
			cond := apimeta.FindStatusCondition(res.Status.Conditions, "Ready")
			require.Equal(t, "InvalidSchedule", cond.Reason)
			require.Equal(t, tt.message, cond.Message)
			require.Empty(t, mock.securityScans)
		})
	}
}
