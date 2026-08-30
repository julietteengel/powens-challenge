package domain

import "time"

const (
	StatusPending   = "pending"
	StatusDelivered = "delivered"
	StatusDead      = "dead"
)

// Job is the aggregate root for a webhook delivery job.
type Job struct {
	ID             string
	EventType      string
	Payload        []byte // never re-marshal: breaks the HMAC signature
	DestinationURL string
	Status         string
	AttemptsCount  int
	LastError      string
	NextAttemptAt  time.Time
	CreatedAt      time.Time
}
