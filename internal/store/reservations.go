package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jayantamadhav/ticket-service/internal/models"
)

const HoldTTL = 45 * time.Second

var (
	ErrSeatUnavailable      = errors.New("seat unavailable")
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
func (s *Store) Reserve(ctx context.Context, showID, userID, idempotencyKey string, seatLabels []string) (*models.Reservation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	existing, err := checkIdempotency(ctx, tx, idempotencyKey, seatLabels)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}

	var pricePaise int64
	err = tx.QueryRow(ctx, `SELECT price_paise FROM shows WHERE id = $1`, showID).Scan(&pricePaise)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrShowNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup show: %w", err)
	}

	sortedSeats := append([]string(nil), seatLabels...)
	sort.Strings(sortedSeats)

	// Lock candidate rows in deterministic order. A seat counts as
	// available if status='available', OR status='held' but its TTL
	// has already passed (lazy expiry at read time).
	rows, err := tx.Query(ctx, `
		SELECT seat_label, status, held_until
		FROM seats
		WHERE show_id = $1 AND seat_label = ANY($2)
		ORDER BY seat_label
		FOR UPDATE
	`, showID, sortedSeats)
	if err != nil {
		return nil, fmt.Errorf("lock seats: %w", err)
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
			return nil, fmt.Errorf("scan seat: %w", err)
		}
		found[label] = seatRow{status: status, heldUntil: heldUntil}
	}
	rows.Close()

	now := time.Now()
	for _, label := range sortedSeats {
		row, ok := found[label]
		if !ok {
			return nil, ErrSeatUnavailable
		}
		isAvailable := row.status == "available"
		isExpiredHold := row.status == "held" && row.heldUntil != nil && row.heldUntil.Before(now)
		if !isAvailable && !isExpiredHold {
			return nil, ErrSeatUnavailable
		}
	}

	requestedCount := len(sortedSeats)
	var newHeldCount int
	err = tx.QueryRow(ctx, `
		INSERT INTO user_show_holds (show_id, user_id, held_count)
		VALUES ($1, $2, $3)
		ON CONFLICT (show_id, user_id)
		DO UPDATE SET held_count = user_show_holds.held_count + $3
		WHERE user_show_holds.held_count + $3 <= (
			SELECT per_user_limit FROM shows WHERE id = $1
		)
		RETURNING held_count
	`, showID, userID, requestedCount).Scan(&newHeldCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPerUserLimitExceeded
	}
	if err != nil {
		return nil, fmt.Errorf("update user_show_holds: %w", err)
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
		return nil, fmt.Errorf("insert reservation: %w", err)
	}

	_, err = tx.Exec(ctx, `
		UPDATE seats
		SET status = 'held', reservation_id = $1, held_by_user_id = $2, held_until = $3
		WHERE show_id = $4 AND seat_label = ANY($5)
	`, reservation.ID, userID, heldUntil, showID, sortedSeats)
	if err != nil {
		return nil, fmt.Errorf("hold seats: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return &reservation, nil
}

func checkIdempotency(ctx context.Context, tx pgx.Tx, idempotencyKey string, requestedSeats []string) (*models.Reservation, error) {
	var r models.Reservation
	err := tx.QueryRow(ctx, `
		SELECT id, show_id, user_id, seats, amount_paise, status, created_at
		FROM reservations
		WHERE idempotency_key = $1
	`, idempotencyKey).Scan(
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
