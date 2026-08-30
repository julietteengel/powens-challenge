package domain

import (
	"errors"
	"testing"
	"time"
)

func TestBackoff_Bounds(t *testing.T) {
	cases := []struct {
		attempt   int
		wantUpper time.Duration
	}{
		{attempt: 0, wantUpper: backoffBase}, // clamped to attempt 1
		{attempt: 1, wantUpper: backoffBase},
		{attempt: 2, wantUpper: 4 * time.Second},
		{attempt: 3, wantUpper: 8 * time.Second},
		{attempt: 10, wantUpper: backoffCap}, // 2^9 * base far exceeds cap
	}

	for _, c := range cases {
		for range 50 {
			d := Backoff(c.attempt)
			if d < 0 || d > c.wantUpper {
				t.Fatalf("Backoff(%d) = %v, want in [0, %v]", c.attempt, d, c.wantUpper)
			}
		}
	}
}

func TestDecideOutcome(t *testing.T) {
	now := time.Now()
	errConn := errors.New("connection refused")

	cases := []struct {
		name        string
		job         *Job
		maxAttempts int
		statusCode  int
		deliverErr  error
		wantStatus  string
		wantErrMsg  string
	}{
		{
			name:        "success",
			job:         &Job{AttemptsCount: 0},
			maxAttempts: 5,
			statusCode:  200,
			wantStatus:  StatusDelivered,
		},
		{
			name:        "network error is retryable",
			job:         &Job{AttemptsCount: 0},
			maxAttempts: 5,
			deliverErr:  errConn,
			wantStatus:  StatusPending,
			wantErrMsg:  "connection refused",
		},
		{
			name:        "429 is retryable",
			job:         &Job{AttemptsCount: 0},
			maxAttempts: 5,
			statusCode:  429,
			wantStatus:  StatusPending,
			wantErrMsg:  "unexpected status code 429",
		},
		{
			name:        "5xx is retryable",
			job:         &Job{AttemptsCount: 0},
			maxAttempts: 5,
			statusCode:  503,
			wantStatus:  StatusPending,
			wantErrMsg:  "unexpected status code 503",
		},
		{
			name:        "other 4xx is terminal",
			job:         &Job{AttemptsCount: 0},
			maxAttempts: 5,
			statusCode:  404,
			wantStatus:  StatusDead,
			wantErrMsg:  "unexpected status code 404",
		},
		{
			name:        "retryable but max attempts reached",
			job:         &Job{AttemptsCount: 4},
			maxAttempts: 5,
			statusCode:  500,
			wantStatus:  StatusDead,
			wantErrMsg:  "unexpected status code 500",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			outcome := DecideOutcome(c.job, c.maxAttempts, c.statusCode, c.deliverErr, now)
			if outcome.Status != c.wantStatus {
				t.Errorf("Status = %q, want %q", outcome.Status, c.wantStatus)
			}
			if outcome.ErrMsg != c.wantErrMsg {
				t.Errorf("ErrMsg = %q, want %q", outcome.ErrMsg, c.wantErrMsg)
			}
			if c.wantStatus == StatusPending && outcome.NextAttemptAt.Before(now) {
				t.Errorf("NextAttemptAt = %v, want at or after %v", outcome.NextAttemptAt, now)
			}
		})
	}
}
