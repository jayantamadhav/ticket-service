package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jayantamadhav/ticket-service/internal/store"
	"github.com/jayantamadhav/ticket-service/internal/store/testutil"
)

func TestReserve_HappyPath(t *testing.T) {
	pool := testutil.NewTestPool(t)
	s := store.New(pool)
	show := testutil.CreateTestShow(t, s, []string{"A1", "A2"}, 4)

	res, err := s.Reserve(context.Background(), show.ID, "alice", "key-1", []string{"A1"})
	if err != nil {
		t.Fatalf("Reserve failed: %v", err)
	}
	if res.Status != "held" {
		t.Errorf("expected status 'held', got %q", res.Status)
	}
	if res.UserID != "alice" {
		t.Errorf("expected user_id 'alice', got %q", res.UserID)
	}
	if len(res.Seats) != 1 || res.Seats[0] != "A1" {
		t.Errorf("expected seats [A1], got %v", res.Seats)
	}
	if res.AmountPaise != 25000 {
		t.Errorf("expected amount 25000, got %d", res.AmountPaise)
	}
}

func TestReserve_IdempotentRetry(t *testing.T) {
	pool := testutil.NewTestPool(t)
	s := store.New(pool)
	show := testutil.CreateTestShow(t, s, []string{"A1"}, 4)

	first, err := s.Reserve(context.Background(), show.ID, "alice", "key-1", []string{"A1"})
	if err != nil {
		t.Fatalf("first Reserve failed: %v", err)
	}

	second, err := s.Reserve(context.Background(), show.ID, "alice", "key-1", []string{"A1"})
	if err != nil {
		t.Fatalf("retry Reserve failed: %v", err)
	}

	if first.ID != second.ID {
		t.Errorf("expected same reservation_id on retry, got %s and %s", first.ID, second.ID)
	}
}

func TestReserve_IdempotencyConflict(t *testing.T) {
	pool := testutil.NewTestPool(t)
	s := store.New(pool)
	show := testutil.CreateTestShow(t, s, []string{"A1", "A2"}, 4)

	_, err := s.Reserve(context.Background(), show.ID, "alice", "key-1", []string{"A1"})
	if err != nil {
		t.Fatalf("first Reserve failed: %v", err)
	}

	_, err = s.Reserve(context.Background(), show.ID, "alice", "key-1", []string{"A2"})
	if !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Errorf("expected ErrIdempotencyConflict, got %v", err)
	}
}

func TestReserve_SeatAlreadyHeld(t *testing.T) {
	pool := testutil.NewTestPool(t)
	s := store.New(pool)
	show := testutil.CreateTestShow(t, s, []string{"A1"}, 4)

	_, err := s.Reserve(context.Background(), show.ID, "alice", "key-1", []string{"A1"})
	if err != nil {
		t.Fatalf("alice's Reserve failed: %v", err)
	}

	_, err = s.Reserve(context.Background(), show.ID, "bob", "key-2", []string{"A1"})
	if !errors.Is(err, store.ErrSeatUnavailable) {
		t.Errorf("expected ErrSeatUnavailable, got %v", err)
	}
}

func TestReserve_SeatNotFound(t *testing.T) {
	pool := testutil.NewTestPool(t)
	s := store.New(pool)
	show := testutil.CreateTestShow(t, s, []string{"A1"}, 4)

	_, err := s.Reserve(context.Background(), show.ID, "alice", "key-1", []string{"Z99"})
	if !errors.Is(err, store.ErrSeatNotFound) {
		t.Errorf("expected ErrSeatNotFound, got %v", err)
	}
}

func TestReserve_PartialRequestAllOrNothing(t *testing.T) {
	pool := testutil.NewTestPool(t)
	s := store.New(pool)
	show := testutil.CreateTestShow(t, s, []string{"A1", "A2"}, 4)

	_, err := s.Reserve(context.Background(), show.ID, "alice", "key-1", []string{"A1"})
	if err != nil {
		t.Fatalf("alice's Reserve failed: %v", err)
	}

	// bob wants A1 (taken) and A2 (free) — should decline the whole request
	_, err = s.Reserve(context.Background(), show.ID, "bob", "key-2", []string{"A1", "A2"})
	if !errors.Is(err, store.ErrSeatUnavailable) {
		t.Errorf("expected ErrSeatUnavailable (all-or-nothing decline), got %v", err)
	}

	// confirm A2 is STILL available — bob's partial failure didn't leak a hold on it
	state, err := s.GetShowState(context.Background(), show.ID)
	if err != nil {
		t.Fatalf("GetShowState failed: %v", err)
	}
	for _, seat := range state.Seats {
		if seat.Label == "A2" && seat.Status != "available" {
			t.Errorf("expected A2 still available after bob's declined partial request, got %s", seat.Status)
		}
	}
}

// TestReserve_PerUserLimitEnforced_FirstRequestExceedsLimit guards against
// a regression where a user's very FIRST reservation request, if it alone
// requests more seats than per_user_limit, could bypass the limit check
// (the INSERT ... ON CONFLICT DO UPDATE guard only applied on conflict,
// not on the initial insert).
func TestReserve_PerUserLimitEnforced_FirstRequestExceedsLimit(t *testing.T) {
	pool := testutil.NewTestPool(t)
	s := store.New(pool)
	show := testutil.CreateTestShow(t, s, []string{"A1", "A2", "A3", "A4", "A5"}, 4)

	// alice's FIRST EVER request for this show asks for 5 seats against a limit of 4
	_, err := s.Reserve(context.Background(), show.ID, "alice", "key-1",
		[]string{"A1", "A2", "A3", "A4", "A5"})
	if !errors.Is(err, store.ErrPerUserLimitExceeded) {
		t.Errorf("expected ErrPerUserLimitExceeded on first over-limit request, got %v", err)
	}

	// confirm nothing was partially held — all 5 seats should still be available
	state, err := s.GetShowState(context.Background(), show.ID)
	if err != nil {
		t.Fatalf("GetShowState failed: %v", err)
	}
	if state.Counts.Available != 5 {
		t.Errorf("expected all 5 seats still available after declined over-limit request, got counts=%+v", state.Counts)
	}
}

// TestReserve_HotSeatContention is the core correctness test: fire many
// concurrent goroutines at the SAME seat and assert exactly one wins.
func TestReserve_HotSeatContention(t *testing.T) {
	pool := testutil.NewTestPool(t)
	s := store.New(pool)
	show := testutil.CreateTestShow(t, s, []string{"A1"}, 4)

	const numAttackers = 100
	var wg sync.WaitGroup
	results := make(chan error, numAttackers)

	for i := 0; i < numAttackers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			userID := "user-" + string(rune('A'+n%26)) + string(rune('0'+n/26))
			idemKey := "key-" + userID
			_, err := s.Reserve(context.Background(), show.ID, userID, idemKey, []string{"A1"})
			results <- err
		}(i)
	}

	wg.Wait()
	close(results)

	successCount := 0
	declineCount := 0
	for err := range results {
		if err == nil {
			successCount++
		} else if errors.Is(err, store.ErrSeatUnavailable) {
			declineCount++
		} else {
			t.Errorf("unexpected error (should be nil or ErrSeatUnavailable): %v", err)
		}
	}

	if successCount != 1 {
		t.Errorf("expected exactly 1 winner, got %d", successCount)
	}
	if declineCount != numAttackers-1 {
		t.Errorf("expected %d declines, got %d", numAttackers-1, declineCount)
	}

	// Reconciliation: confirm exactly one seat is held, rest available (there's only 1 seat total here)
	state, err := s.GetShowState(context.Background(), show.ID)
	if err != nil {
		t.Fatalf("GetShowState failed: %v", err)
	}
	if state.Counts.Held != 1 {
		t.Errorf("expected 1 held seat, got %d", state.Counts.Held)
	}
	if state.Counts.Total != 1 {
		t.Errorf("expected total 1, got %d", state.Counts.Total)
	}
}

func TestReserve_PerUserLimitEnforced(t *testing.T) {
	pool := testutil.NewTestPool(t)
	s := store.New(pool)
	show := testutil.CreateTestShow(t, s, []string{"A1", "A2", "A3", "A4", "A5"}, 4)

	for i, seat := range []string{"A1", "A2", "A3", "A4"} {
		_, err := s.Reserve(context.Background(), show.ID, "alice", "key-"+seat, []string{seat})
		if err != nil {
			t.Fatalf("Reserve %d (%s) failed: %v", i, seat, err)
		}
	}

	_, err := s.Reserve(context.Background(), show.ID, "alice", "key-A5", []string{"A5"})
	if !errors.Is(err, store.ErrPerUserLimitExceeded) {
		t.Errorf("expected ErrPerUserLimitExceeded, got %v", err)
	}
}

// TestReserve_IdempotencyKeyScopedPerUser guards against a regression where
// two different users reusing the same idempotency key string (e.g. "key-1")
// could leak one user's reservation to another. The key must be scoped to
// (user_id, idempotency_key), never globally unique on its own.
func TestReserve_IdempotencyKeyScopedPerUser(t *testing.T) {
	pool := testutil.NewTestPool(t)
	s := store.New(pool)
	show := testutil.CreateTestShow(t, s, []string{"A1", "A2"}, 4)

	aliceRes, err := s.Reserve(context.Background(), show.ID, "alice", "shared-key", []string{"A1"})
	if err != nil {
		t.Fatalf("alice's Reserve failed: %v", err)
	}

	bobRes, err := s.Reserve(context.Background(), show.ID, "bob", "shared-key", []string{"A2"})
	if err != nil {
		t.Fatalf("bob's Reserve failed: %v", err)
	}

	if aliceRes.ID == bobRes.ID {
		t.Fatalf("expected distinct reservations for alice and bob, got the same ID: %s", aliceRes.ID)
	}
	if bobRes.UserID != "bob" {
		t.Errorf("expected bob's reservation to belong to bob, got user_id=%s", bobRes.UserID)
	}
	if len(bobRes.Seats) != 1 || bobRes.Seats[0] != "A2" {
		t.Errorf("expected bob's reservation to hold A2, got %v", bobRes.Seats)
	}
}
