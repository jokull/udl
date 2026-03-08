package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/jokull/udl/internal/newznab"
)

// RetryClass is the daemon-level failure class used for scheduling/retry behavior.
type RetryClass string

const (
	RetryClassRetryable RetryClass = "retryable"
	RetryClassPermanent RetryClass = "permanent"
	RetryClassInvalid   RetryClass = "invalid"
)

// RetryError wraps an operation failure with retry class metadata.
type RetryError struct {
	Class RetryClass
	Op    string
	Err   error
}

func (e *RetryError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Op == "" {
		return fmt.Sprintf("%s: %v", e.Class, e.Err)
	}
	return fmt.Sprintf("%s %s: %v", e.Class, e.Op, e.Err)
}

func (e *RetryError) Unwrap() error { return e.Err }

func wrapRetryClass(class RetryClass, op string, err error) error {
	if err == nil {
		return nil
	}
	var re *RetryError
	if errors.As(err, &re) {
		return err
	}
	return &RetryError{Class: class, Op: op, Err: err}
}

func wrapRetryable(op string, err error) error { return wrapRetryClass(RetryClassRetryable, op, err) }
func wrapPermanent(op string, err error) error { return wrapRetryClass(RetryClassPermanent, op, err) }
func wrapInvalid(op string, err error) error   { return wrapRetryClass(RetryClassInvalid, op, err) }

func isRetryable(err error) bool {
	var re *RetryError
	if errors.As(err, &re) && re.Class == RetryClassRetryable {
		return true
	}
	if newznab.IsRetryable(err) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func isInvalid(err error) bool {
	var re *RetryError
	if errors.As(err, &re) && re.Class == RetryClassInvalid {
		return true
	}
	return newznab.IsInvalid(err)
}

func isPermanent(err error) bool {
	var re *RetryError
	if errors.As(err, &re) && re.Class == RetryClassPermanent {
		return true
	}
	return newznab.IsPermanent(err)
}

func errorClass(err error) RetryClass {
	switch {
	case isInvalid(err):
		return RetryClassInvalid
	case isPermanent(err):
		return RetryClassPermanent
	case isRetryable(err):
		return RetryClassRetryable
	default:
		return RetryClassPermanent
	}
}

func formatClassifiedError(prefix string, err error) string {
	class := string(errorClass(err))
	if prefix == "" {
		return fmt.Sprintf("[%s] %v", class, err)
	}
	return fmt.Sprintf("%s [%s]: %v", prefix, class, err)
}

func isSearchBusyError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "search busy")
}
