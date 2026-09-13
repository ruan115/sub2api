package outbox

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

var (
	ErrBlocked             = errors.New("runtime outbox checkpoint is blocked")
	ErrRetryBudgetExceeded = errors.New("runtime outbox retry budget is exhausted")
	failureCodePattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,63}$`)
)

const DefaultMaxRetryFailures uint64 = 5

type FailureClass string

const (
	FailureRetryable     FailureClass = "retryable"
	FailureTerminal      FailureClass = "terminal"
	FailureAuthority     FailureClass = "authority"
	FailureIntegrity     FailureClass = "integrity"
	FailureSecurity      FailureClass = "security"
	FailureIndeterminate FailureClass = "indeterminate"
)

type Failure struct {
	Class      FailureClass
	Code       string
	FailedAt   time.Time
	RetryAfter time.Time
	// RetryLimit is evaluated atomically against the durable checkpoint's
	// failure_count. Reaching the limit keeps retry_wait durable and asks the
	// supervisor to exit without advancing; a restart can replay the same event.
	RetryLimit uint64
}

func (f Failure) Validate() error {
	if !validFailureClass(f.Class) || !failureCodePattern.MatchString(f.Code) ||
		f.FailedAt.IsZero() || !f.FailedAt.Equal(f.FailedAt.UTC().Truncate(time.Millisecond)) {
		return errors.New("invalid runtime outbox failure")
	}
	if f.Class == FailureRetryable {
		if f.RetryAfter.IsZero() || !f.RetryAfter.Equal(f.RetryAfter.UTC().Truncate(time.Millisecond)) ||
			!f.RetryAfter.After(f.FailedAt) || f.RetryLimit == 0 || f.RetryLimit > 1000 {
			return errors.New("invalid runtime outbox retry deadline")
		}
	} else if !f.RetryAfter.IsZero() || f.RetryLimit != 0 {
		return errors.New("blocked runtime outbox failure cannot have a retry deadline")
	}
	return nil
}

type BlockedError struct {
	ConsumerName        string
	Sequence            int64
	Class               FailureClass
	Code                string
	BlockedClaimVersion uint64
}

func (e *BlockedError) Error() string {
	if e == nil {
		return ErrBlocked.Error()
	}
	return fmt.Sprintf("%s: consumer=%s sequence=%d class=%s code=%s", ErrBlocked, e.ConsumerName, e.Sequence, e.Class, e.Code)
}

func (e *BlockedError) Unwrap() error { return ErrBlocked }

type RetryBudgetError struct {
	ConsumerName string
	Sequence     int64
	Code         string
	FailureCount uint64
}

func (e *RetryBudgetError) Error() string {
	if e == nil {
		return ErrRetryBudgetExceeded.Error()
	}
	return fmt.Sprintf(
		"%s: consumer=%s sequence=%d code=%s failure_count=%d",
		ErrRetryBudgetExceeded, e.ConsumerName, e.Sequence, e.Code, e.FailureCount,
	)
}

func (e *RetryBudgetError) Unwrap() error { return ErrRetryBudgetExceeded }

type handlerFailure struct {
	failure Failure
	cause   error
}

func (e *handlerFailure) Error() string { return e.cause.Error() }
func (e *handlerFailure) Unwrap() error { return e.cause }

func RetryableHandlerError(code string, retryAfter time.Time, cause error) error {
	return newHandlerFailure(FailureRetryable, code, retryAfter, cause)
}

func BlockingHandlerError(class FailureClass, code string, cause error) error {
	return newHandlerFailure(class, code, time.Time{}, cause)
}

func newHandlerFailure(class FailureClass, code string, retryAfter time.Time, cause error) error {
	if cause == nil || !validFailureClass(class) || (class == FailureRetryable && retryAfter.IsZero()) ||
		(class != FailureRetryable && !retryAfter.IsZero()) || !failureCodePattern.MatchString(code) {
		return errors.New("invalid runtime outbox handler failure")
	}
	return &handlerFailure{failure: Failure{Class: class, Code: code, RetryAfter: retryAfter}, cause: cause}
}

func failureForHandlerError(err error, failedAt time.Time) Failure {
	var classified *handlerFailure
	if errors.As(err, &classified) {
		failure := classified.failure
		failure.FailedAt = failedAt
		if failure.Class == FailureRetryable && failure.RetryLimit == 0 {
			failure.RetryLimit = DefaultMaxRetryFailures
		}
		if !failure.RetryAfter.IsZero() {
			failure.RetryAfter = failure.RetryAfter.UTC().Truncate(time.Millisecond)
		}
		if failure.Validate() == nil {
			return failure
		}
	}
	return Failure{Class: FailureIndeterminate, Code: "handler_unclassified", FailedAt: failedAt}
}

func validFailureClass(class FailureClass) bool {
	switch class {
	case FailureRetryable, FailureTerminal, FailureAuthority, FailureIntegrity, FailureSecurity, FailureIndeterminate:
		return true
	default:
		return false
	}
}
