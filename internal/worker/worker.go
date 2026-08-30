package worker

import (
	"context"
	"log"
	"time"

	"powens-challenge/internal/domain"
)

type Store interface {
	WithClaimedJob(ctx context.Context, fn func(*domain.Job) domain.Outcome) (claimed bool, err error)
}

type Deliverer interface {
	Deliver(ctx context.Context, job *domain.Job) (statusCode int, err error)
}

const pollInterval = 500 * time.Millisecond

// Run polls for claimable jobs until ctx is done. Claim/delivery operations
// use a separate, uncancelled context so a job already in flight when
// shutdown begins gets to finish rather than being aborted mid-delivery.
func Run(ctx context.Context, store Store, deliverer Deliverer, maxAttempts int) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		claimed, err := store.WithClaimedJob(context.Background(), func(job *domain.Job) domain.Outcome {
			statusCode, err := deliverer.Deliver(context.Background(), job)
			return domain.DecideOutcome(job, maxAttempts, statusCode, err, time.Now())
		})
		if err != nil {
			log.Printf("worker: claim failed: %v", err)
		}
		if err != nil || !claimed {
			select {
			case <-ctx.Done():
				return
			case <-time.After(pollInterval):
			}
		}
	}
}
