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
	"context"
	"testing"

	"ariga.io/atlas-go-sdk/atlasexec"
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
)

var globalAtlasMock = func(dir string) (AtlasExec, error) {
	return atlasexec.NewClient(dir, "atlas")
}

// newRunner returns a runner that can be used to test a reconcile.Reconciler.
func newRunner[T reconcile.Reconciler](fn func(Manager, AtlasExecFn, bool) T, modify func(*fake.ClientBuilder)) (*helper, runner) {
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
	}, globalAtlasMock, true)
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
