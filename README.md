# Webhook Dispatcher

A small service that accepts webhook-style events over HTTP, persists them, and
delivers them to a destination URL with retries, exponential backoff, HMAC
signing, and dead-lettering after a configurable number of attempts. Built for
the Powens EMI backend technical test.

## How to run it

Requirements: Docker and Docker Compose.

```bash
make up
```

This starts three services:

- `postgres` — the only datastore, doubles as the job queue (see
  [Architecture overview](#architecture-overview))
- `app` — the dispatcher itself, HTTP API on `:8080`
- `testreceiver` — a minimal server that recomputes the HMAC signature on
  everything it receives and logs whether it matches, so you can see
  deliveries land without standing up your own endpoint

Create a job (the destination points at `testreceiver`, reachable from the
`app` container by its service name on the Docker network):

```bash
curl -X POST localhost:8080/jobs \
  -H "Content-Type: application/json" \
  -d '{
    "event_type": "payment.completed",
    "payload": {"amount": 4200, "currency": "EUR"},
    "destination_url": "http://testreceiver:9000/"
  }'
```

You should get back `{"id":"<uuid>"}` with a `201`. Watch it get delivered:

```bash
docker compose logs -f testreceiver
# webhook id=<uuid> signature_valid=true body="{\"amount\": 4200, \"currency\": \"EUR\"}"
```

To see a retry-then-dead-letter cycle, post a job with an unreachable
destination (e.g. `http://localhost:1/`), wait for `MAX_ATTEMPTS` (default 5)
attempts with growing backoff between them, then:

```bash
curl "localhost:8080/jobs?status=dead"
```

Run the tests. The claim-concurrency test needs a real Postgres — start it
first, or `make test` fails fast with a clear "is `make up` running?" message
rather than a silent false pass:

```bash
docker compose up -d postgres   # if `make up` isn't already running
make test
```

Stop everything:

```bash
make down
```

### Configuration

All via environment variables (see `internal/config/config.go`), all optional
except the first two:

| Variable | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | — (required) | Postgres connection string |
| `HMAC_SECRET` | — (required) | Secret used to sign every delivery |
| `WORKER_CONCURRENCY` | `10` | Number of concurrent delivery workers |
| `MAX_ATTEMPTS` | `5` | Attempts before a job is marked dead |
| `DELIVERY_TIMEOUT` | `10s` | Timeout for the outbound HTTP call |
| `SHUTDOWN_GRACE_PERIOD` | `15s` | How long in-flight deliveries get to finish on shutdown |
| `ADDR` | `:8080` | HTTP listen address |
| `TEST_DATABASE_URL` | `postgres://postgres:postgres@localhost:55432/webhooks?sslmode=disable` | Used only by `make test` |

`docker-compose.yml` exposes Postgres on host port `55432`, not the default
`5432`: a Postgres already running locally on the default port (common on a
backend developer's machine) would otherwise silently intercept connections
meant for this project's container — found by actually running the
concurrency test, see [What the AI got wrong](#what-the-ai-got-wrong).

## Architecture overview

```
cmd/dispatcher       the binary: wires everything together, owns graceful shutdown
cmd/testreceiver     standalone verification server (see above)
internal/domain      pure business logic, zero I/O: Job, Outcome, Backoff, DecideOutcome
internal/postgres    Store: claim + persist, one Postgres table doubling as the queue
internal/httpclient  Deliverer: signs and POSTs the webhook
internal/httpapi     POST /jobs, GET /jobs?status=dead
internal/worker      orchestrates N goroutines running claim -> deliver -> persist
internal/config      environment variable loading
migrations/          up/down SQL pair for the jobs table
```

The design is hexagonal-lite with a couple of tactical DDD patterns, not the
full ports-and-adapters ceremony: `domain` has zero I/O imports, and there are
exactly two consumer-defined interfaces (`Store`, `Deliverer`) — the two
things actually faked in tests. `Job` is the aggregate root (stable identity,
mutable lifecycle state); `Outcome` is a value object; `DecideOutcome` is the
single function that owns all delivery-outcome policy, so the SQL layer never
recomputes a business rule, it only persists a decision already made.

**Job lifecycle:**

1. `POST /jobs` inserts a row (`status='pending'`, `next_attempt_at=now()`).
2. `WORKER_CONCURRENCY` goroutines each poll:
   `SELECT ... WHERE status='pending' AND next_attempt_at<=now() ORDER BY next_attempt_at FOR UPDATE SKIP LOCKED LIMIT 1`,
   with the transaction held open for the entire delivery attempt.
3. The `Deliverer` POSTs the exact stored payload bytes (never re-serialized)
   with `X-Webhook-Id` and `X-Webhook-Signature: sha256=<hmac>`.
4. `domain.DecideOutcome` — a pure function, no I/O — decides `delivered`,
   `pending` (with a new backoff delay), or `dead`.
5. One of three plain `UPDATE`s persists that decision and the transaction
   commits, releasing the row. If anything crashes before the commit, the row
   is untouched and immediately claimable again by another worker.
6. On `SIGTERM`/`SIGINT`, the HTTP server stops accepting new requests and
   in-flight deliveries get `SHUTDOWN_GRACE_PERIOD` to finish, both in
   parallel, before the process exits.

**Why Postgres only, no broker:** a single durable system to run and explain;
backoff is a `WHERE` clause; dead jobs are queryable natively without
synchronizing a second system's state with the database. None of the four
mechanisms this brief actually asks for (backoff, dead-lettering, concurrency,
HMAC) become simpler or more correct with a broker in front of them.

**Why the claim transaction stays open for the whole delivery, instead of a
short claim + a visibility-timeout window:** `FOR UPDATE SKIP LOCKED` held for
the duration makes a double-claim structurally impossible rather than merely
unlikely, and crash recovery is immediate — Postgres releases the lock the
instant the connection dies, no arbitrary timeout window to tune.

## Key decisions and trade-offs

### Delivery semantics: at-least-once

No accepted job is ever silently lost; duplicate *deliveries* of the same job
are possible and are an accepted, documented trade-off, not a bug — the same
semantics Stripe and other webhook providers use.

**Why not exactly-once:** the last hop is a `POST` to a third-party URL this
service doesn't control, with no shared transactional protocol — the
two-generals problem. There's no way to tell "the receiver failed" apart from
"it succeeded but the acknowledgment was lost," with any mechanism. What the
payments industry calls "exactly-once" in practice is at-least-once plus an
idempotent consumer, which is what's implemented here via a stable
`X-Webhook-Id` sent unchanged on every attempt.

**Why not at-most-once instead:** that would require marking a job as done
*before* attempting delivery — a crash right after that mark would silently
lose an event that was never actually sent. This system always decides and
commits the outcome only *after* `Deliver()` returns, so it can only ever
duplicate, never lose.

Three concrete, code-level sources of duplication:

1. **Response lost after a real success** — the outbound HTTP call errors out
   (timeout, connection drop) even though the destination already received
   and processed the request. The HTTP client has no way of knowing whether
   the request was processed before the connection dropped, so the failure
   is treated as retryable.
2. **Process or Postgres dies before any commit** — the open transaction rolls
   back, the row is left exactly as it was, and a live worker retries it
   later. Includes the shutdown grace period expiring mid-delivery.
3. **The final `Commit()` itself fails right after a successful `UPDATE`** —
   the whole transaction, including that `UPDATE`, is rolled back even though
   the delivery had already genuinely succeeded.

### Retry strategy

Not every failure is retried the same way. `isRetryable` classifies: a network
error, timeout, `5xx`, or `429` is transient and retried; any other `4xx`
(bad payload, invalid URL, auth failure) is treated as terminal immediately —
retrying a structurally broken request would just burn the whole backoff
schedule before reaching the same conclusion available on attempt one.

Backoff is full-jitter exponential: `delay = random(0, min(cap, base *
2^(attempt-1)))`, `base=2s`, `cap=5min`. Plain exponential backoff without
jitter would make every failing job toward the same destination retry at
exactly the same instant (thundering herd); jitter spreads that out for
negligible extra code.

After `MAX_ATTEMPTS` a still-retryable failure is finally marked `dead` too.
All of this — success detection, classification, the attempts threshold, the
backoff calculation — lives in one pure function, `domain.DecideOutcome`. The
SQL layer never recomputes any of it; it only executes one of three plain
`UPDATE` statements for an outcome already decided.

### Concurrency model

`WORKER_CONCURRENCY` independent goroutines each run the same
claim-deliver-persist loop; `FOR UPDATE SKIP LOCKED` is the only coordination
mechanism between them — no in-memory dispatcher, no semaphore, no broker.

`db.SetMaxOpenConns(WORKER_CONCURRENCY + 2)` matters more than it looks: each
worker holds one connection for the duration of its transaction, so the pool
needs at least that many, and the `+2` keeps the HTTP API from starving while
every worker is mid-delivery. Without this explicit call, changing
`WORKER_CONCURRENCY` would silently have no effect once past the connection
pool's own default size — a real footgun if left unconfigured.

This model also scales to multiple instances of the binary with zero code
change, since `SKIP LOCKED` coordinates just as well across processes as
across goroutines in one process — not needed at this scale, but not blocked
by this design either.

### Infrastructure introduced

Just Postgres. No broker, no cache, no message queue — a deliberate choice
(see above), not an oversight. `docker-compose.yml` runs the three services
listed in [How to run it](#how-to-run-it); Postgres has a healthcheck so the
app never starts against a not-yet-ready database.

## What I'd change before production

Each of these is deliberately documented rather than built, given the time
budget for this exercise:

- **Idempotency-Key at ingestion.** A caller retrying `POST /jobs` after a
  network timeout can currently create two distinct jobs for the same event,
  each with its *own* `X-Webhook-Id` — invisible to the dedup contract above,
  which only covers duplicates created by *this service's own* retries. Fix:
  an optional `Idempotency-Key` header, a `UNIQUE` constraint, and
  `INSERT ... ON CONFLICT DO NOTHING RETURNING id` (return the existing job
  with `200` instead of creating a new one), the standard pattern from Stripe
  and similar APIs.
- **Per-client HMAC secret**, not one global secret — in a real multi-tenant
  Powens, a secret compromised for one client would expose every other
  client's webhooks.
- **`destination_url` validation against SSRF.** Not validated today, on the
  assumption that this dispatcher is only ever called internally by trusted
  Powens services. A dispatcher reachable by external or less-trusted callers
  would need an allowlist, re-checked at call time (not just at job creation)
  to defend against DNS rebinding.
- **A message broker, past a clear threshold** — if throughput outgrows what
  Postgres polling comfortably absorbs, or another service needs to consume
  the same event stream. Should always ship together with an Outbox Pattern,
  never introduced alone.
- **PgBouncer transaction-mode pooling doesn't fit this design as-is** — a
  transaction held open for the duration of an external HTTP call defeats the
  point of a transaction-mode pooler at real scale.
- **A manual requeue endpoint for dead jobs** — currently read-only by design.
  Adding one would need its `UPDATE` re-guarded with `WHERE status='dead'` to
  avoid racing a worker's legitimate retry of the same row.
- **`LISTEN`/`NOTIFY`** to cut new-job pickup latency from the polling
  interval (currently 500ms) to near-zero, without adding infrastructure —
  skipped because the benefit isn't measurable at this volume.
- **Tune `idle_in_transaction_session_timeout` on managed Postgres.** Disabled
  by default on vanilla Postgres (and in this project's Docker image), but
  some managed providers tighten it. If ever deployed on one, it should be set
  well above the HTTP delivery timeout — never below, or Postgres would kill a
  perfectly normal in-flight delivery's transaction.
- **Real secrets management for `docker-compose.yml`.** The Postgres password
  and HMAC secret in there are throwaway local-dev placeholders (the Postgres
  instance is recreated fresh on every `up`, reachable only from localhost) —
  fine for a zero-config local demo, not for anything real. A real deployment
  would pull these from a secrets manager or `.env` file, never commit them.

## What the AI got wrong

Tracked as they happened rather than reconstructed afterward. Grouped by how
they were found, because that turned out to be the more interesting axis:

**Reversed after being challenged on principle, before any code existed:**

- The original design used a short claim plus a visibility-timeout window,
  justified by "the external HTTP call's duration isn't reliably bounded."
  That premise became false the moment an outbound HTTP timeout was set for
  an unrelated reason (stopping a silent destination from freezing a worker
  forever) — but nothing prompted revisiting the earlier decision until asked
  to. Once replayed, the long-held-transaction design won outright: it closes
  an entire class of duplicates and recovers from a crash immediately.
- Whether to implement an `Idempotency-Key` at ingestion flip-flopped four
  times, each round arguing principle (consistency with "no over-engineering,"
  "the one duplicate no existing contract catches") instead of the one
  question that actually settled it: how much time it would cost against a
  budget of a few hours for the whole exercise. That question should have
  come first, not fourth.
- A defensive `idempotency_key UNIQUE` column was left in the schema after
  deciding not to build the feature, "in case there's time later" — a column
  never written to announces a feature that doesn't exist, which is worse
  than simply not mentioning it. Removed; it can come back with the code.
- The cost of the claim-concurrency test was overestimated (45–60 minutes
  quoted, ~20–25 actual) and nearly used as the reason to cut the one test
  that verifies the system's central claim about concurrency safety.

**Bugs caught by review, before or after being written:**

- The single most consequential one: the "is this job dead or does it retry"
  rule was duplicated into a SQL `CASE` statement, despite an
  already-agreed rule to keep all business logic in the domain layer.
  Concretely, this is what let `next_attempt_at` get advanced even on a job
  that was simultaneously marked `dead` in the same statement — a bug that
  disappeared structurally, not just cosmetically, once the rule was moved
  into one pure function (`DecideOutcome`) and the SQL reduced to three plain
  `UPDATE`s with no conditional logic left to desynchronize.
- A function signature with three consecutive `int` parameters
  (`DecideOutcome(currentAttempts, maxAttempts, statusCode int, ...)`) — the
  kind of shape where a swapped argument order compiles fine and fails
  silently. Fixed by passing the `*Job` itself instead of a bare int.
- The `Store` interface originally returned a raw `*sql.Tx` to the `worker`
  package, leaking `database/sql` into a layer meant to stay ignorant of
  Postgres and making it impossible to fake in a test without a real
  database. Fixed by inverting control:
  `WithClaimedJob(ctx, fn) (claimed bool, err error)`.
- `ORDER BY next_attempt_at` was missing from the claim query — not an
  analysis gap, an execution one: the need for it was already written down in
  research notes before the SQL was finalized, and still didn't make it into
  the query until a dedicated code review caught it.
- The outbound response body wasn't drained before `Close()` (defeats HTTP
  connection reuse under load), and the worker used `context.Background()`
  for the claim/delivery transaction — removing every time bound on
  `BeginTx`/`Commit`, not just the HTTP call, and quietly defeating the point
  of the shutdown grace period. Fixed with `context.WithoutCancel(ctx)` plus
  a local `context.WithTimeout`.
- A migration was written without its `down` counterpart, conflating "no
  migration tool needed" (true, given the scope) with "no need for a
  reversible schema file" (false — a `DROP TABLE` costs one line and is
  useful even for manually resetting a local dev database).
- The project rule was "one line per exported identifier, nothing by default
  on unexported ones, never restate what the code already says." The first
  three domain files written (`job.go`, `backoff.go`, `outcome.go`) carried
  multi-sentence comments on almost every type and function regardless — the
  rule existed before the code did, but wasn't applied while writing it.
  Trimmed back to the budget across all three files.

**Found only by running things, never by reading the code:**

- A hardcoded Postgres host port (`5432` in `docker-compose.yml`) collided
  with a completely unrelated Postgres already running locally on this
  machine. No code review would ever have caught this — the YAML is valid,
  `docker compose up` reports no error, and nothing in the Go code is at
  fault. It only surfaced by actually running the claim-concurrency test
  against real infrastructure. The stake wasn't a demo: it's the evaluator's
  own first `make up`, on a machine that, being a backend developer's, has a
  decent chance of already having Postgres on that same default port. Fixed
  by exposing Postgres on `55432` instead — deliberately not `5433` either,
  since that's itself the conventional second choice a developer reaches for.

**The one recurring pattern worth naming:** three separate times above (the
`Idempotency-Key` reversals, the concurrency test near-cut, the leftover
schema column), a decision got made on an argument of principle without
checking the concrete fact underneath it — the actual time cost, what the
test would actually verify, whether the column would actually be used.
Principle alone was a consistently bad substitute for checking.
