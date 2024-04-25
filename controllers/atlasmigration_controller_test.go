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

package controllers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ariga.io/atlas-go-sdk/atlasexec"
	"ariga.io/atlas/sql/migrate"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ariga/atlas-operator/api/v1alpha1"
	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
	"github.com/ariga/atlas-operator/controllers/watch"
)

func TestReconcile_Notfound(t *testing.T) {
	obj := &dbv1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
	}
	_, run := newRunner(NewAtlasMigrationReconciler, nil)
	// Nope when the object is not found
	run(obj, func(result ctrl.Result, err error) {
		require.NoError(t, err)
		require.EqualValues(t, reconcile.Result{}, result)
	})
}

func TestMigration_ConfigMap(t *testing.T) {
	meta := migrationObjmeta()
	obj := &dbv1alpha1.AtlasMigration{
		ObjectMeta: meta,
		Spec: dbv1alpha1.AtlasMigrationSpec{
			TargetSpec: v1alpha1.TargetSpec{URL: "sqlite://file2/?mode=memory"},
			Dir: v1alpha1.Dir{
				ConfigMapRef: &corev1.LocalObjectReference{Name: "migrations-dir"},
			},
		},
	}
	h, reconcile := newRunner(NewAtlasMigrationReconciler, func(cb *fake.ClientBuilder) {
		cb.WithStatusSubresource(obj)
		cb.WithObjects(obj, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "migrations-dir",
				Namespace: "default",
			},
			Data: map[string]string{
				"20230412003626_create_foo.sql": "CREATE TABLE foo (id INT PRIMARY KEY);",
				"atlas.sum": `h1:i2OZ2waAoNC0T8LDtu90qFTpbiYcwTNLOrr5YUrq8+g=
				20230412003626_create_foo.sql h1:8C7Hz48VGKB0trI2BsK5FWpizG6ttcm9ep+tX32y0Tw=`,
			},
		})
	})
	assert := func(except ctrl.Result, ready bool, reason, msg, version string) {
		t.Helper()
		reconcile(obj, func(result ctrl.Result, err error) {
			require.NoError(t, err)
			require.EqualValues(t, except, result)
			res := &dbv1alpha1.AtlasMigration{ObjectMeta: meta}
			h.get(t, res)
			require.Len(t, res.Status.Conditions, 1)
			require.Equal(t, ready, res.IsReady())
			require.Equal(t, reason, res.Status.Conditions[0].Reason)
			require.Contains(t, res.Status.Conditions[0].Message, msg)
			require.Equal(t, version, res.Status.LastAppliedVersion)
		})
	}
	newDir := func(dir map[string]string) {
		t.Helper()
		h.patch(t, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "migrations-dir",
				Namespace: "default",
			},
			Data: dir,
		})
	}
	// First reconcile
	assert(ctrl.Result{Requeue: true}, false, "Reconciling", "Reconciling", "")
	// Second reconcile
	assert(ctrl.Result{}, true, "Applied", "", "20230412003626")
	// Third reconcile, should not change the status
	assert(ctrl.Result{}, true, "Applied", "", "20230412003626")
	// Update the migration script
	newDir(map[string]string{
		"20230412003626_create_foo.sql": "CREATE TABLE foo (id INT PRIMARY KEY);",
		"20230808132722_add-boo.sql":    "CREATE TABLE boo (id INT PRIMARY KEY);",
		"atlas.sum": `h1:zgFwhjzwhLZr82YtR4+PijDiVYNxwr18C3EqZtG4wyE=
		20230412003626_create_foo.sql h1:8C7Hz48VGKB0trI2BsK5FWpizG6ttcm9ep+tX32y0Tw=
		20230808132722_add-boo.sql h1:tD/Qak7Q4n0bp9wO8bjWYhRRcgp+oYcUDQIumztpYpg=`,
	})
	// Fourth reconcile, should change the status to Reconciling
	assert(ctrl.Result{Requeue: true}, false, "Reconciling", "Current migration data has changed", "20230412003626")
	// Fifth reconcile, should change the status to Applied
	assert(ctrl.Result{}, true, "Applied", "", "20230808132722")
	// Update the migration script with bad SQL
	newDir(map[string]string{
		"20230412003626_create_foo.sql": "CREATE TABLE foo (id INT PRIMARY KEY);",
		"20230808132722_add-boo.sql":    "CREATE TABLE boo (id INT PRIMARY KEY);",
		"20230808140359_bad-sql.sql":    "SYNTAX ERROR",
		"atlas.sum": `h1:YLWIn4Si2uYnPM1EpUHk9LT1/6a5DuAdMFwoa9RV7cA=
		20230412003626_create_foo.sql h1:8C7Hz48VGKB0trI2BsK5FWpizG6ttcm9ep+tX32y0Tw=
		20230808132722_add-boo.sql h1:tD/Qak7Q4n0bp9wO8bjWYhRRcgp+oYcUDQIumztpYpg=
		20230808140359_bad-sql.sql h1:8eWRotAPx27YMgDJ3AjziZz947VGEiDzk3rYcmp1P7k=`,
	})
	// Sixth reconcile, should change the status to Reconciling
	assert(ctrl.Result{Requeue: true}, false, "Reconciling", "Current migration data has changed", "20230808132722")
	// Seventh reconcile, should change the status to Failed
	assert(ctrl.Result{}, false, "Migrating", `"SYNTAX ERROR" from version "20230808140359"`, "20230808132722")
	// Check the events generated by the controller
	require.Equal(t, []string{
		"Normal Applied Version 20230412003626 applied",
		"Normal Applied Version 20230412003626 applied",
		"Normal Applied Version 20230808132722 applied",
		`Warning Error sql/migrate: executing statement "SYNTAX ERROR" from version "20230808140359": near "SYNTAX": syntax error`,
	}, h.events())
}

func TestMigration_Local(t *testing.T) {
	meta := migrationObjmeta()
	obj := &dbv1alpha1.AtlasMigration{
		ObjectMeta: meta,
		Spec: dbv1alpha1.AtlasMigrationSpec{
			TargetSpec: v1alpha1.TargetSpec{URL: "sqlite://file2/?mode=memory"},
			Dir: v1alpha1.Dir{
				Local: map[string]string{
					"20230412003626_create_foo.sql": "CREATE TABLE foo (id INT PRIMARY KEY);",
					"atlas.sum": `h1:i2OZ2waAoNC0T8LDtu90qFTpbiYcwTNLOrr5YUrq8+g=
					20230412003626_create_foo.sql h1:8C7Hz48VGKB0trI2BsK5FWpizG6ttcm9ep+tX32y0Tw=`,
				},
			},
		},
	}
	h, reconcile := newRunner(NewAtlasMigrationReconciler, func(cb *fake.ClientBuilder) {
		cb.WithStatusSubresource(obj)
		cb.WithObjects(obj)
	})
	assert := func(except ctrl.Result, ready bool, reason, msg, version string) {
		t.Helper()
		reconcile(obj, func(result ctrl.Result, err error) {
			require.NoError(t, err)
			require.EqualValues(t, except, result)
			res := &dbv1alpha1.AtlasMigration{ObjectMeta: meta}
			h.get(t, res)
			require.Len(t, res.Status.Conditions, 1)
			require.Equal(t, ready, res.IsReady())
			require.Equal(t, reason, res.Status.Conditions[0].Reason)
			require.Contains(t, res.Status.Conditions[0].Message, msg)
			require.Equal(t, version, res.Status.LastAppliedVersion)
		})
	}
	updateDir := func(dir map[string]string) {
		t.Helper()
		h.patch(t, &dbv1alpha1.AtlasMigration{
			ObjectMeta: meta,
			Spec: dbv1alpha1.AtlasMigrationSpec{
				Dir: v1alpha1.Dir{Local: dir},
			},
		})
	}
	assertDir := func(dirMap map[string]string) {
		t.Helper()
		// Check the content of the tarball
		h.get(t, &dbv1alpha1.AtlasMigration{ObjectMeta: meta})
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      makeKeyLatest("atlas-migration"),
				Namespace: "default",
			},
		}
		h.get(t, secret)
		dir, err := extractDirFromSecret(secret)
		require.NoError(t, err)
		require.NotNil(t, dir)
		// It should contain the same files as the local directory
		testContent(t, dirMap, dir)
	}
	// First reconcile
	assert(ctrl.Result{Requeue: true}, false, "Reconciling", "Reconciling", "")
	// Second reconcile
	assert(ctrl.Result{}, true, "Applied", "", "20230412003626")
	assertDir(obj.Spec.Dir.Local)
	// Third reconcile, should not change the status
	assert(ctrl.Result{}, true, "Applied", "", "20230412003626")
	// Update the migration script
	newDir := map[string]string{
		"20230412003626_create_foo.sql": "CREATE TABLE foo (id INT PRIMARY KEY);",
		"20230808132722_add-boo.sql":    "CREATE TABLE boo (id INT PRIMARY KEY);",
		"atlas.sum": `h1:zgFwhjzwhLZr82YtR4+PijDiVYNxwr18C3EqZtG4wyE=
		20230412003626_create_foo.sql h1:8C7Hz48VGKB0trI2BsK5FWpizG6ttcm9ep+tX32y0Tw=
		20230808132722_add-boo.sql h1:tD/Qak7Q4n0bp9wO8bjWYhRRcgp+oYcUDQIumztpYpg=`,
	}
	updateDir(newDir)
	// Fourth reconcile, should change the status to Reconciling
	assert(ctrl.Result{Requeue: true}, false, "Reconciling", "Current migration data has changed", "20230412003626")
	// The content should not change during the migration
	assertDir(obj.Spec.Dir.Local)
	// Fifth reconcile, should change the status to Applied
	assert(ctrl.Result{}, true, "Applied", "", "20230808132722")
	// The content should change to the new directory
	assertDir(newDir)
	// Update the migration script with bad SQL
	updateDir(map[string]string{
		"20230412003626_create_foo.sql": "CREATE TABLE foo (id INT PRIMARY KEY);",
		"20230808132722_add-boo.sql":    "CREATE TABLE boo (id INT PRIMARY KEY);",
		"20230808140359_bad-sql.sql":    "SYNTAX ERROR",
		"atlas.sum": `h1:YLWIn4Si2uYnPM1EpUHk9LT1/6a5DuAdMFwoa9RV7cA=
		20230412003626_create_foo.sql h1:8C7Hz48VGKB0trI2BsK5FWpizG6ttcm9ep+tX32y0Tw=
		20230808132722_add-boo.sql h1:tD/Qak7Q4n0bp9wO8bjWYhRRcgp+oYcUDQIumztpYpg=
		20230808140359_bad-sql.sql h1:8eWRotAPx27YMgDJ3AjziZz947VGEiDzk3rYcmp1P7k=`,
	})
	// Sixth reconcile, should change the status to Reconciling
	assert(ctrl.Result{Requeue: true}, false, "Reconciling", "Current migration data has changed", "20230808132722")
	// Seventh reconcile, should change the status to Failed
	assert(ctrl.Result{}, false, "Migrating", `"SYNTAX ERROR" from version "20230808140359"`, "20230808132722")
	// The content should not change when the migration fails
	assertDir(newDir)
	// Check the events generated by the controller
	require.Equal(t, []string{
		"Normal Applied Version 20230412003626 applied",
		"Normal Applied Version 20230412003626 applied",
		"Normal Applied Version 20230808132722 applied",
		`Warning Error sql/migrate: executing statement "SYNTAX ERROR" from version "20230808140359": near "SYNTAX": syntax error`,
	}, h.events())
}

func TestReconcile_Diff(t *testing.T) {
	tt := migrationCliTest(t)
	tt.initDefaultAtlasMigration()

	// First reconcile
	result, err := tt.r.Reconcile(context.Background(), migrationReq())
	require.NoError(tt, err)
	require.EqualValues(tt, reconcile.Result{}, result)

	status := tt.status()
	require.EqualValues(tt, "20230412003626", status.LastAppliedVersion)

	// Second reconcile (change to in-progress status)
	tt.addMigrationScript("20230412003627_create_bar.sql", "CREATE TABLE bar (id INT PRIMARY KEY);")
	result, err = tt.r.Reconcile(context.Background(), migrationReq())
	require.NoError(tt, err)
	require.EqualValues(tt, reconcile.Result{Requeue: true}, result)

	// Third reconcile
	result, err = tt.r.Reconcile(context.Background(), migrationReq())
	require.NoError(tt, err)
	require.EqualValues(tt, reconcile.Result{}, result)
	status = tt.status()
	fmt.Println(status.Conditions[0].Message)
	require.EqualValues(tt, "20230412003627", status.LastAppliedVersion)

	// Fourth reconcile without any modification

	result, err = tt.r.Reconcile(context.Background(), migrationReq())
	require.NoError(tt, err)
	require.EqualValues(tt, reconcile.Result{}, result)
	status = tt.status()
	fmt.Println(status.Conditions[0].Message)
	require.EqualValues(tt, "20230412003627", status.LastAppliedVersion)
}

func TestReconcile_BadSQL(t *testing.T) {
	tt := migrationCliTest(t)
	tt.initDefaultAtlasMigration()

	// First reconcile
	result, err := tt.r.Reconcile(context.Background(), migrationReq())
	require.NoError(tt, err)
	require.EqualValues(tt, reconcile.Result{}, result)

	status := tt.status()
	require.EqualValues(tt, "20230412003626", status.LastAppliedVersion)

	// Second reconcile
	tt.addMigrationScript("20230412003627_bad_sql.sql", "BAD SQL")
	result, err = tt.r.Reconcile(context.Background(), migrationReq())
	require.NoError(tt, err)
	require.EqualValues(tt, reconcile.Result{Requeue: true}, result)

	// Third migration
	tt.addMigrationScript("20230412003627_bad_sql.sql", "BAD SQL")
	result, err = tt.r.Reconcile(context.Background(), migrationReq())
	require.NoError(tt, err)
	require.EqualValues(tt, reconcile.Result{}, result)

	status = tt.status()
	require.EqualValues(tt, metav1.ConditionFalse, status.Conditions[0].Status)
	require.Contains(tt, status.Conditions[0].Message, "sql/migrate: executing statement")
}

func TestReconcile_LocalMigrationDir(t *testing.T) {
	tt := migrationCliTest(t)
	am := tt.getAtlasMigration()
	am.Spec.Dir.Local = map[string]string{
		"20230412003626_create_foo.sql": "CREATE TABLE foo (id INT PRIMARY KEY);",
		"atlas.sum": `h1:i2OZ2waAoNC0T8LDtu90qFTpbiYcwTNLOrr5YUrq8+g=
		20230412003626_create_foo.sql h1:8C7Hz48VGKB0trI2BsK5FWpizG6ttcm9ep+tX32y0Tw=`,
	}
	tt.k8s.put(am)

	result, err := tt.r.Reconcile(context.Background(), migrationReq())
	require.NoError(tt, err)
	require.EqualValues(tt, reconcile.Result{}, result)

	status := tt.status()
	require.EqualValues(tt, "20230412003626", status.LastAppliedVersion)

	fsDir, err := getSecretValue(context.Background(), tt.r, "default", &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{
			Name: makeKeyLatest("atlas-migration"),
		},
		Key: "migrations.tar.gz",
	})
	require.NoError(t, err)
	require.NotNil(t, fsDir)

	// Check the content of the tarball
	dir, err := tt.r.readDirState(context.Background(), am)
	require.NoError(t, err)
	require.NotNil(t, dir)
	// It should contain the same files as the local directory
	testContent(t, am.Spec.Dir.Local, dir)
}

func testContent(t *testing.T, files map[string]string, dir fs.FS) {
	t.Helper()
	for f, c := range files {
		foo, err := fs.ReadFile(dir, f)
		require.NoError(t, err)
		require.EqualValues(t, c, string(foo))
	}
}

func TestReconcile_LocalMigrationDir_ConfigMap(t *testing.T) {
	tt := migrationCliTest(t)
	tt.initDefaultMigrationDir()
	am := tt.getAtlasMigration()
	am.Spec.Dir.ConfigMapRef = &corev1.LocalObjectReference{Name: "my-configmap"}
	am.Spec.Dir.Local = map[string]string{}

	tt.k8s.put(am)

	result, err := tt.r.Reconcile(context.Background(), migrationReq())
	require.NoError(tt, err)
	require.EqualValues(tt, reconcile.Result{}, result)

	status := tt.status()
	require.EqualValues(tt, metav1.ConditionFalse, status.Conditions[0].Status)
	require.Contains(tt, status.Conditions[0].Message, "cannot use both configmaps and local directory")
}

func TestReconcile_Transient(t *testing.T) {
	tt := newMigrationTest(t)
	tt.k8s.put(&dbv1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Spec: dbv1alpha1.AtlasMigrationSpec{
			TargetSpec: v1alpha1.TargetSpec{
				URLFrom: v1alpha1.Secret{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "other-secret",
						},
						Key: "token",
					},
				},
			},
		},
		Status: v1alpha1.AtlasMigrationStatus{
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionFalse,
				},
			},
		},
	})
	result, err := tt.r.Reconcile(context.Background(), migrationReq())
	require.NoError(t, err)
	require.EqualValues(t, reconcile.Result{RequeueAfter: 5 * time.Second}, result)
	require.Equal(t, []string{
		`Warning TransientErr "other-secret" not found`,
	}, tt.events())
}

func TestReconcile_InvalidChecksum(t *testing.T) {
	tt := migrationCliTest(t)
	am := tt.getAtlasMigration()
	am.Spec.Dir.Local = map[string]string{
		"1.sql":     "foo",
		"atlas.sum": `invalid checksum`,
	}
	tt.k8s.put(am)
	result, err := tt.r.Reconcile(context.Background(), migrationReq())
	require.NoError(t, err)
	require.EqualValues(t, reconcile.Result{}, result)
	require.Contains(t, am.Status.Conditions[0].Message, "checksum mismatch")
}

func TestReconcile_reconcile(t *testing.T) {
	tt := migrationCliTest(t)
	tt.initDefaultMigrationDir()

	md, err := tt.r.extractData(context.Background(), &v1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Spec: v1alpha1.AtlasMigrationSpec{
			TargetSpec: v1alpha1.TargetSpec{URL: tt.dburl},
			Dir: v1alpha1.Dir{
				ConfigMapRef: &corev1.LocalObjectReference{Name: "my-configmap"},
			},
		},
	})
	require.NoError(t, err)
	wd, err := atlasexec.NewWorkingDir(
		atlasexec.WithAtlasHCL(md.render),
		atlasexec.WithMigrations(md.Dir),
	)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, wd.Close())
	}()
	status, err := tt.r.reconcile(context.Background(), wd.Path(), "test")
	require.NoError(t, err)
	require.EqualValues(t, "20230412003626", status.LastAppliedVersion)
}

func TestReconcile_reconciling(t *testing.T) {
	tt := migrationCliTest(t)
	am := &dbv1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Status: v1alpha1.AtlasMigrationStatus{
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionTrue,
				},
			},
		},
		Spec: v1alpha1.AtlasMigrationSpec{
			TargetSpec: v1alpha1.TargetSpec{URL: tt.dburl},
			Dir: v1alpha1.Dir{
				Local: map[string]string{
					"1.sql": "bar",
				},
			},
		},
	}
	tt.k8s.put(am)

	result, err := tt.r.Reconcile(context.Background(), migrationReq())
	require.NoError(t, err)
	// Second reconcile, the status is already reconciling
	require.EqualValues(t, reconcile.Result{Requeue: true}, result)
	tt.k8s.Get(context.Background(), migrationReq().NamespacedName, am)
	require.EqualValues(t, metav1.ConditionFalse, am.Status.Conditions[0].Status)
	require.EqualValues(t, "Reconciling", am.Status.Conditions[0].Reason)
}

func TestReconcile_reconcile_upToDate(t *testing.T) {
	tt := migrationCliTest(t)
	tt.initDefaultMigrationDir()
	tt.k8s.put(&dbv1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Status: dbv1alpha1.AtlasMigrationStatus{
			LastAppliedVersion: "20230412003626",
		},
	})
	md, err := tt.r.extractData(context.Background(), &v1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Spec: v1alpha1.AtlasMigrationSpec{
			TargetSpec: v1alpha1.TargetSpec{URL: tt.dburl},
			Dir: v1alpha1.Dir{
				ConfigMapRef: &corev1.LocalObjectReference{Name: "my-configmap"},
			},
		},
	})
	require.NoError(t, err)
	wd, err := atlasexec.NewWorkingDir(
		atlasexec.WithAtlasHCL(md.render),
		atlasexec.WithMigrations(md.Dir),
	)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, wd.Close())
	}()
	status, err := tt.r.reconcile(context.Background(), wd.Path(), "test")
	require.NoError(t, err)
	require.EqualValues(t, "20230412003626", status.LastAppliedVersion)
}

func TestReconcile_reconcile_baseline(t *testing.T) {
	tt := migrationCliTest(t)
	tt.initDefaultMigrationDir()
	tt.addMigrationScript("20230412003627_create_bar.sql", "CREATE TABLE bar (id INT PRIMARY KEY);")
	tt.addMigrationScript("20230412003628_create_baz.sql", "CREATE TABLE baz (id INT PRIMARY KEY);")

	md, err := tt.r.extractData(context.Background(), &v1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Spec: v1alpha1.AtlasMigrationSpec{
			TargetSpec: v1alpha1.TargetSpec{URL: tt.dburl},
			Dir: v1alpha1.Dir{
				ConfigMapRef: &corev1.LocalObjectReference{Name: "my-configmap"},
			},
			Baseline: "20230412003627",
		},
	})
	require.NoError(t, err)
	wd, err := atlasexec.NewWorkingDir(
		atlasexec.WithAtlasHCL(md.render),
		atlasexec.WithMigrations(md.Dir),
	)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, wd.Close())
	}()
	status, err := tt.r.reconcile(context.Background(), wd.Path(), "test")
	require.NoError(t, err)
	require.EqualValues(t, "20230412003628", status.LastAppliedVersion)
	cli, err := atlasexec.NewClient(wd.Path(), tt.r.execPath)
	require.NoError(t, err)
	report, err := cli.MigrateStatus(context.Background(), &atlasexec.MigrateStatusParams{
		Env: "test",
	})
	require.NoError(t, err)
	require.EqualValues(t, 2, len(report.Applied))
	require.EqualValues(t, "20230412003627", report.Applied[0].Version)
	require.EqualValues(t, "baseline", report.Applied[0].Type)
}

func TestReconcile_getSecretValue(t *testing.T) {
	tt := migrationCliTest(t)
	tt.k8s.put(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-secret",
				Namespace: "default",
			},
			Data: map[string][]byte{
				`token`: []byte(`my-token`),
			},
		},
	)

	// When the secret exists
	value, err := getSecretValue(context.Background(), tt.r, "default", &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{
			Name: "my-secret",
		},
		Key: "token",
	})
	require.NoError(t, err)
	require.EqualValues(t, "my-token", value)
}

func TestReconcile_getSecretValue_notfound(t *testing.T) {
	tt := migrationCliTest(t)

	// When the secret does not exist
	value, err := getSecretValue(context.Background(), tt.r, "default", &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{
			Name: "other-secret",
		},
		Key: "",
	})
	require.EqualValues(t, "", value)
	require.Error(t, err)
	require.Equal(t, " \"other-secret\" not found", err.Error())
}

func TestReconcile_extractMigrationData(t *testing.T) {
	tt := migrationCliTest(t)
	tt.initDefaultMigrationDir()

	amd, err := tt.r.extractData(context.Background(), &v1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Spec: v1alpha1.AtlasMigrationSpec{
			TargetSpec: v1alpha1.TargetSpec{URL: tt.dburl},
			Dir: v1alpha1.Dir{
				ConfigMapRef: &corev1.LocalObjectReference{Name: "my-configmap"},
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, tt.dburl, amd.URL.String())
	require.NotNil(t, amd.Dir)
}

func TestReconcile_extractCloudMigrationData(t *testing.T) {
	tt := migrationCliTest(t)
	tt.initDefaultTokenSecret()

	amd, err := tt.r.extractData(context.Background(), &v1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Spec: v1alpha1.AtlasMigrationSpec{
			TargetSpec: v1alpha1.TargetSpec{URL: tt.dburl},
			Cloud: v1alpha1.Cloud{
				URL:     "https://atlasgo.io/",
				Project: "my-project",
				TokenFrom: v1alpha1.TokenFrom{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "my-secret",
						},
						Key: "token",
					},
				},
			},
			Dir: v1alpha1.Dir{
				Remote: v1alpha1.Remote{
					Name: "my-remote-dir",
					Tag:  "my-remote-tag",
				},
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, tt.dburl, amd.URL.String())
	require.Equal(t, "https://atlasgo.io/", amd.Cloud.URL)
	require.Equal(t, "my-project", amd.Cloud.Project)
	require.Equal(t, "my-token", amd.Cloud.Token)
	require.Equal(t, "my-remote-dir", amd.Cloud.RemoteDir.Name)
	require.Equal(t, "my-remote-tag", amd.Cloud.RemoteDir.Tag)
}

func TestReconciler_watch(t *testing.T) {
	tt := newMigrationTest(t)

	tt.r.watchRefs(&dbv1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Spec: dbv1alpha1.AtlasMigrationSpec{
			TargetSpec: v1alpha1.TargetSpec{
				URLFrom: v1alpha1.Secret{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "database-connection",
						},
					},
				},
			},
			Cloud: v1alpha1.Cloud{
				TokenFrom: v1alpha1.TokenFrom{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "atlas-token",
						},
					},
				},
			},
			Dir: v1alpha1.Dir{
				ConfigMapRef: &corev1.LocalObjectReference{Name: "migration-directory"},
			},
		},
	})

	// Watched database-connection, atlas-token and migration-directory
	dbWatched := tt.r.secretWatcher.Read(types.NamespacedName{Name: "database-connection", Namespace: "default"})
	require.EqualValues(t, []types.NamespacedName{
		{Name: "atlas-migration", Namespace: "default"},
	}, dbWatched)
	atWatched := tt.r.secretWatcher.Read(types.NamespacedName{Name: "atlas-token", Namespace: "default"})
	require.EqualValues(t, []types.NamespacedName{
		{Name: "atlas-migration", Namespace: "default"},
	}, atWatched)
	mdWatched := tt.r.configMapWatcher.Read(types.NamespacedName{Name: "migration-directory", Namespace: "default"})
	require.EqualValues(t, []types.NamespacedName{
		{Name: "atlas-migration", Namespace: "default"},
	}, mdWatched)
}

func TestAtlasMigrationReconciler_Credentials(t *testing.T) {
	tt := migrationCliTest(t)
	tt.k8s.put(&dbv1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Spec: dbv1alpha1.AtlasMigrationSpec{
			TargetSpec: v1alpha1.TargetSpec{
				Credentials: dbv1alpha1.Credentials{
					Scheme: "sqlite",
					Host:   "localhost",
					Parameters: map[string]string{
						"mode": "memory",
					},
				},
			},
			Dir: dbv1alpha1.Dir{
				Local: map[string]string{
					"20230412003626_create_foo.sql": "CREATE TABLE foo (id INT PRIMARY KEY);",
					"atlas.sum": `h1:i2OZ2waAoNC0T8LDtu90qFTpbiYcwTNLOrr5YUrq8+g=
				20230412003626_create_foo.sql h1:8C7Hz48VGKB0trI2BsK5FWpizG6ttcm9ep+tX32y0Tw=`,
				},
			},
		},
		Status: v1alpha1.AtlasMigrationStatus{
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionFalse,
				},
			},
		},
	})
	c, err := tt.r.Reconcile(context.Background(), migrationReq())
	require.NoError(tt, err)
	require.EqualValues(tt, reconcile.Result{}, c)
	ev := tt.events()
	require.Len(t, ev, 1)
	require.Equal(t, "Normal Applied Version 20230412003626 applied", ev[0])
}

func TestWatcher_enabled(t *testing.T) {
	tt := migrationCliTest(t)
	tt.initDefaultAtlasMigration()

	// First Reconcile
	result, err := tt.r.Reconcile(context.Background(), migrationReq())
	require.NoError(tt, err)
	require.EqualValues(tt, reconcile.Result{}, result)

	// Watched configmap
	watched := tt.r.configMapWatcher.Read(types.NamespacedName{Name: "my-configmap", Namespace: "default"})
	require.EqualValues(t, []types.NamespacedName{
		{Name: "atlas-migration", Namespace: "default"},
	}, watched)
}

func TestDefaultTemplate(t *testing.T) {
	migrate := &migrationData{
		URL: must(url.Parse("sqlite://file2/?mode=memory")),
		Dir: must(memDir(map[string]string{
			"1.sql": "CREATE TABLE foo (id INT PRIMARY KEY);",
		})),
	}
	var fileContent bytes.Buffer
	require.NoError(t, migrate.render(&fileContent))
	require.EqualValues(t, `
env {
  name = atlas.env
  url = "sqlite://file2/?mode=memory"
  migration {
    dir = "file://migrations"
  }
}`, fileContent.String())
}

func TestBaselineTemplate(t *testing.T) {
	migrate := &migrationData{
		URL:      must(url.Parse("sqlite://file2/?mode=memory")),
		Dir:      must(memDir(map[string]string{})),
		Baseline: "20230412003626",
	}
	var fileContent bytes.Buffer
	require.NoError(t, migrate.render(&fileContent))
	require.EqualValues(t, `
env {
  name = atlas.env
  url = "sqlite://file2/?mode=memory"
  migration {
    dir = "file://migrations"
    baseline = "20230412003626"
  }
}`, fileContent.String())
}

func TestCloudTemplate(t *testing.T) {
	migrate := &migrationData{
		URL: must(url.Parse("sqlite://file2/?mode=memory")),
		Cloud: &cloud{
			URL:     "https://atlasgo.io/",
			Project: "my-project",
			Token:   "my-token",
			RemoteDir: &v1alpha1.Remote{
				Name: "my-remote-dir",
				Tag:  "my-remote-tag",
			},
		},
	}
	var fileContent bytes.Buffer
	require.NoError(t, migrate.render(&fileContent))
	require.EqualValues(t, `
atlas {
  cloud {
    token = "my-token"
    url = "https://atlasgo.io/"
    project = "my-project"
  }
}
env {
  name = atlas.env
  url = "sqlite://file2/?mode=memory"
  migration {
    dir = "atlas://my-remote-dir?tag=my-remote-tag"
  }
}`, fileContent.String())
}

func TestMigrationWithDeploymentContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		type (
			RunContext struct {
				TriggerType    string `json:"triggerType,omitempty"`
				TriggerVersion string `json:"triggerVersion,omitempty"`
			}
			graphQLQuery struct {
				Query              string          `json:"query"`
				Variables          json.RawMessage `json:"variables"`
				MigrateApplyReport struct {
					Input struct {
						Context *RunContext `json:"context,omitempty"`
					} `json:"input"`
				}
			}
		)
		var m graphQLQuery
		require.NoError(t, json.NewDecoder(r.Body).Decode(&m))
		switch {
		case strings.Contains(m.Query, "query"):
			memdir := &migrate.MemDir{}
			memdir.WriteFile("30230412003626.sql", []byte(`CREATE TABLE foo (id INT PRIMARY KEY)`))
			writeDir(t, memdir, w)
		case strings.Contains(m.Query, "reportMigration"):
			err := json.Unmarshal(m.Variables, &m.MigrateApplyReport)
			require.NoError(t, err)
			require.Equal(t, "my-version", m.MigrateApplyReport.Input.Context.TriggerVersion)
			require.Equal(t, "KUBERNETES", m.MigrateApplyReport.Input.Context.TriggerType)
		}
	}))
	defer srv.Close()
	tt := migrationCliTest(t)
	tt.initDefaultTokenSecret()
	am := tt.getAtlasMigration()
	am.Spec.Cloud.URL = srv.URL
	am.Spec.Dir.Remote.Name = "my-remote-dir"
	am.Spec.Cloud.Project = "my-project"
	am.Spec.Cloud.TokenFrom = v1alpha1.TokenFrom{
		SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{
				Name: "my-secret",
			},
			Key: "token",
		},
	}
	tt.k8s.put(am)
	ctx := dbv1alpha1.WithVersionContext(context.Background(), "my-version")
	result, err := tt.r.Reconcile(ctx, migrationReq())
	require.NoError(tt, err)
	require.EqualValues(tt, reconcile.Result{}, result)
}

func migrationObjmeta() metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      "atlas-migration",
		Namespace: "default",
	}
}

func migrationReq() ctrl.Request {
	return ctrl.Request{
		NamespacedName: client.ObjectKey{
			Name:      "atlas-migration",
			Namespace: "default",
		},
	}
}

// migrationCliTest initializes a test with a real CLI and a temporary SQLite database.
func migrationCliTest(t *testing.T) *migrationTest {
	tt := newMigrationTest(t)
	var err error
	tt.r.execPath, err = exec.LookPath("atlas")
	require.NoError(t, err)
	td, err := os.MkdirTemp("", "operator-test-sqlite-*")
	require.NoError(t, err)
	tt.dburl = "sqlite://" + filepath.Join(td, "test.db")
	t.Cleanup(func() { os.RemoveAll(td) })
	return tt
}

type migrationTest struct {
	*testing.T
	k8s   *mockClient
	r     *AtlasMigrationReconciler
	dburl string
}

func newMigrationTest(t *testing.T) *migrationTest {
	scheme := runtime.NewScheme()
	dbv1alpha1.AddToScheme(scheme)
	m := &mockClient{
		state: map[client.ObjectKey]client.Object{},
	}
	return &migrationTest{
		T:   t,
		k8s: m,
		r: &AtlasMigrationReconciler{
			Client:           m,
			scheme:           scheme,
			secretWatcher:    watch.New(),
			configMapWatcher: watch.New(),
			recorder:         record.NewFakeRecorder(100),
		},
	}
}

func (t *migrationTest) status() dbv1alpha1.AtlasMigrationStatus {
	s := t.k8s.state[migrationReq().NamespacedName].(*dbv1alpha1.AtlasMigration)
	return s.Status
}

func (t *migrationTest) addMigrationScript(name, content string) {
	// Get the current configmap
	cm := corev1.ConfigMap{}
	err := t.k8s.Get(context.Background(), types.NamespacedName{
		Name:      "my-configmap",
		Namespace: "default",
	}, &cm)
	require.NoError(t, err)

	// Update the configmap
	cm.Data[name] = content
	t.k8s.put(&cm)

	sum, err := must(memDir(cm.Data)).Checksum()
	require.NoError(t, err)
	atlasSum, err := sum.MarshalText()
	require.NoError(t, err)
	cm.Data[migrate.HashFileName] = string(atlasSum)
	t.k8s.put(&cm)
}

func (t *migrationTest) initDefaultAtlasMigration() {
	t.initDefaultMigrationDir()
	t.initDefaultTokenSecret()
	t.k8s.put(
		&v1alpha1.AtlasMigration{
			ObjectMeta: migrationObjmeta(),
			Spec: v1alpha1.AtlasMigrationSpec{
				TargetSpec: v1alpha1.TargetSpec{URL: t.dburl},
				Dir: v1alpha1.Dir{
					ConfigMapRef: &corev1.LocalObjectReference{Name: "my-configmap"},
				},
			},
			Status: v1alpha1.AtlasMigrationStatus{
				Conditions: []metav1.Condition{
					{
						Type:   "Ready",
						Status: metav1.ConditionFalse,
					},
				},
			},
		},
	)
}

func (t *migrationTest) initDefaultMigrationDir() {
	t.k8s.put(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-configmap",
				Namespace: "default",
			},
			Data: map[string]string{
				"20230412003626_create_foo.sql": "CREATE TABLE foo (id INT PRIMARY KEY);",
				"atlas.sum": `h1:i2OZ2waAoNC0T8LDtu90qFTpbiYcwTNLOrr5YUrq8+g=
				20230412003626_create_foo.sql h1:8C7Hz48VGKB0trI2BsK5FWpizG6ttcm9ep+tX32y0Tw=`,
			},
		},
	)
}

func (t *migrationTest) getAtlasMigration() *v1alpha1.AtlasMigration {
	return &v1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Spec: v1alpha1.AtlasMigrationSpec{
			TargetSpec: v1alpha1.TargetSpec{URL: t.dburl},
		},
		Status: v1alpha1.AtlasMigrationStatus{
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionFalse,
				},
			},
		},
	}

}

func (t *migrationTest) initDefaultTokenSecret() {
	t.k8s.put(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			`token`: []byte(`my-token`),
		},
	})
}

func (t *migrationTest) events() []string {
	return events(t.r.recorder)
}

func writeDir(t *testing.T, dir migrate.Dir, w io.Writer) {
	// Checksum before archiving.
	hf, err := dir.Checksum()
	require.NoError(t, err)
	ht, err := hf.MarshalText()
	require.NoError(t, err)
	require.NoError(t, dir.WriteFile(migrate.HashFileName, ht))
	// Archive and send.
	arc, err := migrate.ArchiveDir(dir)
	require.NoError(t, err)
	_, err = fmt.Fprintf(w, `{"data":{"dirState":{"content":%q}}}`, base64.StdEncoding.EncodeToString(arc))
	require.NoError(t, err)
}
