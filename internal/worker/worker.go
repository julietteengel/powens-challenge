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

// dbMargin is added on top of the delivery timeout to bound the claim/commit
// SQL round trips surrounding the HTTP call, not just the call itself.
const dbMargin = 5 * time.Second

// Run polls for claimable jobs until ctx is done. Each claim/delivery cycle
// runs on a context derived from ctx via WithoutCancel + its own timeout: it
// survives a SIGTERM (so an in-flight delivery isn't aborted the instant
// shutdown begins) but is still bounded, so a stuck BeginTx/Commit can't
// hang forever or block the shutdown grace period indefinitely.
func Run(ctx context.Context, store Store, deliverer Deliverer, maxAttempts int, deliveryTimeout time.Duration) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliveryTimeout+dbMargin)
		claimed, err := store.WithClaimedJob(opCtx, func(job *domain.Job) domain.Outcome {
			statusCode, err := deliverer.Deliver(opCtx, job)
			return domain.DecideOutcome(job, maxAttempts, statusCode, err, time.Now())
		})
		cancel()

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
