package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jayantamadhav/ticket-service/internal/models"
)

// Confirm flips a held reservation to confirmed, only if:
//   - it exists and belongs to userID
//   - it's still in 'held' status (not already confirmed/cancelled)
//   - its hold hasn't expired yet
func (s *Store) Confirm(ctx context.Context, reservationID, userID string) (*models.Reservation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var r models.Reservation
	var dbUserID string
	err = tx.QueryRow(ctx, `
		SELECT id, show_id, user_id, seats, amount_paise, status, created_at
		FROM reservations WHERE id = $1 FOR UPDATE
	`, reservationID).Scan(&r.ID, &r.ShowID, &dbUserID, &r.Seats, &r.AmountPaise, &r.Status, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrReservationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup reservation: %w", err)
	}
	r.UserID = dbUserID

	if dbUserID != userID {
		return nil, ErrNotOwner
	}
	if r.Status != "held" {
		return nil, ErrAlreadyFinalized
	}

	// Check TTL hasn't passed on any of its seats (they're all held
	// together with the same held_until, set at reserve time).
	var heldUntil *time.Time
	err = tx.QueryRow(ctx, `
		SELECT held_until FROM seats WHERE reservation_id = $1 LIMIT 1
	`, reservationID).Scan(&heldUntil)
	if err != nil {
		return nil, fmt.Errorf("check held_until: %w", err)
	}
	if heldUntil == nil || heldUntil.Before(time.Now()) {
		return nil, ErrHoldExpired
	}

	_, err = tx.Exec(ctx, `UPDATE reservations SET status = 'confirmed' WHERE id = $1`, reservationID)
	if err != nil {
		return nil, fmt.Errorf("confirm reservation: %w", err)
	}
	_, err = tx.Exec(ctx, `
		UPDATE seats SET status = 'confirmed', held_until = NULL WHERE reservation_id = $1
	`, reservationID)
	if err != nil {
		return nil, fmt.Errorf("confirm seats: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	r.Status = "confirmed"
	return &r, nil
}

// Cancel releases a held OR confirmed reservation's seats back to available,
// only if the caller owns it. Decrements the user's held_count accordingly.
// Returns the show ID so callers can refresh show-scoped state (e.g. metrics)
// without a second lookup.
func (s *Store) Cancel(ctx context.Context, reservationID, userID string) (showID string, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var dbUserID, status string
	var seats []string
	err = tx.QueryRow(ctx, `
		SELECT user_id, show_id, status, seats FROM reservations WHERE id = $1 FOR UPDATE
	`, reservationID).Scan(&dbUserID, &showID, &status, &seats)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrReservationNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lookup reservation: %w", err)
	}

	if dbUserID != userID {
		return "", ErrNotOwner
	}
	if status == "cancelled" {
		return "", ErrAlreadyFinalized
	}

	_, err = tx.Exec(ctx, `UPDATE reservations SET status = 'cancelled' WHERE id = $1`, reservationID)
	if err != nil {
		return "", fmt.Errorf("cancel reservation: %w", err)
	}

	// Only release seats that still point at THIS reservation — guards
	// against ever touching a seat that's since been reassigned.
	_, err = tx.Exec(ctx, `
		UPDATE seats
		SET status = 'available', reservation_id = NULL, held_by_user_id = NULL, held_until = NULL
		WHERE reservation_id = $1
	`, reservationID)
	if err != nil {
		return "", fmt.Errorf("release seats: %w", err)
	}

	_, err = tx.Exec(ctx, `
		UPDATE user_show_holds
		SET held_count = GREATEST(held_count - $1, 0)
		WHERE show_id = $2 AND user_id = $3
	`, len(seats), showID, dbUserID)
	if err != nil {
		return "", fmt.Errorf("update user_show_holds: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit tx: %w", err)
	}
	return showID, nil
}
