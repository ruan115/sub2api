package reconcile

import (
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/outbox"
)

const runtimeEventRetryDelay = 2 * time.Second

func retryRuntimeEvent(now func() time.Time, code string, cause error) error {
	failedAt := time.Now().UTC().Truncate(time.Millisecond)
	if now != nil {
		failedAt = now().UTC().Truncate(time.Millisecond)
	}
	return outbox.RetryableHandlerError(code, failedAt.Add(runtimeEventRetryDelay), cause)
}
