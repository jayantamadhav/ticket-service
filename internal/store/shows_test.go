package store_test

import (
	"context"
	"testing"

	"github.com/jayantamadhav/ticket-service/internal/store"
	"github.com/jayantamadhav/ticket-service/internal/store/testutil"
)

func TestCreateShow(t *testing.T) {
	pool := testutil.NewTestPool(t)
	s := store.New(pool)

	show, seats, err := s.CreateShow(context.Background(), "friday-night", []string{"A1", "A2", "A3"}, 25000, 4)
	if err != nil {
		t.Fatalf("CreateShow failed: %v", err)
	}

	if show.Name != "friday-night" {
		t.Errorf("expected name 'friday-night', got %q", show.Name)
	}
	if show.PricePaise != 25000 {
		t.Errorf("expected price 25000, got %d", show.PricePaise)
	}
	if show.PerUserLimit != 4 {
		t.Errorf("expected per_user_limit 4, got %d", show.PerUserLimit)
	}
	if len(seats) != 3 {
		t.Fatalf("expected 3 seats, got %d", len(seats))
	}
	for _, seat := range seats {
		if seat.Status != "available" {
			t.Errorf("expected seat %s to be available, got %s", seat.SeatLabel, seat.Status)
		}
	}
}
