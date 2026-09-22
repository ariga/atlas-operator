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

package controller

import (
	"context"
	"io"
	"testing"

	"ariga.io/atlas/atlasexec"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
)

type (
	check  func(ctrl.Result, error)
	runner func(client.Object, check)
	helper struct {
		client   client.Client
		recorder *record.FakeRecorder
	}
	mockManager struct {
		client   client.Client
		recorder *record.FakeRecorder
		scheme   *runtime.Scheme
	}
	// mockCmd mocks a command returning a single result.
	mockCmd[T any] struct {
		res *T
		err error
	}
	// mockSliceCmd mocks a command that reports one result per target. It
	// records the params it was called with.
	mockSliceCmd[P, T any] struct {
		res    []*T
		err    error
		params *P
	}
	mockAtlasExec struct {
		// stderr is written to the writer set by SetStderr when a command runs,
		// mimicking the Atlas CLI logging to stderr.
		stderr         string
		stderrW        io.Writer
		apply          mockSliceCmd[atlasexec.MigrateApplyParams, atlasexec.MigrateApply]
		down           mockCmd[atlasexec.MigrateDown]
		drift          mockSliceCmd[atlasexec.MigrateDriftParams, atlasexec.MigrateDrift]
		lint           mockCmd[atlasexec.SummaryReport]
		status         mockCmd[atlasexec.MigrateStatus]
		schemaApply    mockSliceCmd[atlasexec.SchemaApplyParams, atlasexec.SchemaApply]
		whoami         mockCmd[atlasexec.WhoAmI]
		schemaPush     mockCmd[atlasexec.SchemaPush]
		schemaPlanList mockCmd[[]atlasexec.SchemaPlanFile]
		schemaPlan     mockCmd[atlasexec.SchemaPlan]
		schemaInspect  mockCmd[string]
	}
)

var _ AtlasExec = &mockAtlasExec{}

// val returns the mocked result, or its zero value if the test set none.
func (c *mockCmd[T]) val() (T, error) {
	var v T
	if c.res != nil {
		v = *c.res
	}
	return v, c.err
}

// call records the params of the call and returns the mocked results.
func (c *mockSliceCmd[P, T]) call(params *P) ([]*T, error) {
	c.params = params
	if c.err != nil {
		return nil, c.err
	}
	return c.res, nil
}

// SchemaPlan implements AtlasExec.
func (m *mockAtlasExec) SchemaPlan(context.Context, *atlasexec.SchemaPlanParams) (*atlasexec.SchemaPlan, error) {
	return m.schemaPlan.res, m.schemaPlan.err
}

// SchemaPlanList implements AtlasExec.
func (m *mockAtlasExec) SchemaPlanList(context.Context, *atlasexec.SchemaPlanListParams) ([]atlasexec.SchemaPlanFile, error) {
	return m.schemaPlanList.val()
}

// SchemaPush implements AtlasExec.
func (m *mockAtlasExec) SchemaPush(context.Context, *atlasexec.SchemaPushParams) (*atlasexec.SchemaPush, error) {
	return m.schemaPush.res, m.schemaPush.err
}

func (m *mockAtlasExec) WhoAmI(context.Context, *atlasexec.WhoAmIParams) (*atlasexec.WhoAmI, error) {
	return m.whoami.res, m.whoami.err
}

// SchemaAppleSlice implements AtlasExec.
func (m *mockAtlasExec) SchemaApplySlice(_ context.Context, params *atlasexec.SchemaApplyParams) ([]*atlasexec.SchemaApply, error) {
	m.writeStderr()
	return m.schemaApply.call(params)
}

// writeStderr streams the mocked stderr output, if any, to the writer
// set by SetStderr.
func (m *mockAtlasExec) writeStderr() {
	if m.stderr == "" || m.stderrW == nil {
		return
	}
	io.WriteString(m.stderrW, m.stderr)
}

// SchemaInspect implements AtlasExec.
func (m *mockAtlasExec) SchemaInspect(context.Context, *atlasexec.SchemaInspectParams) (string, error) {
	return m.schemaInspect.val()
}

// MigrateApplySlice implements AtlasExec.
func (m *mockAtlasExec) MigrateApplySlice(_ context.Context, params *atlasexec.MigrateApplyParams) ([]*atlasexec.MigrateApply, error) {
	m.writeStderr()
	return m.apply.call(params)
}

// MigrateDown implements AtlasExec.
func (m *mockAtlasExec) MigrateDown(context.Context, *atlasexec.MigrateDownParams) (*atlasexec.MigrateDown, error) {
	return m.down.res, m.down.err
}

// MigrateDriftSlice implements AtlasExec.
func (m *mockAtlasExec) MigrateDriftSlice(_ context.Context, params *atlasexec.MigrateDriftParams) ([]*atlasexec.MigrateDrift, error) {
	m.writeStderr()
	return m.drift.call(params)
}

// MigrateStatus implements AtlasExec.
func (m *mockAtlasExec) MigrateStatus(context.Context, *atlasexec.MigrateStatusParams) (*atlasexec.MigrateStatus, error) {
	return m.status.res, m.status.err
}

func (m *mockAtlasExec) Login(context.Context, *atlasexec.LoginParams) error { return nil }
func (m *mockAtlasExec) SetStdout(io.Writer)                                 {}
func (m *mockAtlasExec) SetStderr(w io.Writer)                               { m.stderrW = w }

// newRunner returns a runner that can be used to test a reconcile.Reconciler.
func newRunner[T interface {
	Reconcile(context.Context, reconcile.Request) (reconcile.Result, error)
	SetAtlasClient(AtlasExecFn)
}](fn func(Manager, bool) T, modify func(*fake.ClientBuilder), mock *mockAtlasExec) (*helper, runner) {
	scheme := runtime.NewScheme()
	clientgoscheme.AddToScheme(scheme)
	dbv1alpha1.AddToScheme(scheme)
	b := fake.NewClientBuilder().WithScheme(scheme)
	if modify != nil {
		modify(b)
	}
	c := b.Build()
	r := record.NewFakeRecorder(100)
	a := fn(&mockManager{
		client:   c,
		recorder: r,
		scheme:   scheme,
	}, true)
	a.SetAtlasClient(func(s string, c *Cloud, home string) (AtlasExec, error) {
		if mock == nil {
			return NewAtlasExec(s, c, home)
		}
		return mock, nil
	})
	h := &helper{client: c, recorder: r}
	return h, func(obj client.Object, fn check) {
		fn(a.Reconcile(context.Background(), request(obj)))
	}
}

// request returns a reconcile.Request with
// the namespace and name of the given object.
func request(obj client.Object) reconcile.Request {
	return reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(obj),
	}
}

// GetClient implements Manager.
func (m *mockManager) GetClient() client.Client {
	return m.client
}

// GetEventRecorderFor implements Manager.
func (m *mockManager) GetEventRecorderFor(name string) record.EventRecorder {
	return m.recorder
}

// GetScheme implements Manager.
func (m *mockManager) GetScheme() *runtime.Scheme {
	return m.scheme
}

func (r *helper) get(t *testing.T, o client.Object) {
	t.Helper()
	require.NoError(t, r.client.Get(context.Background(), client.ObjectKeyFromObject(o), o))
}

func (r *helper) patch(t *testing.T, o client.Object) {
	t.Helper()
	require.NoError(t, r.client.Patch(context.Background(), o, client.MergeFrom(nil)))
}

func (r *helper) events() []string {
	var ev []string
	for {
		select {
		case e := <-r.recorder.Events:
			ev = append(ev, e)
		default:
			return ev
		}
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
