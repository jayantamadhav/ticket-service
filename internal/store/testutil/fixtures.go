package testutil

import (
	"context"
	"testing"

	"github.com/jayantamadhav/ticket-service/internal/models"
	"github.com/jayantamadhav/ticket-service/internal/store"
)

// CreateTestShow creates a show with the given seat labels and a default
// per-user limit of 4, returning the created show.
func CreateTestShow(t *testing.T, s *store.Store, seatLabels []string, perUserLimit int) *models.Show {
	t.Helper()
	show, _, err := s.CreateShow(context.Background(), "test-show", seatLabels, 25000, perUserLimit)
	if err != nil {
		t.Fatalf("failed to create test show: %v", err)
	}
	return show
}
