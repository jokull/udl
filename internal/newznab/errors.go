package newznab

import (
	"errors"
	"fmt"
)

// ErrorKind classifies whether an operation can be retried safely.
type ErrorKind string

const (
	Retryable ErrorKind = "retryable"
	Permanent ErrorKind = "permanent"
	Invalid   ErrorKind = "invalid"
)

// Error wraps a Newznab operation failure with retry semantics.
type Error struct {
	Kind ErrorKind
	Op   string
	Err  error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Op == "" {
		return fmt.Sprintf("%s: %v", e.Kind, e.Err)
	}
	return fmt.Sprintf("%s %s: %v", e.Kind, e.Op, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

func wrap(kind ErrorKind, op string, err error) error {
	if err == nil {
		return nil
	}
	var ne *Error
	if errors.As(err, &ne) {
		return err
	}
	return &Error{Kind: kind, Op: op, Err: err}
}

func WrapRetryable(op string, err error) error { return wrap(Retryable, op, err) }
func WrapPermanent(op string, err error) error { return wrap(Permanent, op, err) }
func WrapInvalid(op string, err error) error   { return wrap(Invalid, op, err) }

func IsRetryable(err error) bool {
	var ne *Error
	return errors.As(err, &ne) && ne.Kind == Retryable
}

func IsPermanent(err error) bool {
	var ne *Error
	return errors.As(err, &ne) && ne.Kind == Permanent
}

func IsInvalid(err error) bool {
	var ne *Error
	return errors.As(err, &ne) && ne.Kind == Invalid
}
