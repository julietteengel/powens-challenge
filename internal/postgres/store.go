package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"powens-challenge/internal/domain"
)

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

const claimQuery = `
SELECT id, event_type, payload, destination_url, status, attempts_count, last_error, next_attempt_at, created_at
FROM jobs
WHERE status = 'pending' AND next_attempt_at <= now()
ORDER BY next_attempt_at
FOR UPDATE SKIP LOCKED
LIMIT 1
`

// WithClaimedJob holds the transaction open for the duration of fn, so the
// row stays invisible to other callers (SKIP LOCKED) until fn's Outcome is
// persisted and committed.
func (s *Store) WithClaimedJob(ctx context.Context, fn func(*domain.Job) domain.Outcome) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	job, err := claimJob(ctx, tx)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	outcome := fn(job)

	if err := applyOutcome(ctx, tx, job.ID, outcome); err != nil {
		return false, err
	}
	// If Commit fails here (e.g. Postgres dies right after a successful
	// delivery), the job stays pending and is retried — an accepted
	// at-least-once duplicate, not a bug.
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func claimJob(ctx context.Context, tx *sql.Tx) (*domain.Job, error) {
	var job domain.Job
	var lastError sql.NullString
	err := tx.QueryRowContext(ctx, claimQuery).Scan(
		&job.ID,
		&job.EventType,
		&job.Payload,
		&job.DestinationURL,
		&job.Status,
		&job.AttemptsCount,
		&lastError,
		&job.NextAttemptAt,
		&job.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	job.LastError = lastError.String
	return &job, nil
}

func applyOutcome(ctx context.Context, tx *sql.Tx, jobID string, outcome domain.Outcome) error {
	switch outcome.Status {
	case domain.StatusDelivered:
		_, err := tx.ExecContext(ctx, `
			UPDATE jobs SET
				attempts_count = attempts_count + 1,
				status = 'delivered',
				last_error = NULL
			WHERE id = $1`,
			jobID)
		return err
	case domain.StatusPending:
		_, err := tx.ExecContext(ctx, `
			UPDATE jobs SET
				attempts_count = attempts_count + 1,
				status = 'pending',
				next_attempt_at = $2,
				last_error = $3
			WHERE id = $1`,
			jobID, outcome.NextAttemptAt, outcome.ErrMsg)
		return err
	case domain.StatusDead:
		_, err := tx.ExecContext(ctx, `
			UPDATE jobs SET
				attempts_count = attempts_count + 1,
				status = 'dead',
				last_error = $2
			WHERE id = $1`,
			jobID, outcome.ErrMsg)
		return err
	default:
		return fmt.Errorf("postgres: unknown outcome status %q", outcome.Status)
	}
}
