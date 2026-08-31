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

Starts three services: `postgres` (the only datastore, doubles as the job
queue — see [Architecture overview](#architecture-overview)), `app` (the
dispatcher, HTTP API on `:8080`), and `testreceiver` (a minimal server that
recomputes the HMAC signature on everything it receives and logs whether it
matches, so deliveries are visible without standing up your own endpoint).

Create a job (destination points at `testreceiver`, reachable by its Docker
service name):

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

For a retry-then-dead-letter cycle, post a job with an unreachable
destination:

```bash
curl -X POST localhost:8080/jobs \
  -H "Content-Type: application/json" \
  -d '{"event_type":"payment.failed","payload":{"x":1},"destination_url":"http://localhost:1/"}'
```

After `MAX_ATTEMPTS` (default 5) retries — usually under 30s, backoff is
jittered and capped — check the dead-letter list:

```bash
curl "localhost:8080/jobs?status=dead"
```

Run the tests (the claim-concurrency test needs a real Postgres; `make test`
fails fast with a clear message rather than a silent false pass if it isn't
up):

```bash
cp .env.example .env            # skip if you've already run `make up` once
docker compose up -d postgres   # if `make up` isn't already running
make test
```

Stop everything: `make down`

### Configuration

Environment variables (`internal/config/config.go`), all optional except the
first two:

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

### Secrets

`docker-compose.yml` reads `POSTGRES_PASSWORD`/`HMAC_SECRET` from a gitignored
`.env` rather than hardcoding them. `make up` generates one from
`.env.example` automatically if missing (`cp .env.example .env` to do it by
hand). The defaults are throwaway local-dev values — fine here, not for
production, which needs an actual secrets manager, not a flat file (see
[What the AI got wrong](#what-the-ai-got-wrong)).

Note: `.env` is a `docker-compose`-only mechanism — `go test` never reads it.
Changing `POSTGRES_PASSWORD` in `.env` means also updating
`TEST_DATABASE_URL`, or `make test` fails with a confusing auth error.

### Ports

Postgres is exposed on host port `55432`, not the default `5432`: a Postgres
already running locally on the default port (common on a dev machine) would
otherwise silently intercept connections meant for this project's container.

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

Hexagonal-lite with a couple of tactical DDD patterns, not the full
ports-and-adapters ceremony: `domain` has zero I/O imports, and there are
exactly two consumer-defined interfaces (`Store`, `Deliverer`) — the two
things actually faked in tests. `Job` is the aggregate root; `Outcome` is a
value object; `DecideOutcome` is the single function that owns all
delivery-outcome policy, so the SQL layer never recomputes a business rule,
it only persists a decision already made.

**Job lifecycle:**

1. `POST /jobs` inserts a row (`status='pending'`, `next_attempt_at=now()`).
2. `WORKER_CONCURRENCY` goroutines each poll:
   `SELECT ... WHERE status='pending' AND next_attempt_at<=now() ORDER BY next_attempt_at FOR UPDATE SKIP LOCKED LIMIT 1`,
   transaction held open for the entire delivery attempt.
3. `Deliverer` POSTs the exact stored payload bytes (never re-serialized)
   with `X-Webhook-Id` and `X-Webhook-Signature: sha256=<hmac>`.
4. `domain.DecideOutcome` — pure, no I/O — decides `delivered`, `pending`
   (with a new backoff delay), or `dead`.
5. One of three plain `UPDATE`s persists that decision and commits, releasing
   the row. If anything crashes before the commit, the row is untouched and
   immediately claimable again.
6. On `SIGTERM`/`SIGINT`, the HTTP server stops accepting requests and
   in-flight deliveries get `SHUTDOWN_GRACE_PERIOD` to finish, both in
   parallel, before exit.

**Why Postgres only, no broker:** one durable system to run and explain;
backoff is a `WHERE` clause; dead jobs are queryable natively. None of the
four mechanisms this brief asks for become simpler or more correct with a
broker in front of them.

**Why the claim transaction stays open for the whole delivery** (instead of a
short claim + visibility-timeout window): `FOR UPDATE SKIP LOCKED` held for
the duration makes a double-claim structurally impossible, not just unlikely,
and crash recovery is immediate — Postgres releases the lock the instant the
connection dies.

## Key decisions and trade-offs

### Delivery semantics: at-least-once

No accepted job is ever silently lost; duplicate *deliveries* are possible
and accepted — the same semantics Stripe and other webhook providers use.

**Why not exactly-once:** the last hop is a `POST` to a third-party URL this
service doesn't control, with no shared transactional protocol — the
two-generals problem. There's no way to tell "the receiver failed" apart from
"it succeeded but the ack was lost." What the payments industry calls
"exactly-once" in practice is at-least-once plus an idempotent consumer, via
a stable `X-Webhook-Id` sent unchanged on every attempt.

**Why not at-most-once instead:** that would require marking a job done
*before* attempting delivery — a crash right after would silently lose an
event never actually sent. This system decides and commits the outcome only
*after* `Deliver()` returns, so it can only ever duplicate, never lose.

Three concrete sources of duplication:

1. **Response lost after a real success** — the HTTP call errors out (timeout,
   dropped connection) even though the destination already processed the
   request; there's no way to tell the two apart, so the failure is retried.
2. **Process or Postgres dies before any commit** — the open transaction
   rolls back, the row is unchanged, a live worker retries it later. Includes
   the shutdown grace period expiring mid-delivery.
3. **`Commit()` itself fails right after a successful `UPDATE`** — the whole
   transaction rolls back even though the delivery had genuinely succeeded.

None of these three are prevented, only made safe: every attempt carries the
same `X-Webhook-Id`, so **the receiver must deduplicate on it** — this
dispatcher holds up its half of that contract, not the other half.

### Retry strategy

Two questions: **which failures deserve a retry**, and **how long to wait**.

**Which failures retry** — a network error, timeout, `5xx`, or `429` might
succeed next time; any other `4xx` means the request itself is broken, and
retrying it just burns `MAX_ATTEMPTS` to reach a conclusion already known on
attempt one. `isRetryable` treats those two cases differently.

**How long to wait** — exponential backoff (`2s, 4s, 8s, 16s...`, capped at
5 min), with a random delay between 0 and that value each time (*full
jitter*) instead of the exact value — otherwise every job failing toward the
same destination would retry at the exact same instant ("thundering herd").

After `MAX_ATTEMPTS`, even a transient failure gives up and is marked `dead`.

All of this — success, retryability, the attempts threshold, the backoff
delay — is decided together in one function, `domain.DecideOutcome`, with no
I/O. The SQL layer only saves a decision already made.

### Concurrency model

`WORKER_CONCURRENCY` (default 10) goroutines each run the same loop: grab one
job, deliver it, save the result, repeat.

**Avoiding collisions** — no coordinator hands out work. Each goroutine asks
Postgres for "one pending job I can have" (`FOR UPDATE SKIP LOCKED`), and
Postgres guarantees two goroutines never get the same row. That's the entire
coordination mechanism.

**Why the connection pool size has to match** — a worker keeps its DB
connection tied up for its *entire* delivery attempt (up to
`DELIVERY_TIMEOUT`, 10s by default), not just for a query. So the pool needs
at least `WORKER_CONCURRENCY` connections, or workers would silently queue
for a free one — `db.SetMaxOpenConns(WORKER_CONCURRENCY + 2)` covers all
workers plus headroom for the HTTP API.

**Side benefit, not needed today:** since coordination lives entirely in
Postgres, running two copies of this binary would work correctly with zero
code changes.

### Infrastructure introduced

Just Postgres — no broker, no cache, no message queue, a deliberate choice
(see above). `docker-compose.yml` runs the three services above; Postgres has
a healthcheck so the app never starts against a not-yet-ready database.

## What I'd change before production

Each of these is deliberately documented rather than built, given the time
budget for this exercise:

- **Idempotency-Key at ingestion.** Retrying `POST /jobs` after a timeout can
  create two jobs for the same event — a gap the `X-Webhook-Id` contract
  doesn't cover. Fix: an `Idempotency-Key` header, a `UNIQUE` constraint,
  `INSERT ... ON CONFLICT DO NOTHING` (Stripe's pattern).
- **Per-client HMAC secret**, not one global one — today, a leaked secret
  exposes every client's webhooks.
- **`destination_url` validation against SSRF** — skipped, assuming only
  trusted internal Powens services call this. External callers would need an
  allowlist checked on every call, not just at creation, against DNS rebinding.
- **A message broker**, once volume outgrows Postgres polling or another
  service needs the same events — always paired with an Outbox Pattern.
- **PgBouncer's transaction-mode pooling doesn't fit this design** — holding
  a transaction open for an HTTP call defeats the point of it at scale.
- **A manual requeue endpoint for dead jobs** — read-only today by design;
  would need `WHERE status='dead'` on its `UPDATE` to avoid racing a worker's
  own retry.
- **`LISTEN`/`NOTIFY`** to cut pickup latency below the 500ms poll interval —
  skipped, no measurable benefit at this volume.
- **Check `idle_in_transaction_session_timeout` on managed Postgres.** This
  kills transactions that sit open without running a query — exactly what
  ours looks like while waiting for the destination to respond. Off by
  default here; some managed providers turn it on low. If deployed there, set
  it well above `DELIVERY_TIMEOUT`, or Postgres kills healthy deliveries.
- **Real secrets management** — `.env` (see [Secrets](#secrets)) stops
  committing secrets to git; production needs an actual secrets manager.

## What the AI got wrong

Grouped by how each was found — more interesting than grouping by topic.

**Decided on principle, reversed after being challenged:**

- The original design used a short lock plus a visibility-timeout window,
  justified by "the HTTP call isn't reliably bounded." That stopped being
  true once a delivery timeout was added for an unrelated reason — nobody
  revisited the decision until asked to. Replayed, holding the transaction
  open for the whole delivery won outright.
- Whether to build an `Idempotency-Key` flip-flopped four times, argued each
  time on principle instead of the one question that actually settled it:
  the time cost. That should have come first.
- A defensive `idempotency_key UNIQUE` column stayed in the schema after
  deciding not to build the feature, "in case there's time" — removed; a
  column never written to announces a feature that doesn't exist.
- The concurrency test's cost was overestimated (45–60 min quoted, ~20–25
  actual), nearly used as a reason to cut the one test verifying the
  system's core safety claim.

**Bugs caught by review:**

- Most consequential: the retry-vs-dead rule lived in a SQL `CASE` as well
  as in Go, letting `next_attempt_at` advance on jobs simultaneously marked
  `dead`. Fixed by moving the whole decision into `DecideOutcome`, reducing
  the SQL to three plain `UPDATE`s.
- `DecideOutcome`'s first draft took three consecutive `int` arguments — a
  shape where swapped arguments compile and fail silently. Fixed by passing
  `*Job` instead.
- `Store` originally returned a raw `*sql.Tx` to the `worker` package,
  making it untestable without a real database. Fixed by inverting control:
  `WithClaimedJob(ctx, fn)`.
- `ORDER BY next_attempt_at` was missing from the claim query — flagged in
  research notes before the SQL was written, still missed until review.
- The response body wasn't drained before `Close()`, and the worker used
  `context.Background()` for its transaction, removing every time bound and
  quietly defeating the shutdown grace period. Fixed with
  `context.WithoutCancel(ctx)` plus a local timeout.
- A migration shipped without its `down` file, conflating "no migration tool
  needed" with "no need for a reversible schema file."
- The comment rule ("one line per exported identifier, nothing else")
  existed before the first domain files were written, but wasn't followed
  writing them.

**Found by being asked the right question:**

- The Postgres password and HMAC secret were hardcoded in the committed
  `docker-compose.yml`. A scan flagged it; the reply was "nothing real is
  exposed" — true, but it dodged the real question, asked afterward: if
  someone clones this repo, where do *their* secrets come from? They
  didn't — everyone got the same committed value. Fixed with `.env`.

**The pattern worth naming:** several reversals above were decided on
principle without checking the concrete fact underneath — the actual time
cost, what a test would verify, whether a column would ever be used.
Principle was a consistently bad substitute for checking.
