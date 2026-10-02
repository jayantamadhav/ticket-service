package models

import "time"

type Show struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	PricePaise    int64     `json:"price_paise"`
	PerUserLimit  int       `json:"per_user_limit"`
	CreatedAt     time.Time `json:"created_at"`
}
