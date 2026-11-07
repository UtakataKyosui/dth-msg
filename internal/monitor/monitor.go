package monitor

import (
	"context"
	"errors"
	"time"
)

type State struct {
	GoogleAccountID string            `json:"googleAccountId,omitempty"`
	OwnerUserID     string            `json:"ownerUserId,omitempty"`
	DiscordOAuth    *OAuthPending     `json:"discordOAuth,omitempty"`
	XAttempts       int               `json:"xAttempts,omitempty"`
	Settings        *Settings         `json:"settings,omitempty"`
	LastHeartRate   time.Time         `json:"lastHeartRate"`
	LastCheck       time.Time         `json:"lastCheck"`
	Missing         time.Duration     `json:"missing"`
	Eligible        bool              `json:"eligible"`
	Reason          string            `json:"reason"`
	Confirmed       bool              `json:"confirmed"`
	EventID         string            `json:"eventId,omitempty"`
	Delivered       map[string]bool   `json:"delivered"`
	Messages        map[string]string `json:"messages"`
	RefreshToken    string            `json:"refreshToken,omitempty"`
}

type Store interface {
	Load(context.Context) (State, error)
	Save(context.Context, State) error
}

type Observation struct {
	HeartRate time.Time
	Sync      time.Time
	Battery   string
	Level     *int
}

type Policy struct {
	MissingFor time.Duration
	MaxSyncAge time.Duration
	MaxPollGap time.Duration
}

func (p Policy) Validate() error {
	if p.MissingFor <= 0 || p.MaxSyncAge <= 0 || p.MaxPollGap <= 0 {
		return errors.New("monitoring durations must be positive")
	}
	return nil
}

func (p Policy) Observe(s *State, o Observation, now time.Time) {
	eligible, reason := true, "monitoring"
	switch {
	case o.Battery == "Empty" || (o.Level != nil && *o.Level == 0):
		eligible, reason = false, "battery_empty"
	case o.Battery != "High" && o.Battery != "Medium" && o.Battery != "Low":
		eligible, reason = false, "battery_unknown"
	case o.Sync.IsZero() || o.Sync.After(now) || now.Sub(o.Sync) > p.MaxSyncAge:
		eligible, reason = false, "sync_stale"
	}

	if o.HeartRate.After(s.LastHeartRate) && !o.HeartRate.After(now) {
		s.LastHeartRate = o.HeartRate
		s.Missing = 0
		s.Confirmed = false
		s.EventID = ""
		s.Delivered = nil
		s.XAttempts = 0
		// A newly received sample establishes a new baseline, not an inferred outage.
		s.Eligible = false
	}
	elapsed := now.Sub(s.LastCheck)
	if !s.LastHeartRate.IsZero() && eligible && s.Eligible && elapsed > 0 && elapsed <= p.MaxPollGap {
		s.Missing += elapsed
	}
	s.LastCheck, s.Eligible, s.Reason = now, eligible, reason
	if s.LastHeartRate.IsZero() {
		s.Reason = "waiting_for_first_heart_rate"
	}
	if !s.LastHeartRate.IsZero() && eligible && s.Missing >= p.MissingFor && !s.Confirmed {
		s.Confirmed = true
		s.EventID = now.UTC().Format("20060102T150405.000000000")
		s.Delivered = make(map[string]bool)
	}
}
