package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jayantamadhav/ticket-service/internal/store"
	"github.com/jayantamadhav/ticket-service/internal/store/testutil"
)

func TestConfirm_HappyPath(t *testing.T) {
	pool := testutil.NewTestPool(t)
	s := store.New(pool)
	show := testutil.CreateTestShow(t, s, []string{"A1"}, 4)

	res, _, err := s.Reserve(context.Background(), show.ID, "alice", "key-1", []string{"A1"})
	if err != nil {
		t.Fatalf("Reserve failed: %v", err)
	}

	confirmed, err := s.Confirm(context.Background(), res.ID, "alice")
	if err != nil {
		t.Fatalf("Confirm failed: %v", err)
	}
	if confirmed.Status != "confirmed" {
		t.Errorf("expected status 'confirmed', got %q", confirmed.Status)
	}

	state, err := s.GetShowState(context.Background(), show.ID)
	if err != nil {
		t.Fatalf("GetShowState failed: %v", err)
	}
	if state.Counts.Confirmed != 1 || state.Counts.Held != 0 {
		t.Errorf("expected 1 confirmed, 0 held, got confirmed=%d held=%d", state.Counts.Confirmed, state.Counts.Held)
	}
}

func TestConfirm_NotOwner(t *testing.T) {
	pool := testutil.NewTestPool(t)
	s := store.New(pool)
	show := testutil.CreateTestShow(t, s, []string{"A1"}, 4)

	res, _, err := s.Reserve(context.Background(), show.ID, "alice", "key-1", []string{"A1"})
	if err != nil {
		t.Fatalf("Reserve failed: %v", err)
	}

	_, err = s.Confirm(context.Background(), res.ID, "bob")
	if !errors.Is(err, store.ErrNotOwner) {
		t.Errorf("expected ErrNotOwner, got %v", err)
	}
}

func TestConfirm_AlreadyFinalized(t *testing.T) {
	pool := testutil.NewTestPool(t)
	s := store.New(pool)
	show := testutil.CreateTestShow(t, s, []string{"A1"}, 4)

	res, _, err := s.Reserve(context.Background(), show.ID, "alice", "key-1", []string{"A1"})
	if err != nil {
		t.Fatalf("Reserve failed: %v", err)
	}
	if _, err := s.Confirm(context.Background(), res.ID, "alice"); err != nil {
		t.Fatalf("first Confirm failed: %v", err)
	}

	_, err = s.Confirm(context.Background(), res.ID, "alice")
	if !errors.Is(err, store.ErrAlreadyFinalized) {
		t.Errorf("expected ErrAlreadyFinalized, got %v", err)
	}
}

func TestConfirm_ReservationNotFound(t *testing.T) {
	pool := testutil.NewTestPool(t)
	s := store.New(pool)

	_, err := s.Confirm(context.Background(), "00000000-0000-0000-0000-000000000000", "alice")
	if !errors.Is(err, store.ErrReservationNotFound) {
		t.Errorf("expected ErrReservationNotFound, got %v", err)
	}
}

func TestCancel_HeldReservation(t *testing.T) {
	pool := testutil.NewTestPool(t)
	s := store.New(pool)
	show := testutil.CreateTestShow(t, s, []string{"A1"}, 4)

	res, _, err := s.Reserve(context.Background(), show.ID, "alice", "key-1", []string{"A1"})
	if err != nil {
		t.Fatalf("Reserve failed: %v", err)
	}

	if _, err := s.Cancel(context.Background(), res.ID, "alice"); err != nil {
		t.Fatalf("Cancel failed: %v", err)
	}

	state, err := s.GetShowState(context.Background(), show.ID)
	if err != nil {
		t.Fatalf("GetShowState failed: %v", err)
	}
	if state.Counts.Available != 1 {
		t.Errorf("expected seat available after cancel, got counts=%+v", state.Counts)
	}
}

func TestCancel_ConfirmedReservation(t *testing.T) {
	pool := testutil.NewTestPool(t)
	s := store.New(pool)
	show := testutil.CreateTestShow(t, s, []string{"A1"}, 4)

	res, _, err := s.Reserve(context.Background(), show.ID, "alice", "key-1", []string{"A1"})
	if err != nil {
		t.Fatalf("Reserve failed: %v", err)
	}
	if _, err := s.Confirm(context.Background(), res.ID, "alice"); err != nil {
		t.Fatalf("Confirm failed: %v", err)
	}

	if _, err := s.Cancel(context.Background(), res.ID, "alice"); err != nil {
		t.Fatalf("Cancel of confirmed reservation failed: %v", err)
	}

	state, err := s.GetShowState(context.Background(), show.ID)
	if err != nil {
		t.Fatalf("GetShowState failed: %v", err)
	}
	if state.Counts.Available != 1 {
		t.Errorf("expected seat available after cancelling confirmed reservation, got counts=%+v", state.Counts)
	}
}

func TestCancel_NotOwner(t *testing.T) {
	pool := testutil.NewTestPool(t)
	s := store.New(pool)
	show := testutil.CreateTestShow(t, s, []string{"A1"}, 4)

	res, _, err := s.Reserve(context.Background(), show.ID, "alice", "key-1", []string{"A1"})
	if err != nil {
		t.Fatalf("Reserve failed: %v", err)
	}

	_, err = s.Cancel(context.Background(), res.ID, "bob")
	if !errors.Is(err, store.ErrNotOwner) {
		t.Errorf("expected ErrNotOwner, got %v", err)
	}

	// confirm bob's failed attempt didn't release alice's seat
	state, err := s.GetShowState(context.Background(), show.ID)
	if err != nil {
		t.Fatalf("GetShowState failed: %v", err)
	}
	if state.Counts.Held != 1 {
		t.Errorf("expected seat still held after bob's rejected cancel attempt, got counts=%+v", state.Counts)
	}
}

func TestCancel_SeatRebookableAfterRelease(t *testing.T) {
	pool := testutil.NewTestPool(t)
	s := store.New(pool)
	show := testutil.CreateTestShow(t, s, []string{"A1"}, 4)

	res, _, err := s.Reserve(context.Background(), show.ID, "alice", "key-1", []string{"A1"})
	if err != nil {
		t.Fatalf("Reserve failed: %v", err)
	}
	if _, err := s.Cancel(context.Background(), res.ID, "alice"); err != nil {
		t.Fatalf("Cancel failed: %v", err)
	}

	// bob should now be able to claim the released seat
	_, _, err = s.Reserve(context.Background(), show.ID, "bob", "key-bob-1", []string{"A1"})
	if err != nil {
		t.Errorf("expected bob to successfully claim released seat, got error: %v", err)
	}
}
