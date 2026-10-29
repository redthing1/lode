package model

import "errors"

// Retention controls cleanup; changing it does not itself remove anything.
type Retention struct {
	Versions    int `json:"versions"`
	HistoryDays int `json:"history_days"`
}

func (r Retention) Validate() error {
	if r.Versions < 1 || r.Versions > 10000 || r.HistoryDays < 1 || r.HistoryDays > 36500 {
		return errors.New("keep 1–10000 versions per branch and 1–36500 days of history")
	}
	return nil
}

// CleanupPreview bounds examples independently of the total affected history.
type CleanupPreview struct {
	Token          string    `json:"token"`
	Policy         Retention `json:"policy"`
	Retained       int       `json:"retained"`
	Expire         int       `json:"expire"`
	Forget         int       `json:"forget"`
	ExpireExamples []Version `json:"expire_examples"`
	ForgetExamples []Version `json:"forget_examples"`
}

type History struct {
	Versions []Version `json:"versions"`
	Next     string    `json:"next,omitempty"`
	Boundary string    `json:"boundary,omitempty"`
}
