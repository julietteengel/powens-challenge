package domain

import (
	"fmt"
	"time"
)

// Outcome is the fully-decided result of a delivery attempt.
type Outcome struct {
	Status        string
	NextAttemptAt time.Time // only set when Status is StatusPending
	ErrMsg        string
}

// isRetryable: 429 and 5xx are transient, any other 4xx is permanent.
func isRetryable(err error, statusCode int) bool {
	if err != nil {
		return true
	}
	return statusCode == 429 || statusCode >= 500
}

// DecideOutcome is the only place delivery-outcome policy is decided;
// Store persists the result and never recomputes it.
func DecideOutcome(job *Job, maxAttempts, statusCode int, deliverErr error, now time.Time) Outcome {
	if deliverErr == nil && statusCode >= 200 && statusCode < 300 {
		return Outcome{Status: StatusDelivered}
	}

	retryable := isRetryable(deliverErr, statusCode)
	attempts := job.AttemptsCount + 1
	errMsg := formatErr(deliverErr, statusCode)

	if !retryable || attempts >= maxAttempts {
		return Outcome{Status: StatusDead, ErrMsg: errMsg}
	}
	return Outcome{Status: StatusPending, NextAttemptAt: now.Add(Backoff(attempts)), ErrMsg: errMsg}
}

func formatErr(err error, statusCode int) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("unexpected status code %d", statusCode)
}
