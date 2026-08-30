package postgres

import (
	"context"
	"database/sql"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver used by sql.Open

	"powens-challenge/internal/domain"
)

var testDB *sql.DB

// TestMain reuses the Postgres already started by `make up` rather than
// spinning up testcontainers — it exists for the app already.
func TestMain(m *testing.M) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:55432/webhooks?sslmode=disable"
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatalf("open test database: %v", err)
	}
	if err := db.Ping(); err != nil {
		log.Fatalf("connect to test database (is `make up` running?): %v", err)
	}
	testDB = db

	code := m.Run()
	_ = db.Close()
	os.Exit(code)
}

func TestWithClaimedJob_ClaimsOnlyEligibleJobOnce(t *testing.T) {
	store := NewStore(testDB)
	now := time.Now()

	pendingDueID := insertJob(t, now.Add(-time.Minute), domain.StatusPending)
	pendingFutureID := insertJob(t, now.Add(time.Hour), domain.StatusPending)
	deliveredID := insertJob(t, now, domain.StatusDelivered)
	deadID := insertJob(t, now, domain.StatusDead)
	t.Cleanup(func() { deleteJobs(t, pendingDueID, pendingFutureID, deliveredID, deadID) })

	claimedIDs := make(chan string, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			claimed, err := store.WithClaimedJob(context.Background(), func(job *domain.Job) domain.Outcome {
				claimedIDs <- job.ID
				return domain.Outcome{Status: domain.StatusDelivered}
			})
			if err != nil {
				t.Errorf("WithClaimedJob: %v", err)
			}
			if !claimed {
				claimedIDs <- ""
			}
		})
	}
	wg.Wait()
	close(claimedIDs)

	var got []string
	for id := range claimedIDs {
		if id != "" {
			got = append(got, id)
		}
	}

	if len(got) != 1 {
		t.Fatalf("claimed %d jobs concurrently, want exactly 1: %v", len(got), got)
	}
	if got[0] != pendingDueID {
		t.Errorf("claimed job %q, want the due pending job %q", got[0], pendingDueID)
	}
}

func insertJob(t *testing.T, nextAttemptAt time.Time, status string) string {
	t.Helper()
	var id string
	err := testDB.QueryRow(`
		INSERT INTO jobs (event_type, payload, destination_url, status, next_attempt_at)
		VALUES ('test.event', '{}', 'http://example.invalid', $1, $2)
		RETURNING id`,
		status, nextAttemptAt,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert test job: %v", err)
	}
	return id
}

func deleteJobs(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := testDB.Exec(`DELETE FROM jobs WHERE id = $1`, id); err != nil {
			t.Errorf("cleanup job %s: %v", id, err)
		}
	}
}
