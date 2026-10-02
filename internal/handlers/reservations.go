package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jayantamadhav/ticket-service/internal/middleware"
	"github.com/jayantamadhav/ticket-service/internal/store"
)

type ReservationHandler struct {
	store *store.Store
}

func NewReservationHandler(s *store.Store) *ReservationHandler {
	return &ReservationHandler{store: s}
}

func (h *ReservationHandler) Confirm(w http.ResponseWriter, r *http.Request) {
	reservationID := chi.URLParam(r, "id")

	userID, ok := middleware.UserID(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing identity")
		return
	}

	reservation, err := h.store.Confirm(r.Context(), reservationID, userID)
	if err != nil {
		writeReservationError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, reservation)
}

func (h *ReservationHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	reservationID := chi.URLParam(r, "id")

	userID, ok := middleware.UserID(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing identity")
		return
	}

	err := h.store.Cancel(r.Context(), reservationID, userID)
	if err != nil {
		writeReservationError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

func writeReservationError(w http.ResponseWriter, err error) {
	switch {
	case isNotFound(err):
		writeError(w, http.StatusNotFound, err.Error())
	case isForbidden(err):
		writeError(w, http.StatusForbidden, err.Error())
	case isReservationDecline(err):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
