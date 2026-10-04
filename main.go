package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jayantamadhav/ticket-service/internal/handlers"
	"github.com/jayantamadhav/ticket-service/internal/logging"
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

	poolConfig, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		logger.Error("invalid DATABASE_URL", "error", err)
		os.Exit(1)
	}
	// pgxpool defaults MaxConns to 4x CPU cores, which is far too small to
	// hold ~20k concurrent in-flight reservation transactions during an
	// on-sale stampede. DB_MAX_CONNS lets this be tuned per-deployment;
	// default comfortably under Postgres's own default max_connections=100
	// so the app pool alone doesn't starve other connections (migrate CLI,
	// psql, etc.) against the same database.
	poolConfig.MaxConns = 80
	if v := os.Getenv("DB_MAX_CONNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			poolConfig.MaxConns = int32(n)
		} else {
			logger.Error("invalid DB_MAX_CONNS, ignoring", "value", v)
		}
	}

	pool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		logger.Error("failed to create db pool", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	st := store.New(pool)
	showHandler := handlers.NewShowHandler(st)
	reservationHandler := handlers.NewReservationHandler(st)

	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(logging.RequestLogger(logger))
	r.Use(chimiddleware.Recoverer)

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
