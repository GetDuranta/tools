package devenv

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound        = errors.New("not found")
	ErrConflict        = errors.New("conflict")
	ErrQuotaExceeded   = errors.New("owner environment quota exceeded")
	ErrIdempotency     = errors.New("idempotency key was reused with different input")
	ErrUnauthorized    = errors.New("unauthorized")
	ErrInvalidArgument = errors.New("invalid argument")
	ErrNotReady        = errors.New("environment is not ready")
)

type FieldError struct {
	Field string
	Err   error
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("%s: %v", e.Field, e.Err)
}

func (e *FieldError) Unwrap() error {
	return e.Err
}
