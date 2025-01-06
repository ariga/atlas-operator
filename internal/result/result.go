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

package result

import (
	"errors"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

// Transient checks if the error is transient and returns a result
// that indicates whether the request should be retried.
func Transient(err error) (r ctrl.Result, _ error) {
	if t := (*transientError)(nil); errors.As(err, &t) {
		return Retry(t.after)
	}
	// Permanent errors are not returned as errors because they cause
	// the controller to requeue indefinitely. Instead, they should be
	// reported as a status condition.
	return OK()
}

// OK returns a successful result
func OK() (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

// Failed returns a failed result
func Failed() (ctrl.Result, error) {
	return Retry(0)
}

// Retry requeues the request after the specified number of seconds
func Retry(after int) (ctrl.Result, error) {
	return ctrl.Result{
		Requeue:      true,
		RequeueAfter: time.Second * time.Duration(after),
	}, nil
}

// transientError is an error that should be retried.
type transientError struct {
	err   error
	after int
}

func (t *transientError) Error() string { return t.err.Error() }
func (t *transientError) Unwrap() error { return t.err }

// TransientError wraps an error to indicate that it should be retried.
func TransientError(err error) error {
	return TransientErrorAfter(err, 5)
}

// TransientErrorAfter wraps an error to indicate that it should be retried after
// the given duration.
func TransientErrorAfter(err error, after int) error {
	if err == nil {
		return nil
	}
	return &transientError{err: err, after: after}
}

func isTransient(err error) bool {
	var t *transientError
	return errors.As(err, &t)
}
