package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jayantamadhav/ticket-service/internal/logging"
	"github.com/jayantamadhav/ticket-service/internal/models"
)

const HoldTTL = 45 * time.Second

// lockWaitTimeout bounds how long a single Reserve call will wait to acquire
// its seat-row locks. Under extreme contention (many concurrent requests
// colliding on overlapping seats), the queue to acquire a FOR UPDATE lock
// can otherwise grow long enough to exceed a client's own request timeout —
// the client gives up, its context is canceled mid-query, and that surfaced
// as an unhandled 500 instead of a clean decline. A short statement_timeout
// here makes "the seat is contended right now" fail fast as the same
// ErrSeatUnavailable decline a genuinely-taken seat produces, rather than
// hanging for tens of seconds and erroring out.
const lockWaitTimeout = 3 * time.Second

// pgQueryCanceled is Postgres's SQLSTATE for a statement terminated by
// statement_timeout (or an explicit cancellation).
const pgQueryCanceled = "57014"

var (
	ErrSeatUnavailable      = errors.New("seat unavailable")
	ErrSeatNotFound         = errors.New("seat not found")
	ErrPerUserLimitExceeded = errors.New("per-user limit exceeded")
	ErrIdempotencyConflict  = errors.New("idempotency key reused with different request")
	ErrShowNotFound         = errors.New("show not found")
	ErrReservationNotFound  = errors.New("reservation not found")
	ErrNotOwner             = errors.New("not the owner of this reservation")
	ErrAlreadyFinalized     = errors.New("reservation already confirmed or cancelled")
	ErrHoldExpired          = errors.New("hold has expired")
)

// Reserve attempts to HOLD all requested seats for a user, atomically,
// honoring idempotency and the per-user limit. All-or-nothing: if any
// requested seat isn't available (including treating expired holds as
// available), the whole request is declined and nothing is held.
func (s *Store) Reserve(ctx context.Context, showID, userID, idempotencyKey string, seatLabels []string) (*models.Reservation, bool, error) {
	start := time.Now()
	tx, err := s.pool.Begin(ctx)
	acquireMs := time.Since(start).Milliseconds()
	if err != nil {
		return nil, false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	// Diagnostic timing split: acquire_ms is time spent waiting for a free
	// pgxpool connection (pool exhaustion shows up here, never in Postgres's
	// own logs, since no statement has been sent yet at that point).
	// total_ms is the full call including every query in the transaction.
	// Logged unconditionally via defer so every return path (success or any
	// decline/error) reports it the same way.
	defer func() {
		logging.FromContext(ctx).Info("reserve timing",
			"show_id", showID, "user_id", userID,
			"acquire_ms", acquireMs, "total_ms", time.Since(start).Milliseconds())
	}()

	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", lockWaitTimeout.Milliseconds())); err != nil {
		return nil, false, fmt.Errorf("set statement_timeout: %w", err)
	}

	existing, err := checkIdempotency(ctx, tx, userID, idempotencyKey, seatLabels)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return existing, true, nil
	}

	var pricePaise int64
	err = tx.QueryRow(ctx, `SELECT price_paise FROM shows WHERE id = $1`, showID).Scan(&pricePaise)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, ErrShowNotFound
	}
	if err != nil {
		return nil, false, fmt.Errorf("lookup show: %w", err)
	}

	sortedSeats := append([]string(nil), seatLabels...)
	sort.Strings(sortedSeats)

	rows, err := tx.Query(ctx, `
		SELECT seat_label, status, held_until
		FROM seats
		WHERE show_id = $1 AND seat_label = ANY($2)
		ORDER BY seat_label
		FOR UPDATE
	`, showID, sortedSeats)
	if err != nil {
		if isLockTimeout(err) {
			// Couldn't acquire the row lock(s) within lockWaitTimeout —
			// something else is actively contending for one of these same
			// seats. Treat it the same as "seat taken": a clean, fast
			// decline rather than hanging until a client gives up.
			return nil, false, ErrSeatUnavailable
		}
		return nil, false, fmt.Errorf("lock seats: %w", err)
	}

	type seatRow struct {
		status    string
		heldUntil *time.Time
	}
	found := map[string]seatRow{}
	for rows.Next() {
		var label, status string
		var heldUntil *time.Time
		if err := rows.Scan(&label, &status, &heldUntil); err != nil {
			rows.Close()
			return nil, false, fmt.Errorf("scan seat: %w", err)
		}
		found[label] = seatRow{status: status, heldUntil: heldUntil}
	}
	rows.Close()

	now := time.Now()
	for _, label := range sortedSeats {
		row, ok := found[label]
		if !ok {
			return nil, false, ErrSeatNotFound
		}
		isAvailable := row.status == "available"
		isExpiredHold := row.status == "held" && row.heldUntil != nil && row.heldUntil.Before(now)
		if !isAvailable && !isExpiredHold {
			return nil, false, ErrSeatUnavailable
		}
	}

	requestedCount := len(sortedSeats)
	var newHeldCount int
	_, err = tx.Exec(ctx, `
		INSERT INTO user_show_holds (show_id, user_id, held_count)
		VALUES ($1, $2, 0)
		ON CONFLICT (show_id, user_id) DO NOTHING
	`, showID, userID)
	if err != nil {
		return nil, false, fmt.Errorf("ensure user_show_holds row: %w", err)
	}

	err = tx.QueryRow(ctx, `
		UPDATE user_show_holds
		SET held_count = held_count + $3
		WHERE show_id = $1 AND user_id = $2
		  AND held_count + $3 <= (SELECT per_user_limit FROM shows WHERE id = $1)
		RETURNING held_count
	`, showID, userID, requestedCount).Scan(&newHeldCount)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, ErrPerUserLimitExceeded
	}
	if err != nil {
		return nil, false, fmt.Errorf("update user_show_holds: %w", err)
	}

	amountPaise := pricePaise * int64(requestedCount)
	heldUntil := now.Add(HoldTTL)

	var reservation models.Reservation
	err = tx.QueryRow(ctx, `
		INSERT INTO reservations (show_id, user_id, seats, amount_paise, status, idempotency_key)
		VALUES ($1, $2, $3, $4, 'held', $5)
		RETURNING id, show_id, user_id, seats, amount_paise, status, created_at
	`, showID, userID, sortedSeats, amountPaise, idempotencyKey).Scan(
		&reservation.ID, &reservation.ShowID, &reservation.UserID,
		&reservation.Seats, &reservation.AmountPaise, &reservation.Status, &reservation.CreatedAt,
	)
	if err != nil {
		return nil, false, fmt.Errorf("insert reservation: %w", err)
	}

	_, err = tx.Exec(ctx, `
		UPDATE seats
		SET status = 'held', reservation_id = $1, held_by_user_id = $2, held_until = $3
		WHERE show_id = $4 AND seat_label = ANY($5)
	`, reservation.ID, userID, heldUntil, showID, sortedSeats)
	if err != nil {
		return nil, false, fmt.Errorf("hold seats: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit tx: %w", err)
	}

	return &reservation, false, nil
}

// isLockTimeout reports whether err is a Postgres statement_timeout
// cancellation (SQLSTATE 57014) — the signal that lockWaitTimeout tripped
// while waiting on a FOR UPDATE row lock.
func isLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgQueryCanceled
}

func checkIdempotency(ctx context.Context, tx pgx.Tx, userID, idempotencyKey string, requestedSeats []string) (*models.Reservation, error) {
	var r models.Reservation
	err := tx.QueryRow(ctx, `
		SELECT id, show_id, user_id, seats, amount_paise, status, created_at
		FROM reservations
		WHERE idempotency_key = $1 AND user_id = $2
	`, idempotencyKey, userID).Scan(
		&r.ID, &r.ShowID, &r.UserID, &r.Seats, &r.AmountPaise, &r.Status, &r.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("check idempotency: %w", err)
	}
	if !sameSeats(r.Seats, requestedSeats) {
		return nil, ErrIdempotencyConflict
	}
	return &r, nil
}

func sameSeats(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aSorted := append([]string(nil), a...)
	bSorted := append([]string(nil), b...)
	sort.Strings(aSorted)
	sort.Strings(bSorted)
	for i := range aSorted {
		if aSorted[i] != bSorted[i] {
			return false
		}
	}
	return true
}
