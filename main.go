package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jayantamadhav/ticket-service/internal/handlers"
	"github.com/jayantamadhav/ticket-service/internal/middleware"
	"github.com/jayantamadhav/ticket-service/internal/store"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		logger.Error("DATABASE_URL not set")
		os.Exit(1)
	}

	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		logger.Error("failed to create db pool", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	st := store.New(pool)
	showHandler := handlers.NewShowHandler(st)
	reservationHandler := handlers.NewReservationHandler(st)

	r := chi.NewRouter()

	r.Handle("/metrics", promhttp.Handler())
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			logger.Error("readiness check failed", "error", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"status": "db unreachable"})
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
	})

	r.Post("/shows", showHandler.Create)
	r.Get("/shows/{id}", showHandler.Get)

	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireAuth)
		r.Post("/shows/{id}/reserve", showHandler.Reserve)
		r.Post("/reservations/{id}/confirm", reservationHandler.Confirm)
		r.Post("/reservations/{id}/cancel", reservationHandler.Cancel)
	})

	logger.Info("server starting", "port", 8080)
	if err := http.ListenAndServe(":8080", r); err != nil {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}
