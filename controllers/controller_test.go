package controllers

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"ariga.io/atlas/sql/sqlcheck"
	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
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
	require.EqualError(t, err, "no desired schema specified")
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
	tt.k8s.put(conditionReconciling())
	tt.k8s.put(devDBReady())
	request := req()
	resp, err := tt.r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.EqualValues(t, ctrl.Result{}, resp)
	cond := tt.cond()
	require.EqualValues(t, schemaReadyCond, cond.Type)
	require.EqualValues(t, metav1.ConditionTrue, cond.Status)
	require.EqualValues(t, "Applied", cond.Reason)
}

func TestFirstRunDestructive(t *testing.T) {
	tt := newTest(t)
	tt.mockCLI().report = &sqlcheck.Report{
		Text: "destructive changes detected",
		Diagnostics: []sqlcheck.Diagnostic{
			{Code: "DS001", Text: "Dropping non-virtual column \"c2\""},
		},
	}
	tt.k8s.put(conditionReconciling())
	tt.k8s.put(devDBReady())
	_, err := tt.r.Reconcile(context.Background(), req())
	require.ErrorContains(t, err, "destructive changes detected")
	// First apply is dry.
	require.Len(t, tt.mockCLI().applyRuns, 1)
	require.True(t, tt.mockCLI().applyRuns[0].DryRun)

	// Condition is not ready and FirstRunDestructive.
	cond := tt.cond()
	require.EqualValues(t, schemaReadyCond, cond.Type)
	require.EqualValues(t, metav1.ConditionFalse, cond.Status)
	require.EqualValues(t, "FirstRunDestructive", cond.Reason)
}

func TestExcludes(t *testing.T) {
	tt := newTest(t)
	sc := conditionReconciling()
	sc.Spec.Exclude = []string{"ignore_me"}
	tt.k8s.put(sc)
	tt.k8s.put(devDBReady())
	_, err := tt.r.Reconcile(context.Background(), req())
	require.NoError(t, err)
	runs := tt.mockCLI().applyRuns
	require.EqualValues(t, []string{"ignore_me"}, runs[0].Exclude)
	require.EqualValues(t, []string{"ignore_me"}, runs[1].Exclude)
}

func TestReconcile_Lint(t *testing.T) {
	tt := newTest(t)
	tt.mockCLI().report = &sqlcheck.Report{
		Text: "destructive changes detected",
		Diagnostics: []sqlcheck.Diagnostic{
			{Code: "DS001", Text: "Dropping non-virtual column \"c2\""},
		},
	}
	sc := conditionReconciling()
	sc.Spec.Policy.Lint.Destructive.Error = true
	sc.Status.LastApplied = 1
	tt.k8s.put(sc)
	tt.k8s.put(devDBReady())
	_, err := tt.r.Reconcile(context.Background(), req())
	require.ErrorContains(t, err, "destructive changes detected")
	cont := tt.cond()
	require.EqualValues(t, schemaReadyCond, cont.Type)
	require.EqualValues(t, metav1.ConditionFalse, cont.Status)
	require.EqualValues(t, "LintPolicyError", cont.Reason)
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
	k8s *mockClient
	r   *AtlasSchemaReconciler
}

func newTest(t *testing.T) *test {
	scheme := runtime.NewScheme()
	dbv1alpha1.AddToScheme(scheme)
	m := &mockClient{
		state: map[client.ObjectKey]client.Object{},
	}
	return &test{
		T:   t,
		k8s: m,
		r: &AtlasSchemaReconciler{
			Client: m,
			Scheme: scheme,
			CLI:    &mockCLI{},
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
	return t.r.CLI.(*mockCLI)
}
