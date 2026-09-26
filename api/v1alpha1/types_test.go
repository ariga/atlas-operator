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

package v1alpha1_test

import (
	"context"
	"net/url"
	"testing"

	"ariga.io/atlas/atlasexec"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	testclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ariga/atlas-operator/api/v1alpha1"
)

// This test ensure the priority of methods
// to get the database URL:
// URLFrom > URL > Credentials.PasswordFrom > Credentials > error
func TestTargetSpec_DatabaseURL(t *testing.T) {
	var (
		ctx    = context.Background()
		target = v1alpha1.TargetSpec{}
		client = testclient.NewClientBuilder().
			WithObjects(&v1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
				Data: map[string][]byte{
					"user":     []byte("nobody"),
					"host":     []byte("default"),
					"url":      []byte("mysql://root:root@localhost:3306/secret"),
					"password": []byte("123456"),
				},
			}).
			Build()
		equal = func(a string) {
			u, err := target.DatabaseURL(ctx, client, "default")
			require.NoError(t, err)
			require.Equal(t, a, u.String())
		}
	)

	// Should return the URL from the credentials
	target.Credentials = v1alpha1.Credentials{
		Scheme:   "mysql",
		Host:     "localhost",
		Port:     3306,
		Database: "local",
		User:     "nobody",
		Password: "secret",
	}
	equal("mysql://nobody:secret@localhost:3306/local")

	// Should return the User from the secret
	target.Credentials.UserFrom.SecretKeyRef = &v1.SecretKeySelector{
		LocalObjectReference: v1.LocalObjectReference{
			Name: "test",
		},
		Key: "user",
	}
	equal("mysql://nobody:secret@localhost:3306/local")

	// Should return the Host from the secret
	target.Credentials.HostFrom.SecretKeyRef = &v1.SecretKeySelector{
		LocalObjectReference: v1.LocalObjectReference{
			Name: "test",
		},
		Key: "host",
	}
	equal("mysql://nobody:secret@default:3306/local")

	// Should return the URL from the credentials and the password from the secret
	target.Credentials.PasswordFrom.SecretKeyRef = &v1.SecretKeySelector{
		LocalObjectReference: v1.LocalObjectReference{
			Name: "test",
		},
		Key: "password",
	}
	equal("mysql://nobody:123456@default:3306/local")

	// Should return the same URL if explicitly defined
	target.URL = "mysql://root:root@localhost:3306/test"
	equal(target.URL)

	// Should return the URL from the secret
	target.URLFrom.SecretKeyRef = &v1.SecretKeySelector{
		LocalObjectReference: v1.LocalObjectReference{
			Name: "test",
		},
		Key: "url",
	}
	equal("mysql://root:root@localhost:3306/secret")
}

func TestSchema_Content(t *testing.T) {
	var (
		ctx    = context.Background()
		sch    = v1alpha1.Schema{}
		client = testclient.NewClientBuilder().
			WithObjects(&v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
				Data: map[string]string{
					"schema.bug": `boo`,
					"schema.hcl": `foo`,
					"schema.sql": `bar`,
				},
			}).
			Build()
	)
	sch.SQL = "bar"
	u, data, err := sch.DesiredState(ctx, client, "default")
	require.NoError(t, err)
	require.Equal(t, "file://schema.sql", u.String())
	require.Equal(t, []byte("bar"), data)

	sch.HCL = "foo"
	u, data, err = sch.DesiredState(ctx, client, "default")
	require.NoError(t, err)
	require.Equal(t, "file://schema.hcl", u.String())
	require.Equal(t, []byte("foo"), data)

	// Should return the content from the configmap
	sch.ConfigMapKeyRef = &v1.ConfigMapKeySelector{
		LocalObjectReference: v1.LocalObjectReference{
			Name: "test",
		},
		Key: "schema.sql",
	}
	u, data, err = sch.DesiredState(ctx, client, "default")
	require.NoError(t, err)
	require.Equal(t, "file://schema.sql", u.String())
	require.Equal(t, []byte("bar"), data)

	sch.ConfigMapKeyRef.Key = "schema.bug"
	_, _, err = sch.DesiredState(ctx, client, "default")
	require.ErrorContains(t, err, `configmaps key "schema.bug" must be ending with .sql or .hcl, received ".bug"`)

	sch.ConfigMapKeyRef.Key = "schema.foo"
	_, _, err = sch.DesiredState(ctx, client, "default")
	require.ErrorContains(t, err, `configmaps default/test does not contain key "schema.foo"`)

	sch.ConfigMapKeyRef.Name = "foo"
	_, _, err = sch.DesiredState(ctx, client, "default")
	require.ErrorContains(t, err, `configmaps "foo" not found`)
}

func TestCredentials_URL(t *testing.T) {
	for _, tt := range []struct {
		c   v1alpha1.Credentials
		exp string
	}{
		{
			c: v1alpha1.Credentials{
				Scheme:   "postgres",
				User:     "user",
				Password: "pass",
				Host:     "host",
				Port:     5432,
				Database: "db",
				Parameters: map[string]string{
					"sslmode": "disable",
				},
			},
			exp: "postgres://user:pass@host:5432/db?sslmode=disable",
		},
		{
			c: v1alpha1.Credentials{
				Scheme: "sqlite",
				Host:   "file",
				Parameters: map[string]string{
					"mode": "memory",
				},
			},
			exp: "sqlite://file?mode=memory",
		},
		{
			c: v1alpha1.Credentials{
				Scheme:   "mysql",
				User:     "user",
				Password: "pass",
				Host:     "host",
				Database: "db",
			},
			exp: "mysql://user:pass@host/db",
		},
		{
			c: v1alpha1.Credentials{
				Scheme:   "mysql",
				User:     "user",
				Password: "pass",
				Host:     "",
				Port:     3306,
				Database: "db",
			},
			exp: "mysql://user:pass@:3306/db",
		},
		{
			c: v1alpha1.Credentials{
				Scheme:   "sqlserver",
				User:     "sa",
				Password: "P@ssw0rd0995",
				Host:     "",
				Port:     1433,
				Database: "master",
			},
			exp: "sqlserver://sa:P%40ssw0rd0995@:1433?database=master",
		},
		{
			c: v1alpha1.Credentials{
				Scheme:   "crdb",
				User:     "root",
				Password: "password",
				Host:     "localhost",
				Port:     26257,
				Database: "defaultdb",
				Parameters: map[string]string{
					"sslmode": "disable",
				},
			},
			exp: "crdb://root:password@localhost:26257/defaultdb?sslmode=disable",
		},
		{
			c: v1alpha1.Credentials{
				Scheme:   "ysql",
				User:     "yugabyte",
				Host:     "localhost",
				Port:     5433,
				Database: "yugabyte",
				Parameters: map[string]string{
					"search_path": "public",
					"sslmode":     "disable",
				},
			},
			exp: "ysql://yugabyte@localhost:5433/yugabyte?search_path=public&sslmode=disable",
		},
	} {
		t.Run(tt.exp, func(t *testing.T) {
			u, err := tt.c.URL()
			require.NoError(t, err)
			require.Equal(t, tt.exp, u.String())
		})
	}
}

func TestDriverBySchema_YSQL(t *testing.T) {
	drv, err := v1alpha1.DriverBySchema("ysql")
	require.NoError(t, err)
	require.Equal(t, v1alpha1.DriverYSQL, drv)

	u, err := url.Parse("ysql://yugabyte@localhost:5433/yugabyte?search_path=public&sslmode=disable")
	require.NoError(t, err)

	schemaBound, err := drv.SchemaBound(*u)
	require.NoError(t, err)
	require.True(t, schemaBound)
}

func requireCondition(t *testing.T, conds []metav1.Condition, condType string) metav1.Condition {
	t.Helper()
	cond := meta.FindStatusCondition(conds, condType)
	if cond == nil {
		t.Fatalf("condition %s not found", condType)
	}
	return *cond
}

func TestAtlasMigrationStatusConditions(t *testing.T) {
	res := &v1alpha1.AtlasMigration{ObjectMeta: metav1.ObjectMeta{Generation: 3}}
	res.SetReconciling("syncing")
	require.Equal(t, int64(3), res.Status.ObservedGeneration)
	recon := requireCondition(t, res.Status.Conditions, "Reconciling")
	require.Equal(t, metav1.ConditionTrue, recon.Status)
	require.Equal(t, v1alpha1.ReasonReconciling, recon.Reason)
	ready := requireCondition(t, res.Status.Conditions, "Ready")
	require.Equal(t, metav1.ConditionFalse, ready.Status)
	stalled := requireCondition(t, res.Status.Conditions, "Stalled")
	require.Equal(t, metav1.ConditionFalse, stalled.Status)
	require.Equal(t, 0, res.Status.Failed)

	res.SetNotReady(v1alpha1.ReasonApprovalPending, "waiting approval")
	require.Equal(t, int64(3), res.Status.ObservedGeneration)
	recon = requireCondition(t, res.Status.Conditions, "Reconciling")
	require.Equal(t, metav1.ConditionTrue, recon.Status)
	require.Equal(t, "waiting approval", recon.Message)
	require.Equal(t, 0, res.Status.Failed)
	stalled = requireCondition(t, res.Status.Conditions, "Stalled")
	require.Equal(t, metav1.ConditionFalse, stalled.Status)

	res.SetNotReady(v1alpha1.ReasonCreatingAtlasClient, "boom")
	require.Equal(t, 1, res.Status.Failed)
	stalled = requireCondition(t, res.Status.Conditions, "Stalled")
	require.Equal(t, metav1.ConditionTrue, stalled.Status)
	require.Equal(t, "boom", stalled.Message)
	recon = requireCondition(t, res.Status.Conditions, "Reconciling")
	require.Equal(t, metav1.ConditionFalse, recon.Status)

	// A blocked pre-apply drift check is a failure: it stalls the resource and counts against the backoff limit.
	res.SetNotReady(v1alpha1.ReasonDriftDetected, "database state does not match expected state at version 1")
	require.Equal(t, 2, res.Status.Failed)
	ready = requireCondition(t, res.Status.Conditions, "Ready")
	require.Equal(t, metav1.ConditionFalse, ready.Status)
	require.Equal(t, v1alpha1.ReasonDriftDetected, ready.Reason)
	require.Equal(t, "database state does not match expected state at version 1", ready.Message)
	stalled = requireCondition(t, res.Status.Conditions, "Stalled")
	require.Equal(t, metav1.ConditionTrue, stalled.Status)
	require.Equal(t, v1alpha1.ReasonDriftDetected, stalled.Reason)
	recon = requireCondition(t, res.Status.Conditions, "Reconciling")
	require.Equal(t, metav1.ConditionFalse, recon.Status)

	res.SetReady(v1alpha1.AtlasMigrationStatus{LastApplied: 10})
	require.Equal(t, int64(3), res.Status.ObservedGeneration)
	require.Equal(t, 0, res.Status.Failed)
	ready = requireCondition(t, res.Status.Conditions, "Ready")
	require.Equal(t, metav1.ConditionTrue, ready.Status)
	recon = requireCondition(t, res.Status.Conditions, "Reconciling")
	require.Equal(t, metav1.ConditionFalse, recon.Status)
	stalled = requireCondition(t, res.Status.Conditions, "Stalled")
	require.Equal(t, metav1.ConditionFalse, stalled.Status)
}

func TestAtlasSchemaStatusConditions(t *testing.T) {
	res := &v1alpha1.AtlasSchema{ObjectMeta: metav1.ObjectMeta{Generation: 5}}
	res.SetReconciling("syncing schema")
	require.Equal(t, int64(5), res.Status.ObservedGeneration)
	ready := requireCondition(t, res.Status.Conditions, "Ready")
	require.Equal(t, metav1.ConditionFalse, ready.Status)
	stalled := requireCondition(t, res.Status.Conditions, "Stalled")
	require.Equal(t, metav1.ConditionFalse, stalled.Status)
	recon := requireCondition(t, res.Status.Conditions, "Reconciling")
	require.Equal(t, metav1.ConditionTrue, recon.Status)
	require.Equal(t, 0, res.Status.Failed)

	res.SetNotReady(v1alpha1.ReasonApprovalPending, "waiting plan")
	recon = requireCondition(t, res.Status.Conditions, "Reconciling")
	require.Equal(t, metav1.ConditionTrue, recon.Status)
	stalled = requireCondition(t, res.Status.Conditions, "Stalled")
	require.Equal(t, metav1.ConditionFalse, stalled.Status)
	require.Equal(t, 0, res.Status.Failed)

	res.SetNotReady(v1alpha1.ReasonCreatingAtlasClient, "connect failed")
	require.Equal(t, 1, res.Status.Failed)
	stalled = requireCondition(t, res.Status.Conditions, "Stalled")
	require.Equal(t, metav1.ConditionTrue, stalled.Status)
	require.Equal(t, "connect failed", stalled.Message)
	recon = requireCondition(t, res.Status.Conditions, "Reconciling")
	require.Equal(t, metav1.ConditionFalse, recon.Status)

	res.SetReady(v1alpha1.AtlasSchemaStatus{ObservedHash: "hash"}, nil)
	require.Equal(t, 0, res.Status.Failed)
	require.Equal(t, int64(5), res.Status.ObservedGeneration)
	ready = requireCondition(t, res.Status.Conditions, "Ready")
	require.Equal(t, metav1.ConditionTrue, ready.Status)
	require.Contains(t, ready.Message, "applied successfully")
	recon = requireCondition(t, res.Status.Conditions, "Reconciling")
	require.Equal(t, metav1.ConditionFalse, recon.Status)
	stalled = requireCondition(t, res.Status.Conditions, "Stalled")
	require.Equal(t, metav1.ConditionFalse, stalled.Status)
}

func TestDriftPolicy_AsBlock(t *testing.T) {
	f := hclwrite.NewFile()
	f.Body().AppendBlock((&v1alpha1.DriftPolicy{}).AsBlock())
	require.Equal(t, `check "migrate_apply" {
  drift {
    on_error = FAIL
  }
}
`, string(hclwrite.Format(f.Bytes())))

	f = hclwrite.NewFile()
	f.Body().AppendBlock((&v1alpha1.DriftPolicy{
		OnError: v1alpha1.DriftOnErrorContinue,
		Exclude: []string{"public.audit_*", "*[type=extension]"},
	}).AsBlock())
	require.Equal(t, `check "migrate_apply" {
  drift {
    on_error = CONTINUE
    exclude  = ["public.audit_*", "*[type=extension]"]
  }
}
`, string(hclwrite.Format(f.Bytes())))

	require.False(t, (*v1alpha1.MigrationPolicy)(nil).HasDrift())
	require.False(t, (&v1alpha1.MigrationPolicy{}).HasDrift())
	require.True(t, (&v1alpha1.MigrationPolicy{Drift: &v1alpha1.DriftPolicy{}}).HasDrift())
}

func TestAtlasDriftCheckStatusConditions(t *testing.T) {
	var (
		res  = &v1alpha1.AtlasDriftCheck{ObjectMeta: metav1.ObjectMeta{Generation: 7}}
		cond = func(typ string) metav1.Condition {
			t.Helper()
			return requireCondition(t, res.Status.Conditions, typ)
		}
		requireStatus = func(typ string, status metav1.ConditionStatus, reason string) {
			t.Helper()
			c := cond(typ)
			require.Equal(t, status, c.Status, "condition %s", typ)
			require.Equal(t, reason, c.Reason, "condition %s", typ)
			require.Equal(t, int64(7), c.ObservedGeneration, "condition %s", typ)
		}
	)
	// A clean check is ready and not drifted.
	res.SetChecked(&atlasexec.MigrateDrift{Mode: "registry", Version: "2"})
	require.Equal(t, int64(7), res.Status.ObservedGeneration)
	requireStatus("Ready", metav1.ConditionTrue, v1alpha1.ReasonChecked)
	requireStatus("Reconciling", metav1.ConditionFalse, v1alpha1.ReasonChecked)
	requireStatus("Stalled", metav1.ConditionFalse, v1alpha1.ReasonChecked)
	requireStatus("Drifted", metav1.ConditionFalse, v1alpha1.ReasonNoDrift)
	require.Equal(t, "no drift detected at version 2", cond("Drifted").Message)
	require.Equal(t, "registry", res.Status.Mode)
	require.Equal(t, "2", res.Status.Version)
	require.NotNil(t, res.Status.LastCheckTime)
	require.Empty(t, res.Status.Fingerprint)
	require.Nil(t, res.Status.Summary)

	// Drift with onDrift=Report keeps the check ready.
	drifted := &atlasexec.MigrateDrift{
		Mode: "registry", Version: "2", Drifted: true, Fingerprint: "fp1",
		Summary: &atlasexec.MigrateDriftSummary{
			Total: 3, Extra: 1, Missing: 1, Modified: 1,
			Types: map[string]int{"table": 2, "index": 1},
		},
	}
	res.SetDrifted(drifted, v1alpha1.DriftActionReport)
	const msg = "3 drifted objects (extra 1, missing 1, modified 1) at version 2: index 1, table 2"
	requireStatus("Ready", metav1.ConditionTrue, v1alpha1.ReasonChecked)
	requireStatus("Reconciling", metav1.ConditionFalse, v1alpha1.ReasonChecked)
	requireStatus("Stalled", metav1.ConditionFalse, v1alpha1.ReasonChecked)
	requireStatus("Drifted", metav1.ConditionTrue, v1alpha1.ReasonDriftDetected)
	require.Equal(t, msg, cond("Drifted").Message)
	require.Equal(t, "fp1", res.Status.Fingerprint)
	require.Equal(t, &v1alpha1.DriftSummary{
		Total: 3, Extra: 1, Missing: 1, Modified: 1,
		Types: map[string]int{"table": 2, "index": 1},
	}, res.Status.Summary)

	// The same drift with onDrift=Fail degrades the check.
	res.SetDrifted(drifted, v1alpha1.DriftActionFail)
	requireStatus("Ready", metav1.ConditionFalse, v1alpha1.ReasonDriftDetected)
	requireStatus("Reconciling", metav1.ConditionFalse, v1alpha1.ReasonDriftDetected)
	requireStatus("Stalled", metav1.ConditionTrue, v1alpha1.ReasonDriftDetected)
	requireStatus("Drifted", metav1.ConditionTrue, v1alpha1.ReasonDriftDetected)
	require.Equal(t, msg, cond("Ready").Message)

	// A transient failure keeps reconciling and the last result.
	res.SetCheckFailed(v1alpha1.ReasonCheckFailed, "dial tcp: connection refused", false)
	requireStatus("Ready", metav1.ConditionFalse, v1alpha1.ReasonCheckFailed)
	requireStatus("Reconciling", metav1.ConditionTrue, v1alpha1.ReasonCheckFailed)
	requireStatus("Stalled", metav1.ConditionFalse, v1alpha1.ReasonCheckFailed)
	requireStatus("Drifted", metav1.ConditionUnknown, v1alpha1.ReasonCheckFailed)
	require.Equal(t, "fp1", res.Status.Fingerprint)

	// A permanent failure stalls it.
	res.SetCheckFailed(v1alpha1.ReasonNoMigrationHistory, "no migration history found", true)
	requireStatus("Ready", metav1.ConditionFalse, v1alpha1.ReasonNoMigrationHistory)
	requireStatus("Reconciling", metav1.ConditionFalse, v1alpha1.ReasonNoMigrationHistory)
	requireStatus("Stalled", metav1.ConditionTrue, v1alpha1.ReasonNoMigrationHistory)
	requireStatus("Drifted", metav1.ConditionUnknown, v1alpha1.ReasonNoMigrationHistory)

	// A missing target is permanent.
	res.SetCheckFailed(v1alpha1.ReasonTargetNotFound, `AtlasMigration "app" not found`, true)
	requireStatus("Ready", metav1.ConditionFalse, v1alpha1.ReasonTargetNotFound)
	requireStatus("Stalled", metav1.ConditionTrue, v1alpha1.ReasonTargetNotFound)
	requireStatus("Drifted", metav1.ConditionUnknown, v1alpha1.ReasonTargetNotFound)

	// A busy target only flips Reconciling, the rest is left alone.
	res.SetChecked(&atlasexec.MigrateDrift{Mode: "local", Version: "3"})
	res.SetTargetNotReady("migration apply in progress")
	requireStatus("Reconciling", metav1.ConditionTrue, v1alpha1.ReasonTargetNotReady)
	requireStatus("Ready", metav1.ConditionTrue, v1alpha1.ReasonChecked)
	requireStatus("Stalled", metav1.ConditionFalse, v1alpha1.ReasonChecked)
	requireStatus("Drifted", metav1.ConditionFalse, v1alpha1.ReasonNoDrift)
	require.Equal(t, "3", res.Status.Version)

	// Suspending touches Reconciling and the observed generation only.
	res.Generation = 8
	res.SetSuspended()
	require.Equal(t, int64(8), res.Status.ObservedGeneration)
	require.Equal(t, metav1.ConditionFalse, cond("Reconciling").Status)
	require.Equal(t, v1alpha1.ReasonSuspended, cond("Reconciling").Reason)
	require.Equal(t, metav1.ConditionTrue, cond("Ready").Status)
	require.Equal(t, "3", res.Status.Version)
}

func TestAtlasMigration_IsReconciling(t *testing.T) {
	res := &v1alpha1.AtlasMigration{}
	require.False(t, res.IsReconciling())
	res.SetReconciling("applying")
	require.True(t, res.IsReconciling())
	res.SetReady(v1alpha1.AtlasMigrationStatus{})
	require.False(t, res.IsReconciling())
	// A stalled migration is not reconciling: the check must still run.
	res.SetNotReady(v1alpha1.ReasonMigrating, "boom")
	require.False(t, res.IsReconciling())
}
