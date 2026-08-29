package domain

import (
	"math"
	"math/rand"
	"time"
)

const (
	backoffBase = 2 * time.Second
	backoffCap  = 5 * time.Minute
)

// Backoff returns a full-jitter delay for attempt (1-indexed).
func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	exp := float64(backoffBase) * math.Pow(2, float64(attempt-1))
	upper := math.Min(exp, float64(backoffCap))
	return time.Duration(rand.Int63n(int64(upper) + 1))
}
