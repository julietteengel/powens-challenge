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
destination:

```bash
curl -X POST localhost:8080/jobs \
  -H "Content-Type: application/json" \
  -d '{"event_type":"payment.failed","payload":{"x":1},"destination_url":"http://localhost:1/"}'
```

Wait for `MAX_ATTEMPTS` (default 5) attempts with growing backoff between
them — usually under 30 seconds, since backoff is jittered and capped — then
check it landed in the dead-letter list:

```bash
curl "localhost:8080/jobs?status=dead"
```

Run the tests. The claim-concurrency test needs a real Postgres — start it
first, or `make test` fails fast with a clear "is `make up` running?" message
rather than a silent false pass. `make up` creates `.env` automatically on
first run (see [Secrets](#secrets) below); starting Postgres standalone
without ever having run `make up` needs that same step first:

```bash
cp .env.example .env            # skip if you've already run `make up` once
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

### Secrets

`docker-compose.yml` doesn't hardcode `POSTGRES_PASSWORD` or `HMAC_SECRET` —
it reads them from a `.env` file (Docker Compose's built-in variable
substitution), which is gitignored and never committed. `make up` creates one
from `.env.example` automatically if it doesn't exist yet; to do it manually:

```bash
cp .env.example .env
```

The default values are throwaway local-dev placeholders (see
[What the AI got wrong](#what-the-ai-got-wrong) for why this wasn't the
original setup) — fine for this demo, where nothing real is protected by
either value. A real deployment would pull both from an actual secrets
manager, never from a flat file at all.

`.env` is read by `docker-compose` only — `make test` runs `go test`
directly, which never reads it. If you change `POSTGRES_PASSWORD` in
`.env`, update `TEST_DATABASE_URL` to match, or `make test` will fail to
connect with a confusing auth error instead of an obvious one.

### Ports

`docker-compose.yml` exposes Postgres on host port `55432`, not the default
`5432`: a Postgres already running locally on the default port (common on a
backend developer's machine) would otherwise silently intercept connections
meant for this project's container.

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

None of these three are prevented, only made safe: every attempt carries the
same `X-Webhook-Id`, so **the receiver must deduplicate on it** — this
dispatcher holds up its half of that contract, not the other half. In
production, whoever owns the receiving endpoint has to actually implement
that check.

### Retry strategy

Two separate questions: **which failures deserve a retry**, and **how long to
wait before retrying**.

**Which failures retry** — a network error, a timeout, a `5xx`, or a `429`
might well succeed on the next try: nothing was wrong with the request
itself. But any other `4xx` (bad payload, invalid URL, refused auth) means
the request itself is broken — retrying the exact same broken request
won't fix it, it just burns through all `MAX_ATTEMPTS` to reach a
conclusion already known on the first try. So `isRetryable` treats those two
cases differently: retry the first kind, mark the second `dead` right away.

**How long to wait** — exponential backoff (`2s, 4s, 8s, 16s...`, capped at
5 minutes), but with a random delay between 0 and that value each time
(*full jitter*), not the exact value. Without that randomness, every job
failing toward the same destination at the same moment would all retry
again at the exact same instant, hammering it right when it's already
struggling (a "thundering herd"). Jitter spreads retries out over time for
almost no extra code.

After `MAX_ATTEMPTS`, even a transient failure finally gives up and is
marked `dead`.

All four of these decisions — was it a success, is it retryable, has it hit
the limit, how long to wait — are made together in one function,
`domain.DecideOutcome`, with no database or network calls inside it. The SQL
layer never re-derives any of this; it only saves a decision that's already
been made.

### Concurrency model

`WORKER_CONCURRENCY` (default 10) is how many jobs can be delivered at the
same time. That's implemented as that many goroutines, each running the
exact same loop on its own: try to grab one job, deliver it, save the
result, repeat.

**How they avoid grabbing the same job** — there's no coordinator handing
out work. Each goroutine just asks Postgres for "one pending job I can
have" (`FOR UPDATE SKIP LOCKED`), and Postgres itself guarantees that two
goroutines asking at the same time never get the same row. That's the
entire coordination mechanism — nothing built in Go, no in-memory
scheduler, no lock file.

**Why the connection pool size has to match** — normally a database
connection is only busy for the fraction of a second a query takes. Here,
each worker keeps its connection tied up for its *entire* delivery attempt
— including the HTTP call to the destination, which can take up to
`DELIVERY_TIMEOUT` (10s by default). So with 10 workers, at least 10
database connections need to be available at once, or some workers would
just sit waiting for a free connection instead of actually delivering
anything — `WORKER_CONCURRENCY=50` would silently behave like a much lower
number. `db.SetMaxOpenConns(WORKER_CONCURRENCY + 2)` sets the pool large
enough for all the workers, plus a couple of spare connections so the HTTP
API isn't left waiting when every worker is busy at once.

**A side benefit, not something this project needs today**: since the
coordination lives entirely in Postgres, running two copies of this binary
at once would work correctly with zero code changes — they'd naturally
never grab the same job either.

### Infrastructure introduced

Just Postgres. No broker, no cache, no message queue — a deliberate choice
(see above), not an oversight. `docker-compose.yml` runs the three services
listed in [How to run it](#how-to-run-it); Postgres has a healthcheck so the
app never starts against a not-yet-ready database.

## What I'd change before production

Each of these is deliberately documented rather than built, given the time
budget for this exercise:

- **Idempotency-Key at ingestion.** Retrying `POST /jobs` after a timeout can
  create two jobs for the same event — a gap the `X-Webhook-Id` dedup
  contract doesn't cover. Fix: an `Idempotency-Key` header, a `UNIQUE`
  constraint, `INSERT ... ON CONFLICT DO NOTHING` (Stripe's pattern).
- **Per-client HMAC secret**, not one global one — today, a secret leaked for
  one client exposes every client's webhooks.
- **`destination_url` validation against SSRF.** Skipped, assuming only
  trusted internal Powens services call this. An externally-reachable
  dispatcher would need an allowlist, checked on every call, not just at
  creation, to prevent DNS rebinding.
- **A message broker**, once volume outgrows Postgres polling or another
  service needs the same events — always paired with an Outbox Pattern,
  never added alone.
- **PgBouncer's transaction-mode pooling doesn't fit this design** — holding
  a transaction open for an entire HTTP call defeats the point of it at
  scale.
- **A manual requeue endpoint for dead jobs** — read-only today by design.
  Would need `WHERE status='dead'` on its `UPDATE` to avoid racing a
  worker's own retry.
- **`LISTEN`/`NOTIFY`** to cut pickup latency below the 500ms polling
  interval — skipped, no measurable benefit at this volume.
- **Check `idle_in_transaction_session_timeout` on managed Postgres.** This
  setting makes Postgres kill any transaction that sits open without running
  a query for too long — meant to catch buggy clients that forget to close a
  transaction. Ours looks exactly like that on purpose: while waiting for the
  destination to respond, the transaction is open but no SQL is running.
  Vanilla Postgres (and this project's Docker image) disables this by
  default, so it's a non-issue here — but some managed providers turn it on
  with a low value. If ever deployed there, it must be raised well above
  `DELIVERY_TIMEOUT`, or Postgres would kill perfectly healthy deliveries
  mid-flight, mistaking them for a stuck client.
- **Real secrets management.** `docker-compose.yml` reads secrets from a
  gitignored `.env` now (see [Secrets](#secrets)) — enough to stop
  committing them, not enough for production, which needs an actual secrets
  manager, not a flat file.

## What the AI got wrong

Grouped by how each was found — that turned out more interesting than
grouping by topic.

**Decided on principle, reversed after being challenged:**

- The original design claimed a job with a short lock plus a
  visibility-timeout window, justified by "the HTTP call isn't reliably
  bounded." That stopped being true once a delivery timeout was added for
  an unrelated reason — nobody revisited the decision until asked to.
  Replayed, holding the transaction open for the whole delivery won
  outright: it closes a whole class of duplicates and recovers from a crash
  instantly.
- Whether to build an `Idempotency-Key` at ingestion flip-flopped four
  times, argued each time on principle instead of the one question that
  actually settled it: the time cost. That should have come first.
- A defensive `idempotency_key UNIQUE` column stayed in the schema after
  deciding not to build the feature, "in case there's time." A column never
  written to announces a feature that doesn't exist — removed.
- The concurrency test's cost was overestimated (45–60 min quoted, ~20–25
  actual) and nearly used as the reason to cut the one test that verifies
  the system's core safety claim.

**Bugs caught by review:**

- The most consequential one: the retry-vs-dead rule lived in a SQL `CASE`
  as well as in Go, breaking an already-agreed rule to keep business logic
  in one place. It's exactly what let `next_attempt_at` advance on jobs
  simultaneously marked `dead`. Fixed by moving the whole decision into one
  function (`DecideOutcome`), reducing the SQL to three plain `UPDATE`s.
- `DecideOutcome`'s first draft took three consecutive `int` arguments — a
  shape where swapped arguments compile fine and fail silently. Fixed by
  passing `*Job` instead.
- `Store` originally returned a raw `*sql.Tx` to the `worker` package,
  leaking Postgres into a layer meant to stay ignorant of it and making it
  untestable without a real database. Fixed by inverting control:
  `WithClaimedJob(ctx, fn)`.
- `ORDER BY next_attempt_at` was missing from the claim query — already
  flagged in research notes before the SQL was written, and still missed
  until a dedicated review caught it.
- The response body wasn't drained before `Close()` (blocks connection
  reuse), and the worker used `context.Background()` for its transaction —
  removing every time bound, not just the HTTP call, quietly defeating the
  shutdown grace period. Fixed with `context.WithoutCancel(ctx)` plus a
  local timeout.
- A migration shipped without its `down` file, conflating "no migration
  tool needed" with "no need for a reversible schema file."
- The comment rule ("one line per exported identifier, nothing else")
  existed before the first domain files were written, but wasn't followed
  writing them. Trimmed after the fact.

**Found by being asked the right question:**

- The Postgres password and HMAC secret were hardcoded in the committed
  `docker-compose.yml`. A scan flagged it; the reply was "fine, this
  Postgres is thrown away every run, nothing real is exposed" — true, but
  it dodged the actual question, asked afterward: if someone else clones
  this repo, where do *their* secrets come from? They didn't — everyone
  got the same committed value. Fixed with a gitignored `.env` instead.

**The pattern worth naming:** several of the reversals above were decided
on principle without checking the concrete fact underneath — the actual
time cost, what a test would verify, whether a column would ever be used.
Principle was a consistently bad substitute for checking.
