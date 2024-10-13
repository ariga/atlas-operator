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

package watch

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllertest"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
)

func TestWatcher(t *testing.T) {
	obj := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod",
			Namespace: "namespace",
		},
	}
	objNsName := types.NamespacedName{Name: obj.Name, Namespace: obj.Namespace}

	mdb1 := dbv1alpha1.AtlasMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mdb1",
			Namespace: "namespace",
		},
	}
	mdb2 := dbv1alpha1.AtlasMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mdb2",
			Namespace: "namespace",
		},
	}
	t.Run("Non-watched object", func(t *testing.T) {
		watcher := New()
		queue := newQueue()

		watcher.Create(context.Background(), event.CreateEvent{
			Object: obj,
		}, queue)

		// Ensure no reconciliation is queued if object is not watched.
		assert.Equal(t, 0, queue.Len())
	})

	t.Run("Multiple objects to reconcile", func(t *testing.T) {
		watcher := New()
		queue := newQueue()
		watcher.Watch(objNsName, mdb1.NamespacedName())
		watcher.Watch(objNsName, mdb2.NamespacedName())

		watcher.Create(context.Background(), event.CreateEvent{
			Object: obj,
		}, queue)

		// Ensure multiple reconciliations are enqueued.
		assert.Equal(t, 2, queue.Len())
	})

	t.Run("Create event", func(t *testing.T) {
		watcher := New()
		queue := newQueue()
		watcher.Watch(objNsName, mdb1.NamespacedName())

		watcher.Create(context.Background(), event.CreateEvent{
			Object: obj,
		}, queue)

		assert.Equal(t, 1, queue.Len())
	})

	t.Run("Update event", func(t *testing.T) {
		watcher := New()
		queue := newQueue()
		watcher.Watch(objNsName, mdb1.NamespacedName())

		watcher.Update(context.Background(), event.UpdateEvent{
			ObjectOld: obj,
			ObjectNew: obj,
		}, queue)

		assert.Equal(t, 1, queue.Len())
	})

	t.Run("Delete event", func(t *testing.T) {
		watcher := New()
		queue := newQueue()
		watcher.Watch(objNsName, mdb1.NamespacedName())

		watcher.Delete(context.Background(), event.DeleteEvent{
			Object: obj,
		}, queue)

		assert.Equal(t, 1, queue.Len())
	})

	t.Run("Generic event", func(t *testing.T) {
		watcher := New()
		queue := newQueue()
		watcher.Watch(objNsName, mdb1.NamespacedName())

		watcher.Generic(context.Background(), event.GenericEvent{
			Object: obj,
		}, queue)

		assert.Equal(t, 1, queue.Len())
	})
}

func TestWatcherAdd(t *testing.T) {
	watcher := New()
	assert.Empty(t, watcher.watched)

	watchedName := types.NamespacedName{Name: "object", Namespace: "namespace"}

	mdb1 := dbv1alpha1.AtlasMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mdb1",
			Namespace: "namespace",
		},
	}
	mdb2 := dbv1alpha1.AtlasMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mdb2",
			Namespace: "namespace",
		},
	}

	// Ensure single object can be added to empty watchlist.
	watcher.Watch(watchedName, mdb1.NamespacedName())
	assert.Len(t, watcher.watched, 1)
	assert.Equal(t, []types.NamespacedName{mdb1.NamespacedName()}, watcher.watched[watchedName])

	// Ensure object can only be watched once.
	watcher.Watch(watchedName, mdb1.NamespacedName())
	assert.Len(t, watcher.watched, 1)
	assert.Equal(t, []types.NamespacedName{mdb1.NamespacedName()}, watcher.watched[watchedName])

	// Ensure a single object can be watched for multiple reconciliations.
	watcher.Watch(watchedName, mdb2.NamespacedName())
	assert.Len(t, watcher.watched, 1)
	assert.Equal(t, []types.NamespacedName{
		mdb1.NamespacedName(),
		mdb2.NamespacedName(),
	}, watcher.watched[watchedName])
}

func newQueue() *controllertest.Queue {
	return &controllertest.Queue{
		TypedInterface: workqueue.NewTyped[reconcile.Request](),
	}
}
