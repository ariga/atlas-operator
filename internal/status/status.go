// Copyright 2025 The Atlas Operator Authors.
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

package status

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type (
	// OptionBuilder is an interface that can be implemented
	// by any type that can provide a list of options
	OptionBuilder[T any] interface {
		GetOptions() []Option[T]
	}
	// Option is an interface that can be implemented by any type
	// that can apply an option to a resource and return a result
	Option[T any] interface {
		ApplyOption(o T)
		GetResult() (ctrl.Result, error)
	}
)

// Update takes the options provided by the given option builder, applies them all and then updates the resource
func Update[T client.Object](ctx context.Context, sw client.StatusWriter, obj T, b OptionBuilder[T]) (r ctrl.Result, err error) {
	opts := b.GetOptions()
	for _, o := range opts {
		o.ApplyOption(obj)
	}
	if err := sw.Update(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}
	for _, o := range opts {
		if r, err = o.GetResult(); err != nil {
			return r, err
		}
	}
	for _, o := range opts {
		if r, _ := o.GetResult(); r.Requeue || r.RequeueAfter > 0 {
			return r, nil
		}
	}
	return ctrl.Result{}, nil
}
