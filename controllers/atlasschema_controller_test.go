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
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"ariga.io/atlas-go-sdk/atlasexec"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ariga/atlas-operator/api/v1alpha1"
	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
	"github.com/ariga/atlas-operator/controllers/watch"
)

const (
	devDBSuffix     = "-atlas-dev-db"
	schemaReadyCond = "Ready"
)

func TestReconcile_NotFound(t *testing.T) {
	tt := newTest(t)
	resp, err := tt.r.Reconcile(context.Background(), req())
	require.NoError(t, err)
	require.EqualValues(t, ctrl.Result{}, resp)
}

func TestReconcile_NoCond(t *testing.T) {
	tt := newTest(t)
	tt.k8s.put(&dbv1alpha1.AtlasSchema{
		ObjectMeta: objmeta(),
	})
	request := req()
	resp, err := tt.r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.EqualValues(t, ctrl.Result{Requeue: true}, resp)
	cond := tt.cond()
	require.EqualValues(t, schemaReadyCond, cond.Type)
	require.EqualValues(t, metav1.ConditionFalse, cond.Status)
	require.EqualValues(t, "Reconciling", cond.Message)
}

func TestReconcile_ReadyButDiff(t *testing.T) {
	tt := newTest(t)
	tt.k8s.put(&dbv1alpha1.AtlasSchema{
		ObjectMeta: objmeta(),
		Spec: dbv1alpha1.AtlasSchemaSpec{
			Schema: dbv1alpha1.Schema{SQL: "create table foo (id int primary key);"},
			TargetSpec: v1alpha1.TargetSpec{
				URL: "mysql://root:password@localhost:3306/test",
			},
		},
		Status: dbv1alpha1.AtlasSchemaStatus{
			ObservedHash: "old",
			Conditions: []metav1.Condition{
				{
					Type:   schemaReadyCond,
					Status: metav1.ConditionTrue,
				},
			},
		},
	})
	request := req()
	resp, err := tt.r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.EqualValues(t, ctrl.Result{Requeue: true}, resp)
	cond := tt.cond()
	require.EqualValues(t, schemaReadyCond, cond.Type)
	require.EqualValues(t, metav1.ConditionFalse, cond.Status)
}

func TestReconcile_Reconcile(t *testing.T) {
	meta := objmeta()
	obj := &dbv1alpha1.AtlasSchema{
		ObjectMeta: meta,
		Spec:       dbv1alpha1.AtlasSchemaSpec{},
	}
	h, reconcile := newRunner(NewAtlasSchemaReconciler, func(cb *fake.ClientBuilder) {
		cb.WithStatusSubresource(obj)
		cb.WithObjects(obj)
	}, nil)
	assert := func(except ctrl.Result, ready bool, reason, msg string) {
		t.Helper()
		reconcile(obj, func(result ctrl.Result, err error) {
			require.NoError(t, err)
			require.EqualValues(t, except, result)
			res := &dbv1alpha1.AtlasSchema{ObjectMeta: meta}
			h.get(t, res)
			require.Len(t, res.Status.Conditions, 1)
			require.Equal(t, ready, res.IsReady())
			require.Equal(t, reason, res.Status.Conditions[0].Reason)
			require.Contains(t, res.Status.Conditions[0].Message, msg)
		})
	}
	// First reconcile
	assert(ctrl.Result{Requeue: true}, false, "Reconciling", "Reconciling")
	// Second reconcile, return error for missing database
	assert(ctrl.Result{RequeueAfter: 5 * time.Second}, false, "ReadSchema", "no target database defined")
	// Add Target database and try again
	h.patch(t, &dbv1alpha1.AtlasSchema{
		ObjectMeta: meta,
		Spec: dbv1alpha1.AtlasSchemaSpec{
			TargetSpec: v1alpha1.TargetSpec{URL: "sqlite://file2/?mode=memory"},
		},
	})
	// Third reconcile, return error for missing schema
	assert(ctrl.Result{RequeueAfter: 5000000000}, false, "ReadSchema", "no desired schema specified")
	// Add schema,
	h.patch(t, &dbv1alpha1.AtlasSchema{
		ObjectMeta: meta,
		Spec: dbv1alpha1.AtlasSchemaSpec{
			TargetSpec: v1alpha1.TargetSpec{URL: "sqlite://file2/?mode=memory"},
			Schema:     dbv1alpha1.Schema{SQL: "CREATE TABLE foo(id INT PRIMARY KEY);"},
		},
	})
	// Fourth reconcile, should be success
	assert(ctrl.Result{}, true, "Applied", "The schema has been applied successfully")
	// Update schema for new column
	h.patch(t, &dbv1alpha1.AtlasSchema{
		ObjectMeta: meta,
		Spec: dbv1alpha1.AtlasSchemaSpec{
			TargetSpec: v1alpha1.TargetSpec{URL: "sqlite://file2/?mode=memory"},
			Schema:     dbv1alpha1.Schema{SQL: "CREATE TABLE foo(id INT PRIMARY KEY, c1 INT NULL);"},
		},
	})
	// Fifth reconcile, should be requeue
	assert(ctrl.Result{Requeue: true}, false, "Reconciling", "current schema does not match last applied")
	// Sixth reconcile, should be success
	assert(ctrl.Result{}, true, "Applied", "`c1` int NULL")
	// Check the events generated by the controller
	require.Equal(t, []string{
		"Warning TransientErr no target database defined",
		"Warning TransientErr no desired schema specified",
		"Normal Applied Applied schema",
		"Normal Applied Applied schema",
	}, h.events())
}

func TestExtractData_CustomDevURL(t *testing.T) {
	sc := conditionReconciling()
	sc.Spec.DevURL = "mysql://dev"
	tt := newTest(t)
	data, err := tt.r.extractData(context.Background(), sc)
	require.NoError(t, err)
	require.EqualValues(t, "mysql://dev", data.DevURL)
}

func TestExtractData_CustomDevURL_Secret(t *testing.T) {
	tt := newTest(t)
	sc := conditionReconciling()
	tt.k8s.put(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "devdb",
			Namespace: "test",
		},
		Data: map[string][]byte{
			"url": []byte("mysql://dev"),
		},
	})
	sc.Spec.DevURLFrom = dbv1alpha1.Secret{
		SecretKeyRef: &corev1.SecretKeySelector{
			Key: "url",
			LocalObjectReference: corev1.LocalObjectReference{
				Name: "devdb",
			},
		},
	}
	tt.k8s.put(sc)
	data, err := tt.r.extractData(context.Background(), sc)
	require.NoError(t, err)
	require.EqualValues(t, "mysql://dev", data.DevURL)
}

func TestReconcile_Credentials_BadPassSecret(t *testing.T) {
	tt := newTest(t)
	sc := conditionReconciling()
	sc.Spec.URL = ""
	sc.Spec.Credentials = dbv1alpha1.Credentials{
		Scheme: "mysql",
		User:   "root",
		PasswordFrom: dbv1alpha1.Secret{
			SecretKeyRef: &corev1.SecretKeySelector{
				Key: "password",
				LocalObjectReference: corev1.LocalObjectReference{
					Name: "pass-secret",
				},
			},
		},
		Host:     "localhost",
		Port:     3306,
		Database: "test",
	}
	tt.k8s.put(sc)
	request := req()
	resp, err := tt.r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.EqualValues(t, ctrl.Result{RequeueAfter: time.Second * 5}, resp)
	events := tt.events()
	require.EqualValues(t, `Warning TransientErr "pass-secret" not found`, events[0])
}

func TestSchemaConfigMap(t *testing.T) {
	tt := cliTest(t)
	sc := conditionReconciling()
	// Schema defined in configmap.
	tt.k8s.put(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "schema-configmap",
			Namespace: "test",
		},
		Data: map[string]string{
			"schema.sql": "CREATE TABLE foo (id INT PRIMARY KEY);",
		},
	})
	sc.Spec.Schema = dbv1alpha1.Schema{
		ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{
				Name: "schema-configmap",
			},
			Key: "schema.sql",
		},
	}
	sc.Spec.URL = tt.dburl
	tt.k8s.put(sc)
	ctx := context.Background()

	_, err := tt.r.Reconcile(ctx, req())
	require.NoError(t, err)

	// Assert that the schema was applied.
	cli, err := tt.r.atlasClient("")
	require.NoError(t, err)
	inspect, err := cli.SchemaInspect(ctx, &atlasexec.SchemaInspectParams{
		URL:    tt.dburl,
		DevURL: "sqlite://mem?mode=memory",
		Format: "sql",
	})
	require.NoError(t, err)
	require.Contains(t, inspect, "CREATE TABLE `foo` (`id` int NULL, PRIMARY KEY (`id`));")
}

func TestConfigMapNotFound(t *testing.T) {
	tt := cliTest(t)
	sc := conditionReconciling()
	sc.Spec.Schema = dbv1alpha1.Schema{
		ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{
				Name: "schema-configmap",
			},
			Key: "schema.sql",
		},
	}
	sc.Spec.URL = tt.dburl
	tt.k8s.put(sc)
	ctx := context.Background()

	res, err := tt.r.Reconcile(ctx, req())
	require.NoError(t, err)
	require.EqualValues(t, ctrl.Result{RequeueAfter: time.Second * 5}, res)
	cond := tt.cond()
	require.Contains(t, cond.Message, `"schema-configmap" not found`)
}

func TestExcludes(t *testing.T) {
	tt := cliTest(t)
	sc := conditionReconciling()
	sc.Spec.Exclude = []string{"x"}
	tt.k8s.put(sc)
	tt.initDB("create table x (c int);")
	_, err := tt.r.Reconcile(context.Background(), req())
	require.NoError(t, err)
}

func TestReconcile_Lint(t *testing.T) {
	tt := cliTest(t)
	sc := conditionReconciling()
	sc.Spec.URL = tt.dburl
	sc.Spec.Policy = &dbv1alpha1.Policy{
		Lint: &dbv1alpha1.Lint{
			Destructive: &dbv1alpha1.CheckConfig{Error: true},
		},
	}
	sc.Status.LastApplied = 1
	tt.k8s.put(sc)
	tt.initDB("create table x (c int);")
	_, err := tt.r.Reconcile(context.Background(), req())
	require.NoError(t, err) // this is a non transient error, therefore we don't requeue.
	cont := tt.cond()
	require.EqualValues(t, schemaReadyCond, cont.Type)
	require.EqualValues(t, metav1.ConditionFalse, cont.Status)
	require.EqualValues(t, "LintPolicyError", cont.Reason)
}

func Test_FirstRunDestructive(t *testing.T) {
	tt := cliTest(t)
	sc := conditionReconciling()
	sc.Spec.URL = tt.dburl
	tt.k8s.put(sc)
	tt.initDB("create table x (c int);")
	_, err := tt.r.Reconcile(context.Background(), req())
	require.NoError(t, err) // this is a non transient error, therefore we don't requeue.

	// Condition is not ready and FirstRunDestructive.
	cond := tt.cond()
	require.EqualValues(t, schemaReadyCond, cond.Type)
	require.EqualValues(t, metav1.ConditionFalse, cond.Status)
	require.EqualValues(t, "FirstRunDestructive", cond.Reason)

	events := tt.events()
	require.Len(t, events, 1)
	ev := events[0]
	require.Contains(t, ev, "FirstRunDestructive")
	require.Contains(t, ev, "Warning")

	cli, err := tt.r.atlasClient("")
	require.NoError(t, err)
	ins, err := cli.SchemaInspect(context.Background(), &atlasexec.SchemaInspectParams{
		URL:    tt.dburl,
		Format: "sql",
	})
	require.NoError(t, err)
	require.Contains(t, ins, "CREATE TABLE `x` (`c` int NULL);")
}

func TestBadSQL(t *testing.T) {
	tt := cliTest(t)
	sc := conditionReconciling()
	sc.Spec.Schema.SQL = "bad sql;"
	sc.Spec.URL = tt.dburl
	sc.Spec.Policy = &dbv1alpha1.Policy{
		Lint: &dbv1alpha1.Lint{
			Destructive: &dbv1alpha1.CheckConfig{Error: true},
		},
	}
	sc.Status.LastApplied = 1
	tt.k8s.put(sc)
	resp, err := tt.r.Reconcile(context.Background(), req())
	require.EqualValues(t, ctrl.Result{}, resp)
	require.NoError(t, err) // this is a non transient error, therefore we don't requeue.
	cont := tt.cond()
	require.EqualValues(t, schemaReadyCond, cont.Type)
	require.EqualValues(t, metav1.ConditionFalse, cont.Status)
	require.EqualValues(t, "LintPolicyError", cont.Reason)
	require.Contains(t, cont.Message, "executing statement:")
}

func TestDiffPolicy(t *testing.T) {
	tt := cliTest(t)
	sc := conditionReconciling()
	sc.Spec.URL = tt.dburl
	sc.Spec.Schema.SQL = "create table y (c int);"
	sc.Spec.Policy = &dbv1alpha1.Policy{
		Diff: &dbv1alpha1.Diff{
			Skip: &dbv1alpha1.SkipChanges{
				DropTable: true,
			},
		},
	}
	sc.Status.LastApplied = 1
	tt.k8s.put(sc)
	tt.initDB("create table x (c int);")
	_, err := tt.r.Reconcile(context.Background(), req())
	require.NoError(t, err)
	cli, err := tt.r.atlasClient("")
	require.NoError(t, err)
	ins, err := cli.SchemaInspect(context.Background(), &atlasexec.SchemaInspectParams{
		URL:    tt.dburl,
		Format: "sql",
	})
	require.NoError(t, err)
	require.Contains(t, ins, "CREATE TABLE `x`", "expecting original table to be present")
}

func TestConfigTemplate(t *testing.T) {
	var buf bytes.Buffer
	data := &managedData{
		EnvName: defaultEnvName,
		URL:     must(url.Parse("mysql://root:password@localhost:3306/test")),
		DevURL:  "mysql://root:password@localhost:3306/dev",
		Policy: &dbv1alpha1.Policy{
			Lint: &dbv1alpha1.Lint{
				Destructive: &dbv1alpha1.CheckConfig{Error: true},
			},
			Diff: &dbv1alpha1.Diff{
				ConcurrentIndex: &dbv1alpha1.ConcurrentIndex{
					Create: true,
					Drop:   true,
				},
				Skip: &dbv1alpha1.SkipChanges{
					DropSchema: true,
					DropTable:  true,
				},
			},
		},
		Schemas: []string{"foo", "bar"},
		ext:     "sql",
	}
	err := data.render(&buf)
	require.NoError(t, err)
	expected := `variable "lint_destructive" {
  type = bool
  default = true
}
diff {
  concurrent_index {
    create = true
    drop = true
  }
  skip {
    drop_schema = true
    drop_table = true
  }
}
lint {
  destructive {
    error = var.lint_destructive
  }
}
env {
  name = atlas.env
  src  = "schema.sql"
  url  = "mysql://root:password@localhost:3306/test"
  dev  = "mysql://root:password@localhost:3306/dev"
  schemas = ["foo","bar"]
  exclude = []
}
`
	require.EqualValues(t, expected, buf.String())
}

func TestTemplate_Func_RemoveSpecialChars(t *testing.T) {
	var buf bytes.Buffer
	tmpl, err := tmpl.New("specialChars").Parse(`
	{{- removeSpecialChars .Text -}}
	{{- removeSpecialChars .URL -}}
`)
	require.NoError(t, err)
	var textWithSpecialChars = "a\tb\rc\n"
	err = tmpl.ExecuteTemplate(&buf, `specialChars`, struct {
		Text string
		URL  *url.URL
	}{
		Text: textWithSpecialChars,
		URL:  &url.URL{},
	})
	require.NoError(t, err)
	require.EqualValues(t, "abc", buf.String())
	// invalid data type
	err = tmpl.ExecuteTemplate(&buf, `specialChars`, struct {
		Text int
	}{
		Text: 0,
	})
	require.ErrorContains(t, err, "unsupported type int")
}

func conditionReconciling() *dbv1alpha1.AtlasSchema {
	return &dbv1alpha1.AtlasSchema{
		ObjectMeta: objmeta(),
		Spec: dbv1alpha1.AtlasSchemaSpec{
			TargetSpec: v1alpha1.TargetSpec{
				URL: "sqlite://file?mode=memory",
			},
			Schema: dbv1alpha1.Schema{SQL: "CREATE TABLE foo (id INT PRIMARY KEY);"},
		},
		Status: dbv1alpha1.AtlasSchemaStatus{
			Conditions: []metav1.Condition{
				{
					Type:   schemaReadyCond,
					Status: metav1.ConditionFalse,
				},
			},
		},
	}
}

func devDBReady() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-atlas-schema" + devDBSuffix,
			Namespace: "test",
		},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas: 1,
		},
	}
}

func objmeta() metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      "my-atlas-schema",
		Namespace: "test",
	}
}

func req() ctrl.Request {
	return ctrl.Request{
		NamespacedName: client.ObjectKey{
			Name:      "my-atlas-schema",
			Namespace: "test",
		},
	}
}

type test struct {
	*testing.T
	k8s   *mockClient
	r     *AtlasSchemaReconciler
	dburl string
}

// cliTest initializes a test with a real CLI and a temporary SQLite database.
func cliTest(t *testing.T) *test {
	tt := newTest(t)
	var err error
	tt.r.atlasClient = globalAtlasMock
	require.NoError(t, err)
	td, err := os.MkdirTemp("", "operator-test-sqlite-*")
	require.NoError(t, err)
	tt.dburl = "sqlite://" + filepath.Join(td, "test.db")
	t.Cleanup(func() { os.RemoveAll(td) })
	return tt
}

func newTest(t *testing.T) *test {
	scheme := runtime.NewScheme()
	dbv1alpha1.AddToScheme(scheme)
	m := &mockClient{
		state: map[client.ObjectKey]client.Object{},
	}
	r := record.NewFakeRecorder(100)
	return &test{
		T:   t,
		k8s: m,
		r: &AtlasSchemaReconciler{
			Client:           m,
			scheme:           scheme,
			atlasClient:      globalAtlasMock,
			configMapWatcher: watch.New(),
			secretWatcher:    watch.New(),
			recorder:         r,
			devDB: &devDBReconciler{
				Client:   m,
				scheme:   scheme,
				recorder: r,
			},
		},
	}
}

type (
	mockClient struct {
		client.Client
		state map[client.ObjectKey]client.Object
	}
	mockSubResourceWriter struct {
		client.SubResourceWriter
		ref *mockClient
	}
)

func (m *mockClient) put(obj client.Object) {
	m.state[client.ObjectKeyFromObject(obj)] = obj
}

func (m *mockClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	// retrieve the object from the state map
	o, ok := m.state[key]
	if !ok {
		return kerr.NewNotFound(schema.GroupResource{
			Group: obj.GetObjectKind().GroupVersionKind().Group,
		}, key.Name)
	}
	// if o and obj are the same type, just copy o into obj
	if reflect.TypeOf(o) == reflect.TypeOf(obj) {
		reflect.ValueOf(obj).Elem().Set(reflect.ValueOf(o).Elem())
		return nil
	}
	return nil
}

func (m *mockClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	delete(m.state, client.ObjectKeyFromObject(obj))
	return nil
}

// Hardcoded list of pods to simulate a running dev db.
func (m *mockClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if reflect.TypeOf(list) != reflect.TypeOf(&corev1.PodList{}) {
		return fmt.Errorf("unsupported list type: %T", list)
	}
	podList := list.(*corev1.PodList)
	podList.Items = []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-atlas-schema" + devDBSuffix,
				Namespace: "test",
				Annotations: map[string]string{
					annoConnTmpl: "sqlite://file?mode=memory",
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "mysql",
						Ports: []corev1.ContainerPort{
							{ContainerPort: 3306},
						},
					},
				},
			},
			Status: corev1.PodStatus{
				PodIP: "1.2.3.4",
				Phase: corev1.PodRunning,
			},
		},
	}
	return nil
}

func (m *mockClient) Status() client.StatusWriter {
	return &mockSubResourceWriter{
		ref: m,
	}
}

func (s *mockSubResourceWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	s.ref.put(obj)
	return nil
}

func TestMock(t *testing.T) {
	tt := newTest(t)
	tt.k8s.put(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
		},
	})
	var d appsv1.Deployment
	// Req a non existent object
	err := tt.k8s.Get(context.Background(), client.ObjectKey{
		Name: "non-existent",
	}, &d)
	require.True(t, kerr.IsNotFound(err))
	// Retrieve an existing object
	err = tt.k8s.Get(context.Background(), client.ObjectKey{
		Name:      "test",
		Namespace: "default",
	}, &d)
	require.NoError(t, err)
	require.EqualValues(t, d.Spec.Replicas, ptr.To[int32](1))
}

func (m *mockClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	m.put(obj)
	return nil
}

func TestTemplateSanity(t *testing.T) {
	var b bytes.Buffer
	v := &devDB{
		NamespacedName: types.NamespacedName{
			Name:      "test",
			Namespace: "default",
		},
	}
	for _, tt := range []string{"mysql", "postgres", "sqlserver"} {
		t.Run(tt, func(t *testing.T) {
			v.Driver = tt
			err := tmpl.ExecuteTemplate(&b, "devdb.tmpl", v)
			require.NoError(t, err)
			var d appsv1.Deployment
			err = yaml.NewYAMLToJSONDecoder(&b).Decode(&d)
			require.NoError(t, err)
			b.Reset()
		})
	}
}

func (t *test) cond() metav1.Condition {
	s := t.k8s.state[req().NamespacedName].(*dbv1alpha1.AtlasSchema)
	return s.Status.Conditions[0]
}

func (t *test) initDB(statement string) {
	wd, err := atlasexec.NewWorkingDir()
	require.NoError(t, err)
	defer wd.Close()
	_, err = wd.WriteFile("schema.sql", []byte(statement))
	require.NoError(t, err)
	cli, err := atlasexec.NewClient(wd.Path(), "atlas")
	require.NoError(t, err)
	_, err = cli.SchemaApply(context.Background(), &atlasexec.SchemaApplyParams{
		URL:    t.dburl,
		DevURL: "sqlite://file2/?mode=memory",
		To:     "file://./schema.sql",
	})
	require.NoError(t, err)
}

func (t *test) events() []string {
	return events(t.r.recorder)
}

func events(r record.EventRecorder) []string {
	// read events from channel
	var ev []string
	for {
		select {
		case e := <-r.(*record.FakeRecorder).Events:
			ev = append(ev, e)
		default:
			return ev
		}
	}
}

// Versions after v0.17 of Atlas return a slightly more readable error message. This test
// ensures we support both formats.
func TestSQLErrRegression(t *testing.T) {
	m := `executing statement "create table bar (id int)"`
	require.True(t, isSQLErr(fmt.Errorf(`sql/migrate: execute: %s`, m)))
	require.True(t, isSQLErr(fmt.Errorf(`sql/migrate: %s`, m)))
	require.True(t, isSQLErr(errors.New(`Error: read state from "schema.sql": executing statement: "bad sql;": near "bad": syntax error`)))
}

func Test_truncateSQL(t *testing.T) {
	// The first line is over the limit but no newline is added.
	require.Equal(t, []string{
		"-- truncated 37 bytes...",
	}, truncateSQL([]string{
		"CREATE TABLE FOO(id INT PRIMARY KEY);",
	}, 10))

	require.Equal(t, []string{
		"CREATE TABLE FOO(id INT PRIMARY KEY);\n-- truncated 37 bytes...",
	}, truncateSQL([]string{
		"CREATE TABLE FOO(id INT PRIMARY KEY);\nCREATE TABLE BAR(id INT PRIMARY KEY);",
	}, 37))
	require.Equal(t, []string{
		"-- truncated 108 bytes...",
	}, truncateSQL([]string{
		"CREATE TABLE FOO(id INT PRIMARY KEY); --the first statement is so long\nCREATE TABLE BAR(id INT PRIMARY KEY);",
	}, 37))

	require.Equal(t, []string{
		"CREATE TABLE FOO(id INT PRIMARY KEY);",
		"-- truncated 37 bytes...",
	}, truncateSQL([]string{
		"CREATE TABLE FOO(id INT PRIMARY KEY);",
		"CREATE TABLE BAR(id INT PRIMARY KEY);",
	}, 37))
}
