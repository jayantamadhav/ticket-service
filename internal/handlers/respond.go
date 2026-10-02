package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jayantamadhav/ticket-service/internal/store"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func isDecline(err error) bool {
	return errors.Is(err, store.ErrSeatUnavailable) ||
		errors.Is(err, store.ErrPerUserLimitExceeded) ||
		errors.Is(err, store.ErrIdempotencyConflict)
}

func isNotFound(err error) bool {
	return errors.Is(err, store.ErrShowNotFound) || errors.Is(err, store.ErrReservationNotFound) || errors.Is(err, store.ErrSeatNotFound)
}

func isForbidden(err error) bool {
	return errors.Is(err, store.ErrNotOwner)
}

func isReservationDecline(err error) bool {
	return errors.Is(err, store.ErrAlreadyFinalized) ||
		errors.Is(err, store.ErrHoldExpired)
}
