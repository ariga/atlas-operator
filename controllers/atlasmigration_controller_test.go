package controllers

import (
	"context"
	"errors"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ariga/atlas-operator/api/v1alpha1"
	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
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

func TestReconcile_Transient(t *testing.T) {
	tt := newMigrationTest(t)
	tt.k8s.put(&dbv1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Spec: dbv1alpha1.AtlasMigrationSpec{
			Version: "latest",
			URLFrom: v1alpha1.URLFrom{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "other-secret",
					},
					Key: "token",
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
	setDefaultMigrationDir(tt)

	status, err := tt.r.reconcile(context.Background(), v1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Spec: v1alpha1.AtlasMigrationSpec{
			URL: tt.dburl,
			Dir: v1alpha1.Dir{
				ConfigMapRef: "my-configmap",
			},
		},
	})

	require.NoError(t, err)
	require.EqualValues(t, "20230412003626", status.LastAppliedVersion)
}

func TestReconcile_reconcile_uptodate(t *testing.T) {
	tt := migrationCliTest(t)
	setDefaultMigrationDir(tt)
	tt.k8s.put(&dbv1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Status: dbv1alpha1.AtlasMigrationStatus{
			LastAppliedVersion: "20230412003626",
		},
	})

	status, err := tt.r.reconcile(context.Background(), v1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Spec: v1alpha1.AtlasMigrationSpec{
			URL: tt.dburl,
			Dir: v1alpha1.Dir{
				ConfigMapRef: "my-configmap",
			},
		},
	})
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
	value, err := tt.r.getSecretValue(context.Background(), "default", corev1.SecretKeySelector{
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
	value, err := tt.r.getSecretValue(context.Background(), "default", corev1.SecretKeySelector{
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
	setDefaultMigrationDir(tt)

	amd, cleanUp, err := tt.r.extractMigrationData(context.Background(), v1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
		Spec: v1alpha1.AtlasMigrationSpec{
			URL: tt.dburl,
			Dir: v1alpha1.Dir{
				ConfigMapRef: "my-configmap",
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
	setDefaultTokenSecrect(tt)

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

func TestReconcile_createTmpDir(t *testing.T) {
	tt := newMigrationTest(t)
	setDefaultMigrationDir(tt)

	// When the configmap exists
	dir, cleanUp, err := tt.r.createTmpDir(context.Background(), "default", v1alpha1.Dir{
		ConfigMapRef: "my-configmap",
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

func TestReconcile_createTmpDir_notfound(t *testing.T) {
	tt := newMigrationTest(t)
	setDefaultMigrationDir(tt)

	// When the configmap does not exist
	_, _, err := tt.r.createTmpDir(context.Background(), "default", v1alpha1.Dir{
		ConfigMapRef: "other-configmap",
	})
	require.Error(t, err)
	require.Equal(t, " \"other-configmap\" not found", err.Error())
}

func TestReconcile_updateResourceStatus(t *testing.T) {
	tt := newMigrationTest(t)
	am := v1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
	}
	tt.k8s.put(&am)

	// Update status with no error
	err := tt.r.updateResourceStatus(context.Background(), am, nil)
	require.NoError(t, err)
	tt.k8s.Get(context.Background(), migrationReq().NamespacedName, &am)
	require.Equal(t, am.Status.Conditions[0].Status, metav1.ConditionTrue)
	require.Equal(t, am.Status.Conditions[0].Type, "Ready")
	require.Equal(t, am.Status.Conditions[0].Reason, "Applied")
}

func TestReconcile_updateResourceStatus_witherr(t *testing.T) {
	tt := newMigrationTest(t)
	am := v1alpha1.AtlasMigration{
		ObjectMeta: migrationObjmeta(),
	}
	tt.k8s.put(&am)

	// Update status with error
	err := tt.r.updateResourceStatus(context.Background(), am, errors.New("my-error"))
	require.NoError(t, err)
	tt.k8s.Get(context.Background(), types.NamespacedName{
		Name:      am.Name,
		Namespace: am.Namespace,
	}, &am)
	require.Equal(t, am.Status.Conditions[0].Status, metav1.ConditionFalse)
	require.Equal(t, am.Status.Conditions[0].Type, "Ready")
	require.Equal(t, am.Status.Conditions[0].Reason, "Reconciling")
	require.Equal(t, am.Status.Conditions[0].Message, "my-error")
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
			Client: m,
			Scheme: scheme,
			CLI:    nil,
		},
	}
}

func setDefaultMigrationDir(t *migrationTest) {
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

func setDefaultTokenSecrect(t *migrationTest) {
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

type migrationTest struct {
	*testing.T
	k8s   *mockClient
	r     *AtlasMigrationReconciler
	dburl string
}
