package result

import (
	"errors"
)

// TransientError is an error that should be retried.
type TransientError struct {
	Err   error
	After int
}

// Error implements the error interface
func (t *TransientError) Error() string {
	return t.Err.Error()
}

// Unwrap implements the errors.Wrapper interface
func (t *TransientError) Unwrap() error {
	return t.Err
}

// TransientErrorAfter wraps an error to indicate that it should be retried after
// the given duration.
func TransientErrorAfter(err error, after int) error {
	if err == nil {
		return nil
	}
	return &TransientError{Err: err, After: after}
}

// IsTransient checks if the error is transient
func IsTransient(err error) bool {
	var t *TransientError
	return errors.As(err, &t)
}
