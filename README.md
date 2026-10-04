# ticket-service

JSON HTTP API for selling assigned seats to a show. No seat is ever
double-sold, no user exceeds their per-show seat limit, and retried requests
never double-charge.

See [WRITEUP.md](WRITEUP.md) for design rationale.

## Stack

- Go 1.27, [chi](https://github.com/go-chi/chi)
- PostgreSQL
- Prometheus client for `/metrics`

## Running locally

```bash
cp env.example .env
docker compose -f docker-compose.yml -f docker-compose.dev.yml up --build
```

Brings up Postgres and the API (hot-reloading via `air`) on `:8080`. Migrations aren't run
automatically yet — apply them once before first use:

```bash
brew install golang-migrate   # or see golang-migrate/migrate on GitHub
migrate -path migrations \
  -database "postgres://ticket:ticket@localhost:5432/ticket_service?sslmode=disable" \
  up
```

Without Docker: start Postgres yourself, apply migrations as above, then
`export DATABASE_URL=...` and `go run .`.

## API

Money is always integer paise.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | `/shows` | none | Create a show with its seat map |
| GET | `/shows/{id}` | none | Per-seat status + counts |
| POST | `/shows/{id}/reserve` | Bearer token | Hold seat(s) |
| POST | `/reservations/{id}/confirm` | Bearer token (owner) | Confirm a hold |
| POST | `/reservations/{id}/cancel` | Bearer token (owner) | Release a hold/booking |
| GET | `/healthz` | none | Liveness |
| GET | `/readyz` | none | Readiness, fails closed if DB is down |
| GET | `/metrics` | none | Prometheus metrics |

Identity comes from `Authorization: Bearer <user-id>`, never the body. A
Postman collection is in [.postman/](.postman/).

```
POST /shows/{id}/reserve
Authorization: Bearer alice

{ "seats": ["A12"], "idempotency_key": "alice-req-1" }
```

- `idempotency_key` can be a header (`Idempotency-Key`) instead of a body field.
- Multi-seat requests are all-or-nothing.
- Holds expire after 45s if not confirmed or cancelled.

## Observability

- Structured JSON logs (`log/slog`) with a `request_id` on every line for a
  given request.
- `/metrics`: `reservations_confirmed_total`, `reservations_declined_total{reason}`
  (`seat_taken`, `per_user_limit`, `idempotent_replay`, ...), `seats_available{show_id}`.
- `/healthz` is pure liveness; `/readyz` pings the DB with a 2s timeout.

## Load testing

```bash
./burst.sh                              # against http://localhost:8080
./burst.sh https://your-deploy.example
```

`cmd/burst` creates a fresh show and fires a concurrent stampede at it: a
hot-seat storm (many users racing one seat), a per-user-limit stress test,
and concurrent idempotent retries. It prints the outcome distribution, checks
the hot seat resolved to exactly one winner, and reconciles
`available + held + confirmed == total`. Exits non-zero on any 5xx or
reconciliation failure.

To approximate the spec's ~20,000-concurrent-reservation stampede:

```bash
go run ./cmd/burst -base-url http://localhost:8080 \
  -seats 1000 -hot-seat-users 500 -spread-users 19200 -retry-users 50
```

The bulk of that load runs through a bounded worker pool (`-max-in-flight`,
default 2000) so it doesn't need 20,000 simultaneous OS threads/connections
on the machine generating it — only the hot-seat storm always fires fully
simultaneous and uncapped, since that's the actual race being tested. If you
push `-max-in-flight` high and see `other` (transport-level failures) in the
output, that's the client machine's own fd/port limits, not the server —
lower `-max-in-flight` or raise `ulimit -n`. On the server side, `DB_MAX_CONNS`
(env var, default 80) sizes the Postgres pool for the load you're sending.

## Tests

```bash
go test ./...
```

Store tests spin up an ephemeral Postgres and include a hot-seat contention
test and an all-or-nothing partial-request test.

## Known gaps

- Migrations aren't applied automatically on startup.
- No live deployment URL yet.
