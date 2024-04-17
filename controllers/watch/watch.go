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
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// ResourceWatcher implements handler.EventHandler and is used to trigger reconciliation when
// a watched object changes. It's designed to only be used for a single type of object.
// If multiple types should be watched, one ResourceWatcher for each type should be used.
type ResourceWatcher struct {
	watched map[types.NamespacedName][]types.NamespacedName
}

// New will create a new ResourceWatcher with no watched objects.
func New() *ResourceWatcher {
	return &ResourceWatcher{
		watched: make(map[types.NamespacedName][]types.NamespacedName),
	}
}

// Watch will add a new object to watch.
func (w ResourceWatcher) Watch(watchedName, dependentName types.NamespacedName) {
	// Check if resource is already being watched.
	existing := w.watched[watchedName]
	if slices.Contains(existing, dependentName) {
		return
	}
	w.watched[watchedName] = append(existing, dependentName)
}

func (w ResourceWatcher) Read(watchedName types.NamespacedName) []types.NamespacedName {
	return w.watched[watchedName]
}

func (w ResourceWatcher) Create(ctx context.Context, event event.CreateEvent, queue workqueue.RateLimitingInterface) {
	w.handleEvent(event.Object, queue)
}

func (w ResourceWatcher) Update(ctx context.Context, event event.UpdateEvent, queue workqueue.RateLimitingInterface) {
	w.handleEvent(event.ObjectOld, queue)
}

func (w ResourceWatcher) Delete(ctx context.Context, event event.DeleteEvent, queue workqueue.RateLimitingInterface) {
	w.handleEvent(event.Object, queue)
}

func (w ResourceWatcher) Generic(ctx context.Context, event event.GenericEvent, queue workqueue.RateLimitingInterface) {
	w.handleEvent(event.Object, queue)
}

// handleEvent is called when an event is received for an object.
// It will check if the object is being watched and trigger a reconciliation for
// the dependent object.
func (w ResourceWatcher) handleEvent(meta metav1.Object, queue workqueue.RateLimitingInterface) {
	changedObjectName := types.NamespacedName{
		Name:      meta.GetName(),
		Namespace: meta.GetNamespace(),
	}
	// Enqueue reconciliation for each dependent object.
	for _, reconciledObjectName := range w.watched[changedObjectName] {
		queue.Add(reconcile.Request{
			NamespacedName: reconciledObjectName,
		})
	}
}
