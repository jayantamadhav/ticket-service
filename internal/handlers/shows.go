package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
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
		writeError(w, http.StatusInternalServerError, "failed to create show")
		return
	}

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

	reservation, err := h.store.Reserve(r.Context(), showID, userID, req.IdempotencyKey, req.Seats)
	if err != nil {
		switch {
		case isDecline(err):
			writeError(w, http.StatusConflict, err.Error())
		case isNotFound(err):
			writeError(w, http.StatusNotFound, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	status := http.StatusCreated
	writeJSON(w, status, reservation)
}

func (h *ShowHandler) Get(w http.ResponseWriter, r *http.Request) {
	showID := chi.URLParam(r, "id")

	state, err := h.store.GetShowState(r.Context(), showID)
	if err != nil {
		switch {
		case isNotFound(err):
			writeError(w, http.StatusNotFound, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	writeJSON(w, http.StatusOK, state)
}
