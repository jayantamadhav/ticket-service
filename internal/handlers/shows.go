package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jayantamadhav/ticket-service/internal/logging"
	"github.com/jayantamadhav/ticket-service/internal/metrics"
	"github.com/jayantamadhav/ticket-service/internal/middleware"
	"github.com/jayantamadhav/ticket-service/internal/store"
)

type ShowHandler struct {
	store *store.Store
}

func NewShowHandler(s *store.Store) *ShowHandler {
	return &ShowHandler{store: s}
}

type createShowRequest struct {
	Name         string   `json:"name"`
	Seats        []string `json:"seats"`
	PricePaise   int64    `json:"price_paise"`
	PerUserLimit int      `json:"per_user_limit"` // optional, defaults to 4
}

func (h *ShowHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req createShowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" || len(req.Seats) == 0 || req.PricePaise < 0 {
		writeError(w, http.StatusBadRequest, "name, seats, and non-negative price_paise are required")
		return
	}
	if req.PerUserLimit <= 0 {
		req.PerUserLimit = 4
	}

	show, seats, err := h.store.CreateShow(r.Context(), req.Name, req.Seats, req.PricePaise, req.PerUserLimit)
	if err != nil {
		logging.FromContext(r.Context()).Error("create show failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create show")
		return
	}

	logging.FromContext(r.Context()).Info("show created", "show_id", show.ID, "seats", len(seats))
	metrics.SeatsAvailable.WithLabelValues(show.ID).Set(float64(len(seats)))

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":             show.ID,
		"name":           show.Name,
		"price_paise":    show.PricePaise,
		"per_user_limit": show.PerUserLimit,
		"created_at":     show.CreatedAt,
		"seats":          seats,
	})
}

type reserveRequest struct {
	Seats          []string `json:"seats"`
	IdempotencyKey string   `json:"idempotency_key"`
}

func (h *ShowHandler) Reserve(w http.ResponseWriter, r *http.Request) {
	showID := chi.URLParam(r, "id")

	userID, ok := middleware.UserID(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing identity")
		return
	}

	var req reserveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Idempotency key can also come from a header — accept either, header takes precedence.
	if headerKey := r.Header.Get("Idempotency-Key"); headerKey != "" {
		req.IdempotencyKey = headerKey
	}
	if req.IdempotencyKey == "" || len(req.Seats) == 0 {
		writeError(w, http.StatusBadRequest, "seats and idempotency_key are required")
		return
	}

	log := logging.FromContext(r.Context()).With("show_id", showID, "user_id", userID, "seats", req.Seats)

	reservation, isReplay, err := h.store.Reserve(r.Context(), showID, userID, req.IdempotencyKey, req.Seats)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrSeatUnavailable):
			metrics.ReservationsDeclined.WithLabelValues(metrics.ReasonSeatTaken).Inc()
			log.Info("reservation declined", "reason", metrics.ReasonSeatTaken)
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, store.ErrPerUserLimitExceeded):
			metrics.ReservationsDeclined.WithLabelValues(metrics.ReasonPerUserLimit).Inc()
			log.Info("reservation declined", "reason", metrics.ReasonPerUserLimit)
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, store.ErrIdempotencyConflict):
			metrics.ReservationsDeclined.WithLabelValues(metrics.ReasonIdempotentReplay).Inc()
			log.Info("reservation declined", "reason", "idempotency_conflict")
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, store.ErrSeatNotFound):
			metrics.ReservationsDeclined.WithLabelValues(metrics.ReasonSeatNotFound).Inc()
			log.Info("reservation declined", "reason", metrics.ReasonSeatNotFound)
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, store.ErrShowNotFound):
			metrics.ReservationsDeclined.WithLabelValues(metrics.ReasonShowNotFound).Inc()
			log.Info("reservation declined", "reason", metrics.ReasonShowNotFound)
			writeError(w, http.StatusNotFound, err.Error())
		default:
			log.Error("reservation failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	if isReplay {
		metrics.ReservationsDeclined.WithLabelValues(metrics.ReasonIdempotentReplay).Inc()
		log.Info("idempotent replay", "reservation_id", reservation.ID)
	} else {
		metrics.ReservationsConfirmed.Add(float64(len(reservation.Seats)))
		log.Info("reservation confirmed", "reservation_id", reservation.ID)
		h.refreshSeatsAvailableMetric(r.Context(), showID)
	}

	writeJSON(w, http.StatusCreated, reservation)
}

func (h *ShowHandler) Get(w http.ResponseWriter, r *http.Request) {
	showID := chi.URLParam(r, "id")

	state, err := h.store.GetShowState(r.Context(), showID)
	if err != nil {
		switch {
		case isNotFound(err):
			writeError(w, http.StatusNotFound, err.Error())
		default:
			logging.FromContext(r.Context()).Error("get show state failed", "show_id", showID, "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	writeJSON(w, http.StatusOK, state)
}

func (h *ShowHandler) refreshSeatsAvailableMetric(ctx context.Context, showID string) {
	refreshSeatsAvailableMetric(ctx, h.store, showID)
}

// refreshSeatsAvailableMetric recomputes and sets the seats_available gauge
// for a show. Best-effort: a failure here shouldn't fail the request that
// triggered it, just leaves the metric briefly stale.
func refreshSeatsAvailableMetric(ctx context.Context, st *store.Store, showID string) {
	count, err := st.AvailableSeats(ctx, showID)
	if err != nil {
		logging.FromContext(ctx).Error("failed to refresh seats_available metric", "show_id", showID, "error", err)
		return
	}
	metrics.SeatsAvailable.WithLabelValues(showID).Set(float64(count))
}
