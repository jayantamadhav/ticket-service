package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jayantamadhav/ticket-service/internal/models"
)

// CreateShow inserts a show and all its seats in one transaction.
// If any seat insert fails (e.g. duplicate labels in the request),
// the whole thing rolls back — you never end up with a half-created show.
func (s *Store) CreateShow(ctx context.Context, name string, seatLabels []string, pricePaise int64, perUserLimit int) (*models.Show, []models.Seat, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) // no-op if committed

	var show models.Show
	err = tx.QueryRow(ctx, `
		INSERT INTO shows (name, price_paise, per_user_limit)
		VALUES ($1, $2, $3)
		RETURNING id, name, price_paise, per_user_limit, created_at
	`, name, pricePaise, perUserLimit).Scan(
		&show.ID, &show.Name, &show.PricePaise, &show.PerUserLimit, &show.CreatedAt,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("insert show: %w", err)
	}

	seats := make([]models.Seat, 0, len(seatLabels))
	for _, label := range seatLabels {
		var seat models.Seat
		err = tx.QueryRow(ctx, `
			INSERT INTO seats (show_id, seat_label, status)
			VALUES ($1, $2, 'available')
			RETURNING id, show_id, seat_label, status
		`, show.ID, label).Scan(&seat.ID, &seat.ShowID, &seat.SeatLabel, &seat.Status)
		if err != nil {
			return nil, nil, fmt.Errorf("insert seat %s: %w", label, err)
		}
		seats = append(seats, seat)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("commit tx: %w", err)
	}

	return &show, seats, nil
}

type ShowState struct {
	ShowID string        `json:"show_id"`
	Name   string        `json:"name"`
	Seats  []SeatSummary `json:"seats"`
	Counts SeatCounts    `json:"counts"`
}

type SeatSummary struct {
	Label  string `json:"label"`
	Status string `json:"status"`
}

type SeatCounts struct {
	Available int `json:"available"`
	Held      int `json:"held"`
	Confirmed int `json:"confirmed"`
	Total     int `json:"total"`
}

func (s *Store) GetShowState(ctx context.Context, showID string) (*ShowState, error) {
	var name string
	err := s.pool.QueryRow(ctx, `SELECT name FROM shows WHERE id = $1`, showID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrShowNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup show: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT seat_label, status, held_until FROM seats WHERE show_id = $1 ORDER BY seat_label
	`, showID)
	if err != nil {
		return nil, fmt.Errorf("query seats: %w", err)
	}
	defer rows.Close()

	state := &ShowState{ShowID: showID, Name: name}
	now := time.Now()
	for rows.Next() {
		var label, status string
		var heldUntil *time.Time
		if err := rows.Scan(&label, &status, &heldUntil); err != nil {
			return nil, fmt.Errorf("scan seat: %w", err)
		}
		// Lazy expiry on read: a held seat past its TTL displays as
		// available, matching what Reserve's claim check already allows.
		if status == "held" && heldUntil != nil && heldUntil.Before(now) {
			status = "available"
		}
		state.Seats = append(state.Seats, SeatSummary{Label: label, Status: status})
		switch status {
		case "available":
			state.Counts.Available++
		case "held":
			state.Counts.Held++
		case "confirmed":
			state.Counts.Confirmed++
		}
	}
	state.Counts.Total = state.Counts.Available + state.Counts.Held + state.Counts.Confirmed

	return state, nil
}
