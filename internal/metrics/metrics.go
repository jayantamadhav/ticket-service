package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

var (
	ReservationsConfirmed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "reservations_confirmed_total",
		Help: "Total number of seats successfully confirmed.",
	})

	ReservationsDeclined = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "reservations_declined_total",
		Help: "Total number of reservation attempts declined, by reason.",
	}, []string{"reason"})

	SeatsAvailable = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "seats_available",
		Help: "Current number of available seats per show.",
	}, []string{"show_id"})
)

func init() {
	prometheus.MustRegister(ReservationsConfirmed)
	prometheus.MustRegister(ReservationsDeclined)
	prometheus.MustRegister(SeatsAvailable)
}

// Decline reasons — keep these in sync with the error types in internal/store.
const (
	ReasonSeatTaken        = "seat_taken"
	ReasonPerUserLimit     = "per_user_limit"
	ReasonIdempotentReplay = "idempotent_replay"
	ReasonSeatNotFound     = "seat_not_found"
	ReasonShowNotFound     = "show_not_found"
	ReasonNotOwner         = "not_owner"
	ReasonAlreadyFinalized = "already_finalized"
	ReasonHoldExpired      = "hold_expired"
)
