package controllers

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ariga.io/atlas/sql/migrate"
	"github.com/ariga/atlas-operator/api/v1alpha1"
	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
	"github.com/ariga/atlas-operator/controllers/watch"
	"github.com/ariga/atlas-operator/internal/atlas"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestReconcile_Notfound(t *testing.T) {
	tt := newMigrationTest(t)
	result, err := tt.r.Reconcile(context.Background(), migrationReq())
	require.NoError(tt, err)
	require.EqualValues(tt, reconcile.Result{}, result)
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
	require.Contains(tt, status.Conditions[0].Message, "sql/migrate: execute: executing statement")
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
	require.Contains(tt, status.Conditions[0].Message, "cannot define both configmap and local directory")
}

func TestReconcile_Transient(t *testing.T) {
	tt := newMigrationTest(t)
	tt.k8s.put(&dbv1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Spec: dbv1alpha1.AtlasMigrationSpec{
			URLFrom: v1alpha1.URLFrom{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "other-secret",
					},
					Key: "token",
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
}

func TestReconcile_reconcile(t *testing.T) {
	tt := migrationCliTest(t)
	tt.initDefaultMigrationDir()

	md, _, err := tt.r.extractMigrationData(context.Background(), v1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Spec: v1alpha1.AtlasMigrationSpec{
			URL: tt.dburl,
			Dir: v1alpha1.Dir{
				ConfigMapRef: &corev1.LocalObjectReference{Name: "my-configmap"},
			},
		},
	})
	require.NoError(t, err)

	status, err := tt.r.reconcile(context.Background(), md)
	require.NoError(t, err)
	require.EqualValues(t, "20230412003626", status.LastAppliedVersion)
}

func TestReconcile_reconciling(t *testing.T) {
	tt := newMigrationTest(t)
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
	}
	tt.k8s.put(am)

	result, err := tt.r.Reconcile(context.Background(), migrationReq())
	require.NoError(t, err)
	require.EqualValues(t, reconcile.Result{Requeue: true}, result)
	tt.k8s.Get(context.Background(), migrationReq().NamespacedName, am)
	require.EqualValues(t, metav1.ConditionFalse, am.Status.Conditions[0].Status)
	require.EqualValues(t, "Reconciling", am.Status.Conditions[0].Reason)

}

func TestReconcile_reconcile_uptodate(t *testing.T) {
	tt := migrationCliTest(t)
	tt.initDefaultMigrationDir()
	tt.k8s.put(&dbv1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Status: dbv1alpha1.AtlasMigrationStatus{
			LastAppliedVersion: "20230412003626",
		},
	})

	md, _, err := tt.r.extractMigrationData(context.Background(), v1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Spec: v1alpha1.AtlasMigrationSpec{
			URL: tt.dburl,
			Dir: v1alpha1.Dir{
				ConfigMapRef: &corev1.LocalObjectReference{Name: "my-configmap"},
			},
		},
	})
	require.NoError(t, err)

	status, err := tt.r.reconcile(context.Background(), md)
	require.NoError(t, err)
	require.EqualValues(t, "20230412003626", status.LastAppliedVersion)
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
	value, err := getSecretValue(context.Background(), tt.r, "default", corev1.SecretKeySelector{
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
	value, err := getSecretValue(context.Background(), tt.r, "default", corev1.SecretKeySelector{
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

	amd, cleanUp, err := tt.r.extractMigrationData(context.Background(), v1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Spec: v1alpha1.AtlasMigrationSpec{
			URL: tt.dburl,
			Dir: v1alpha1.Dir{
				ConfigMapRef: &corev1.LocalObjectReference{Name: "my-configmap"},
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, tt.dburl, amd.URL)
	parse, err := url.Parse(amd.Migration.Dir)
	require.NoError(t, err)
	require.DirExists(t, parse.Path)
	cleanUp()

}

func TestReconcile_extractCloudMigrationData(t *testing.T) {
	tt := migrationCliTest(t)
	tt.initDefaultTokenSecret()

	amd, cleanUp, err := tt.r.extractMigrationData(context.Background(), v1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Spec: v1alpha1.AtlasMigrationSpec{
			URL: tt.dburl,
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
	require.Equal(t, tt.dburl, amd.URL)
	require.Equal(t, "https://atlasgo.io/", amd.Cloud.URL)
	require.Equal(t, "my-project", amd.Cloud.Project)
	require.Equal(t, "my-token", amd.Cloud.Token)
	require.Equal(t, "my-remote-dir", amd.Cloud.RemoteDir.Name)
	require.Equal(t, "my-remote-tag", amd.Cloud.RemoteDir.Tag)
	cleanUp()
}

func TestReconcile_createTmpDirFromMap(t *testing.T) {
	tt := newMigrationTest(t)

	// When the configmap exists
	dir, cleanUp, err := tt.r.createTmpDirFromMap(context.Background(), map[string]string{
		"20230412003626_create_foo.sql": "CREATE TABLE foo (id INT PRIMARY KEY);",
		"atlas.sum": `h1:i2OZ2waAoNC0T8LDtu90qFTpbiYcwTNLOrr5YUrq8+g=
		20230412003626_create_foo.sql h1:8C7Hz48VGKB0trI2BsK5FWpizG6ttcm9ep+tX32y0Tw=`,
	})
	require.NoError(t, err)
	parse, err := url.Parse(dir)
	require.NoError(t, err)
	require.DirExists(t, parse.Path)
	files, err := ioutil.ReadDir(parse.Path)
	require.NoError(t, err)
	require.Len(t, files, 2)
	cleanUp()
	require.NoDirExists(t, parse.Path)
}

func TestReconcile_createTmpDirFromCfgMap(t *testing.T) {
	tt := newMigrationTest(t)
	tt.initDefaultMigrationDir()

	// When the configmap exists
	dir, cleanUp, err := tt.r.createTmpDirFromCfgMap(context.Background(), "default", "my-configmap")
	require.NoError(t, err)
	parse, err := url.Parse(dir)
	require.NoError(t, err)
	require.DirExists(t, parse.Path)
	files, err := ioutil.ReadDir(parse.Path)
	require.NoError(t, err)
	require.Len(t, files, 2)
	cleanUp()
	require.NoDirExists(t, parse.Path)
}

func TestReconcile_createTmpDirFromCfgMap_notfound(t *testing.T) {
	tt := newMigrationTest(t)
	tt.initDefaultMigrationDir()

	// When the configmap does not exist
	_, _, err := tt.r.createTmpDirFromCfgMap(context.Background(), "default", "other-configmap")
	require.Error(t, err)
	require.Equal(t, " \"other-configmap\" not found", err.Error())
}

func TestReconciler_watch(t *testing.T) {
	tt := newMigrationTest(t)

	tt.r.watch(dbv1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Spec: dbv1alpha1.AtlasMigrationSpec{
			URLFrom: v1alpha1.URLFrom{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "database-connection",
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
	migrate := atlasMigrationData{}
	migrate.Migration = &migration{
		Dir: "my-dir",
	}

	file, cleanup, err := migrate.render()
	require.NoError(t, err)
	parse, err := url.Parse(file)
	require.NoError(t, err)
	fileContent, err := os.ReadFile(parse.Path)
	require.NoError(t, err)
	require.FileExists(t, parse.Path)
	require.EqualValues(t, `
env {
  name = atlas.env
  url = ""
  migration {
    dir = "my-dir"
  }
}`, string(fileContent))
	cleanup()
	require.NoFileExists(t, file)
}

func TestCloudTemplate(t *testing.T) {
	migrate := atlasMigrationData{}
	migrate.Cloud = &cloud{
		URL:     "https://atlasgo.io/",
		Project: "my-project",
		Token:   "my-token",
		RemoteDir: &remoteDir{
			Name: "my-remote-dir",
			Tag:  "my-remote-tag",
		},
	}

	file, cleanup, err := migrate.render()
	require.NoError(t, err)
	parse, err := url.Parse(file)
	require.NoError(t, err)
	fileContent, err := os.ReadFile(parse.Path)
	require.NoError(t, err)
	require.FileExists(t, parse.Path)
	require.EqualValues(t, `
atlas {
  cloud {
    token = "my-token"
    url = "https://atlasgo.io/"
    project = "my-project"
  }
}
data "remote_dir" "this" {
  name = "my-remote-dir"
  tag = "my-remote-tag"
}
env {
  name = atlas.env
  url = ""
  migration {
    dir = data.remote_dir.this.url
  }
}`, string(fileContent))
	cleanup()
	require.NoFileExists(t, file)
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
	wd, err := os.Getwd()
	require.NoError(t, err)
	cli, err := atlas.NewClient(wd, "atlas")
	require.NoError(t, err)
	tt.r.CLI = cli
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
	secretWatcher := watch.New()
	configMapWatcher := watch.New()
	m := &mockClient{
		state: map[client.ObjectKey]client.Object{},
	}
	return &migrationTest{
		T:   t,
		k8s: m,
		r: &AtlasMigrationReconciler{
			Client:           m,
			Scheme:           scheme,
			secretWatcher:    &secretWatcher,
			configMapWatcher: &configMapWatcher,
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

	// Create a temporary directory dir with a new configmap
	dirUrl, cleanUp, err := t.r.createTmpDirFromCfgMap(context.Background(), "default", "my-configmap")
	require.NoError(t, err)
	defer cleanUp()
	u, err := url.Parse(dirUrl)
	require.NoError(t, err)
	require.DirExists(t, u.Path)

	// Recalculate atlas.sum
	ld, err := migrate.NewLocalDir(u.Path)
	require.NoError(t, err)
	checkSum, err := ld.Checksum()
	require.NoError(t, err)
	atlasSum, err := checkSum.MarshalText()
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
				URL: t.dburl,
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
			URL: t.dburl,
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
