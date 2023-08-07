package controllers

import (
	"context"
	"errors"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// getSecretValue gets the value of the given secret key selector.
func getSecretValue(
	ctx context.Context,
	r client.Reader,
	ns string,
	selector corev1.SecretKeySelector,
) (string, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: selector.Name}, secret); err != nil {
		return "", transient(err)
	}
	us := string(secret.Data[selector.Key])
	return us, nil
}

// isSQLErr returns true if the error is a SQL error.
func isSQLErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "sql/migrate: execute: executing statement")
}

// transientError is an error that should be retried.
type transientError struct {
	err   error
	after time.Duration
}

func (t *transientError) Error() string { return t.err.Error() }
func (t *transientError) Unwrap() error { return t.err }

// transient wraps an error to indicate that it should be retried.
func transient(err error) error {
	return transientAfter(err, 5*time.Second)
}

// transientAfter wraps an error to indicate that it should be retried after
// the given duration.
func transientAfter(err error, after time.Duration) error {
	if err == nil {
		return nil
	}
	return &transientError{err: err, after: after}
}

func isTransient(err error) bool {
	var t *transientError
	return errors.As(err, &t)
}

// result returns a ctrl.Result and an error. If the error is transient, the
// task will be requeued after seconds defined by the error.
// Permanent errors are not returned as errors because they cause
// the controller to requeue indefinitely. Instead, they should be
// reported as a status condition.
func result(err error) (r ctrl.Result, _ error) {
	if t := (*transientError)(nil); errors.As(err, &t) {
		r.RequeueAfter = t.after
	}
	return r, nil
}
