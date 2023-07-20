package controllers

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"ariga.io/atlas/sql/sqlcheck"
	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
	"github.com/ariga/atlas-operator/controllers/watch"
	"github.com/ariga/atlas-operator/internal/atlas"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
			URL:    "mysql://root:password@localhost:3306/test",
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

func TestReconcile_NoSchema(t *testing.T) {
	tt := newTest(t)
	tt.k8s.put(&dbv1alpha1.AtlasSchema{
		ObjectMeta: objmeta(),
		Status: dbv1alpha1.AtlasSchemaStatus{
			Conditions: []metav1.Condition{
				{
					Type:   schemaReadyCond,
					Status: metav1.ConditionFalse,
				},
			},
		},
	})
	request := req()
	_, err := tt.r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	cond := tt.cond()
	require.EqualValues(t, schemaReadyCond, cond.Type)
	require.EqualValues(t, metav1.ConditionFalse, cond.Status)
	require.EqualValues(t, "ReadSchema", cond.Reason)
	require.EqualValues(t, "no desired schema specified", cond.Message)
}

func TestReconcile_HasSchema(t *testing.T) {
	tt := newTest(t)
	tt.k8s.put(conditionReconciling())
	request := req()
	resp, err := tt.r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.EqualValues(t, ctrl.Result{RequeueAfter: time.Second * 15}, resp)
	d := tt.k8s.state[types.NamespacedName{
		Namespace: request.Namespace,
		Name:      request.Name + devDBSuffix,
	}].(*appsv1.Deployment)
	require.EqualValues(t, "mysql:8", d.Spec.Template.Spec.Containers[0].Image)
}

func TestReconcile_HasSchemaAndDB(t *testing.T) {
	tt := newTest(t)
	sc := conditionReconciling()
	sc.Spec.Schemas = []string{"a", "b"}
	tt.k8s.put(sc)
	tt.k8s.put(devDBReady())
	request := req()
	resp, err := tt.r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.EqualValues(t, ctrl.Result{}, resp)
	cond := tt.cond()
	require.EqualValues(t, schemaReadyCond, cond.Type)
	require.EqualValues(t, metav1.ConditionTrue, cond.Status)
	require.EqualValues(t, "Applied", cond.Reason)
	require.EqualValues(t, []string{"a", "b"}, tt.mockCLI().applyRuns[0].Schema)

	events := tt.events()
	require.EqualValues(t, "Normal Applied Applied schema", events[0])
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
	inspect, err := tt.r.cli.SchemaInspect(ctx, &atlas.SchemaInspectParams{
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

func TestReconcile_Lint_With_DisabledDestructive(t *testing.T) {
	tt := cliTest(t)
	sc := conditionReconciling()
	sc.Spec.URL = tt.dburl
	destErr := false
	sc.Spec.Policy.Lint.Destructive.Error = &destErr
	sc.Status.LastApplied = 1
	tt.k8s.put(sc)
	tt.initDB("create table x (c int);")
	_, err := tt.r.Reconcile(context.Background(), req())
	require.NoError(t, err) // this is a non transient error, therefore we don't requeue.
	cont := tt.cond()
	require.EqualValues(t, schemaReadyCond, cont.Type)
	require.EqualValues(t, metav1.ConditionTrue, cont.Status)
	require.EqualValues(t, "Applied", cont.Reason)
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

	ins, err := tt.r.cli.SchemaInspect(context.Background(), &atlas.SchemaInspectParams{
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
	sc.Status.LastApplied = 1
	tt.k8s.put(sc)
	resp, err := tt.r.Reconcile(context.Background(), req())
	require.EqualValues(t, ctrl.Result{}, resp)
	require.NoError(t, err) // this is a non transient error, therefore we don't requeue.
	cont := tt.cond()
	require.EqualValues(t, schemaReadyCond, cont.Type)
	require.EqualValues(t, metav1.ConditionFalse, cont.Status)
	require.EqualValues(t, "LintPolicyError", cont.Reason)
	require.Contains(t, cont.Message, "sql/migrate: execute: executing statement")
}

func TestDiffPolicy(t *testing.T) {
	tt := cliTest(t)
	sc := conditionReconciling()
	sc.Spec.URL = tt.dburl
	sc.Spec.Schema.SQL = "create table y (c int);"
	sc.Spec.Policy.Diff.Skip = dbv1alpha1.SkipChanges{
		DropTable: true,
	}
	sc.Status.LastApplied = 1
	tt.k8s.put(sc)
	tt.initDB("create table x (c int);")
	_, err := tt.r.Reconcile(context.Background(), req())
	require.NoError(t, err)
	ins, err := tt.r.cli.SchemaInspect(context.Background(), &atlas.SchemaInspectParams{
		URL:    tt.dburl,
		Format: "sql",
	})
	require.NoError(t, err)
	require.Contains(t, ins, "CREATE TABLE `x`", "expecting original table to be present")
}

func TestConfigTemplate(t *testing.T) {
	var buf bytes.Buffer
	destErr := true
	err := tmpl.ExecuteTemplate(&buf, "conf.tmpl", dbv1alpha1.Policy{
		Lint: dbv1alpha1.Lint{
			Destructive: dbv1alpha1.CheckConfig{Error: &destErr},
		},
		Diff: dbv1alpha1.Diff{
			Skip: dbv1alpha1.SkipChanges{
				DropSchema: true,
				DropTable:  true,
			},
		},
	})
	require.NoError(t, err)
	expected := `env {
  name = atlas.env
}

variable "lint_destructive" {
    type = bool
    default = true
}
diff {
  skip {
      drop_schema = true
      drop_table = true
  }
}
lint {
  destructive {
    error = var.lint_destructive
  }
}`
	require.EqualValues(t, expected, buf.String())
}

func conditionReconciling() *dbv1alpha1.AtlasSchema {
	return &dbv1alpha1.AtlasSchema{
		ObjectMeta: objmeta(),
		Spec: dbv1alpha1.AtlasSchemaSpec{
			URL:    "mysql://root:password@localhost:3306/test",
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
	wd, err := os.Getwd()
	require.NoError(t, err)
	cli, err := atlas.NewClient(wd, "atlas")
	require.NoError(t, err)
	tt.r.cli = cli
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
	configMapWatcher := watch.New()
	secretWatcher := watch.New()
	return &test{
		T:   t,
		k8s: m,
		r: &AtlasSchemaReconciler{
			Client:           m,
			scheme:           scheme,
			cli:              &mockCLI{},
			configMapWatcher: &configMapWatcher,
			secretWatcher:    &secretWatcher,
			recorder:         record.NewFakeRecorder(100),
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
	mockCLI struct {
		CLI
		inspect   string
		plan      string
		report    *sqlcheck.Report
		applyRuns []*atlas.SchemaApplyParams
	}
)

func (m *mockClient) put(obj client.Object) {
	m.state[client.ObjectKeyFromObject(obj)] = obj
}

func (m *mockClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	// retrieve the object from the state map
	o, ok := m.state[key]
	if !ok {
		return errors.NewNotFound(schema.GroupResource{
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
					"atlasgo.io/conntmpl": "mysql://root:password@" + hostReplace + ":3306/test",
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
			Replicas: pointer.Int32(1),
		},
	})
	var d appsv1.Deployment
	// Req a non existent object
	err := tt.k8s.Get(context.Background(), client.ObjectKey{
		Name: "non-existent",
	}, &d)
	require.True(t, errors.IsNotFound(err))
	// Retrieve an existing object
	err = tt.k8s.Get(context.Background(), client.ObjectKey{
		Name:      "test",
		Namespace: "default",
	}, &d)
	require.NoError(t, err)
	require.EqualValues(t, d.Spec.Replicas, pointer.Int32(1))
}

func (m *mockClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	m.put(obj)
	return nil
}

func TestTemplateSanity(t *testing.T) {
	var b bytes.Buffer
	v := &devDB{
		Name:      "test",
		Namespace: "default",
	}
	for _, tt := range []string{"mysql", "postgres"} {
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

func (c *mockCLI) SchemaApply(_ context.Context, params *atlas.SchemaApplyParams) (*atlas.SchemaApply, error) {
	c.applyRuns = append(c.applyRuns, params)
	return &atlas.SchemaApply{
		Changes: atlas.Changes{
			Pending: []string{c.plan},
		},
	}, nil
}

func (c *mockCLI) SchemaInspect(context.Context, *atlas.SchemaInspectParams) (string, error) {
	return c.inspect, nil
}

func (c *mockCLI) Lint(ctx context.Context, _ *atlas.LintParams) (*atlas.SummaryReport, error) {
	rep := &atlas.SummaryReport{
		Files: []*atlas.FileReport{
			{Name: "1.sql"},
		},
	}
	if c.report != nil {
		rep.Files[0].Error = "err"
		rep.Files[0].Reports = append(rep.Files[0].Reports, *c.report)
	}
	return rep, nil
}

func (t *test) cond() metav1.Condition {
	s := t.k8s.state[req().NamespacedName].(*dbv1alpha1.AtlasSchema)
	return s.Status.Conditions[0]
}

func (t *test) mockCLI() *mockCLI {
	return t.r.cli.(*mockCLI)
}

func (t *test) initDB(statement string) {
	f, clean, err := atlas.TempFile(statement, "sql")
	require.NoError(t, err)
	defer clean()
	_, err = t.r.cli.SchemaApply(context.Background(), &atlas.SchemaApplyParams{
		URL:    t.dburl,
		DevURL: "sqlite://file2/?mode=memory",
		To:     f,
	})
	require.NoError(t, err)
}

func (t *test) events() []string {
	r := t.r.recorder.(*record.FakeRecorder)
	// read events from channel
	var events []string
	for {
		select {
		case e := <-r.Events:
			events = append(events, e)
		default:
			return events
		}
	}
}
