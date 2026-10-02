package testutil

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewTestPool connects to the test database (expects it already migrated)
// and truncates all tables before each test for a clean slate.
func NewTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://ticket:ticket@localhost:5432/ticket_service?sslmode=disable"
	}

	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("failed to connect to test db: %v", err)
	}

	_, err = pool.Exec(context.Background(), `
		TRUNCATE shows, seats, reservations, user_show_holds CASCADE
	`)
	if err != nil {
		t.Fatalf("failed to truncate test db: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
	})

	return pool
}
