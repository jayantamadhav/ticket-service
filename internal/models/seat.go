package models

import "time"

type SeatStatus string

const (
	SeatAvailable SeatStatus = "available"
	SeatHeld      SeatStatus = "held"
	SeatConfirmed SeatStatus = "confirmed"
)

type Seat struct {
	ID            string     `json:"id"`
	ShowID        string     `json:"show_id"`
	SeatLabel     string     `json:"seat_label"`
	Status        SeatStatus `json:"status"`
	ReservationID *string    `json:"reservation_id,omitempty"`
	HeldByUserID  *string    `json:"held_by_user_id,omitempty"`
	HeldUntil     *time.Time `json:"held_until,omitempty"`
}
