package controllers

import (
	"context"
	"testing"

	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestReconcile(t *testing.T) {
	scheme := runtime.NewScheme()
	dbv1alpha1.AddToScheme(scheme)
	mock := &MockClient{}
	r := AtlasSchemaReconciler{
		Client: mock,
		Scheme: scheme,
	}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{
			Name:      "my-atlas-schema",
			Namespace: "test",
		},
	})
	require.NoError(t, err)
	require.EqualValues(t, mock.created.Name, "my-atlas-schema-dev-db")
	require.EqualValues(t, mock.created.Spec.Template.Spec.Containers[0].Image, "mysql:8")
}

type MockClient struct {
	client.Client
	created *appsv1.Deployment
}

func (m *MockClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	switch obj.(type) {
	case *dbv1alpha1.AtlasSchema:
		as := &dbv1alpha1.AtlasSchema{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: key.Namespace,
				Name:      key.Name,
			},
			Spec: dbv1alpha1.AtlasSchemaSpec{
				URLFrom: dbv1alpha1.URLFrom{
					SecretKeyRef: &v1.SecretKeySelector{
						LocalObjectReference: v1.LocalObjectReference{Name: "test-secret"},
						Key:                  "url",
					},
				},
			},
		}
		*obj.(*dbv1alpha1.AtlasSchema) = *as
	case *v1.Secret:
		s := &v1.Secret{
			Data: map[string][]byte{
				"url": []byte("mysql://root:pass@/test"),
			},
		}
		*obj.(*v1.Secret) = *s
	default:
		return errors.NewNotFound(schema.GroupResource{
			Group: obj.GetObjectKind().GroupVersionKind().Group,
		}, key.Name)
	}
	return nil
}

func (m *MockClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	switch obj.(type) {
	case *appsv1.Deployment:
		m.created = obj.(*appsv1.Deployment)
	}
	return nil
}
