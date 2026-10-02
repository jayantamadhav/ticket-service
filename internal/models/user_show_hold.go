package models

type UserShowHold struct {
	ShowID    string `json:"show_id"`
	UserID    string `json:"user_id"`
	HeldCount int    `json:"held_count"`
}
