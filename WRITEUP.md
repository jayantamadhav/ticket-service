# WRITEUP

## The atomic decision

The seat-claim decision happens inside a single Postgres transaction, never
as read-then-write from the application:

1. Lock the exact seat rows with
   `SELECT ... FROM seats WHERE show_id = $1 AND seat_label = ANY($2) FOR UPDATE`
   ([reservations.go:59-68](internal/store/reservations.go#L59-L68)). Any
   other transaction touching those rows blocks until this one commits or
   rolls back.
2. With rows locked, check each seat's status in memory and bail out for the
   *whole request* if any seat isn't available
   ([reservations.go:86-97](internal/store/reservations.go#L86-L97)).
3. Only then `UPDATE` them to `held`, insert the reservation, commit.

"Is it free? ok, take it" is one atomic unit, not two round trips — nothing
can read a consistent view of those rows while they're locked, so the classic
double-sell race can't happen.

**Deadlock avoidance for multi-seat requests**: seat labels are sorted
before the `FOR UPDATE` query ([reservations.go:56-57](internal/store/reservations.go#L56-L57)),
so every transaction acquires overlapping seats' locks in the same
order regardless of request order. Two transactions wanting overlapping sets
queue behind each other instead of deadlocking.

**Per-user limit** uses the same pattern — a single conditional `UPDATE`:

```sql
UPDATE user_show_holds
SET held_count = held_count + $3
WHERE show_id = $1 AND user_id = $2
  AND held_count + $3 <= (SELECT per_user_limit FROM shows WHERE id = $1)
RETURNING held_count
```

([reservations.go:110-116](internal/store/reservations.go#L110-L116)) either
updates and returns a row, or returns nothing if the limit would be
exceeded. No separate read-then-write.

## Idempotency

- **Storage**: `idempotency_key` column on `reservations`, with a
  `UNIQUE(user_id, idempotency_key)` constraint
  ([migrations/000003](migrations/000003_scope_idempotency_per_user.up.sql)).
  Scoped per-user so two users can't collide on the same key string.
- **Enforcement**: `checkIdempotency` ([reservations.go:157-176](internal/store/reservations.go#L157-L176))
  runs inside the same transaction, before anything else. Existing key +
  same seats → return the existing reservation. Existing key + different
  seats → 409.
- **Known gap**: two requests with the *identical* key arriving genuinely
  simultaneously can both pass the in-transaction check before either
  commits. The DB's `UNIQUE` constraint guarantees only one insert
  succeeds (no double-booking, no double-charge), but the loser currently
  surfaces as a generic 500 instead of the clean "here's your existing
  reservation" response. Fix: catch Postgres's `23505` on insert and retry
  the read once.

## Holds & expiry

Reservations are created `held` with `held_until = now + 45s`
([reservations.go:14](internal/store/reservations.go#L14)). Expiry is lazy,
not a background sweep — a `held` seat past its TTL is treated as available
both when a new reservation claims it
([reservations.go:92-96](internal/store/reservations.go#L92-L96)) and when
read via `GET /shows/{id}` ([shows.go:101-105](internal/store/shows.go#L101-L105)).

`POST /reservations/{id}/confirm` promotes `held` → `confirmed` if the hold
hasn't expired. `POST /reservations/{id}/cancel` releases a hold or
confirmed booking early, owner-only, and only releases seats still pointing
at that reservation ID — so a cancel can never resurrect a seat already
reassigned to someone else ([confirm_cancel.go:110-116](internal/store/confirm_cancel.go#L110-L116)).

Trade-off: lazy expiry means `user_show_holds.held_count` is only
decremented on explicit cancel, not when a hold silently lapses. A user who
lets all 4 holds expire without cancelling stays at their limit until they
cancel or something else touches those rows. Fix: a periodic sweep that also
decrements `held_count`, or deriving `held_count` from live seat rows
instead of maintaining it as a counter.

## Consistency vs. availability under a partition

Consistency wins on the write path: every reservation requires a successful
transaction against the single Postgres instance. If the DB is unreachable,
`/readyz` fails closed (503) and writes simply fail, rather than risk two
instances with divergent views selling the same seat. No multi-region/
multi-primary setup — one datastore is the system of record, which is the
right trade for "never double-sell," at the cost of the write path going
down if that instance is unreachable. A reasonable next step is serving
slightly-stale `GET /shows/{id}` reads from a replica during a primary
outage, since a stale "sold out" is safer than inventing availability.

## Observability — what pages at 2am

- A spike in `reservations_declined_total{reason="seat_taken"}` alone is
  expected during a real stampede — not a page.
- Any 5xx rate above zero is the page: it means the correctness guarantees
  this whole exercise is about no longer hold at the API boundary.
- `/readyz` returning 503 for more than a few seconds — DB dependency down.
- The reconciliation check (`available + held + confirmed != total`, also
  checked by `cmd/burst`) failing would mean the atomic-decision invariant
  itself broke — should never happen, worth its own alert.
- Every log line carries a `request_id`, so one problematic request can be
  grepped end-to-end across create/reserve/confirm/cancel.

## AI usage

Used in the order in [AI_USAGE.md](AI_USAGE.md): scaffolding/Dockerizing,
migrations from already-designed models, handlers, unit tests, Prometheus
wiring, the burst script. Each step was reviewed individually rather than
accepted as one generated block. The atomic seat-locking design
(`SELECT ... FOR UPDATE` + sorted lock order + conditional per-user-limit
`UPDATE`) was directed deliberately — that's the one piece this exercise is
unforgiving about getting wrong. AI decided more of the boilerplate: JSON
(de)serialization, error-to-status mapping, the Dockerfile, the logging
middleware.

## What's next

- Catch the idempotency-key insert race (`23505`) instead of 500ing.
- Gate `POST /shows` behind an actual admin credential — currently open.
- Sweep expired holds (decrementing `held_count`), or derive it from live
  seat rows instead of a counter.
- Run migrations automatically on container start.
- Deploy to a public URL with CI running tests + the burst script on every merge.
