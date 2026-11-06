package monitor

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Config struct {
	AdminToken   string   `json:"adminToken"`
	ClientID     string   `json:"clientId"`
	ClientSecret string   `json:"clientSecret"`
	RefreshToken string   `json:"refreshToken"`
	DeviceName   string   `json:"deviceName"`
	MissingHours int      `json:"missingHours"`
	SyncMinutes  int      `json:"syncMinutes"`
	PollMinutes  int      `json:"pollMinutes"`
	Targets      []Target `json:"targets"`
}

func (c Config) Validate() error {
	if len(c.AdminToken) < 32 || c.ClientID == "" || c.ClientSecret == "" || c.RefreshToken == "" || c.DeviceName == "" {
		return errors.New("Google credentials, device name and an admin token of at least 32 bytes are required")
	}
	if c.MissingHours < 1 || c.MissingHours > 8760 || c.SyncMinutes < 1 || c.SyncMinutes > 1440 || c.PollMinutes < 1 || c.PollMinutes > 1440 {
		return errors.New("invalid monitoring durations")
	}
	if len(c.Targets) == 0 || len(c.Targets) > 10 {
		return errors.New("configure between 1 and 10 SNS targets")
	}
	ids := map[string]bool{}
	for _, target := range c.Targets {
		if err := target.Validate(); err != nil {
			return err
		}
		if ids[target.ID] {
			return errors.New("target IDs must be unique")
		}
		ids[target.ID] = true
	}
	return nil
}

func HTTPClient() *http.Client {
	return &http.Client{
		Timeout:   20 * time.Second,
		Transport: &http.Transport{},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirects are not allowed")
		},
	}
}

type Service struct {
	Config Config
	Store  Store
	Health HealthClient
	HTTP   *http.Client
	Now    func() time.Time
	mu     sync.Mutex
}

func NewService(c Config, store Store) (*Service, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	client := HTTPClient()
	return &Service{
		Config: c, Store: store, HTTP: client, Now: time.Now,
		Health: HealthClient{
			HTTP: client, BaseURL: "https://health.googleapis.com/v4",
			TokenURL: "https://oauth2.googleapis.com/token", ClientID: c.ClientID,
			ClientSecret: c.ClientSecret, RefreshToken: c.RefreshToken, DeviceName: c.DeviceName,
		},
	}, nil
}

func (s *Service) Check(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.Store.Load(ctx)
	if err != nil {
		return err
	}
	now := s.Now().UTC()
	token, refresh, err := s.Health.Token(ctx, state.RefreshToken)
	if err == nil && refresh != "" {
		state.RefreshToken = refresh
		if err = s.Store.Save(ctx, state); err != nil {
			return err
		}
	}
	var observation Observation
	if err == nil {
		since := now.Add(-24 * time.Hour)
		// Include overlap for delayed uploads without growing the window indefinitely.
		if !state.LastCheck.IsZero() && state.LastCheck.After(since) {
			since = state.LastCheck.Add(-time.Hour)
		}
		observation, err = s.Health.Observe(ctx, token, since, now)
	}
	if err != nil {
		state.LastCheck, state.Eligible, state.Reason = now, false, "health_api_unavailable"
		if saveErr := s.Store.Save(ctx, state); saveErr != nil {
			return saveErr
		}
		return err
	}
	policy := Policy{
		MissingFor: time.Duration(s.Config.MissingHours) * time.Hour,
		MaxSyncAge: time.Duration(s.Config.SyncMinutes) * time.Minute,
		MaxPollGap: time.Duration(s.Config.PollMinutes) * time.Minute,
	}
	policy.Observe(&state, observation, now)
	if err := s.Store.Save(ctx, state); err != nil {
		return err
	}
	// Empty/stale batteries and API failures also suspend pending message retries.
	if !state.Confirmed || !state.Eligible {
		return nil
	}
	var failures []error
	for _, target := range s.Config.Targets {
		message := state.Messages[target.ID]
		if message == "" || state.Delivered[target.ID] {
			continue
		}
		if err := Deliver(ctx, s.HTTP, target, message, state.EventID); err != nil {
			failures = append(failures, errors.New(target.ID+": delivery failed"))
			continue
		}
		state.Delivered[target.ID] = true
		if err := s.Store.Save(ctx, state); err != nil {
			return err
		}
	}
	return errors.Join(failures...)
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	got := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
	want := sha256.Sum256([]byte(s.Config.AdminToken))
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/check" {
		if err := s.Check(r.Context()); err != nil {
			http.Error(w, `{"error":"check failed; monitoring paused or delivery pending"}`, http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.Store.Load(r.Context())
	if err != nil {
		http.Error(w, `{"error":"storage unavailable"}`, http.StatusInternalServerError)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/status":
		// Do not expose refresh tokens, message contents or target credentials.
		_ = json.NewEncoder(w).Encode(struct {
			LastHeartRate time.Time       `json:"lastHeartRate"`
			LastCheck     time.Time       `json:"lastCheck"`
			MissingSecs   int64           `json:"missingSeconds"`
			Reason        string          `json:"reason"`
			Confirmed     bool            `json:"suspectedDeath"`
			Delivered     map[string]bool `json:"delivered"`
		}{state.LastHeartRate, state.LastCheck, int64(state.Missing / time.Second), state.Reason, state.Confirmed, state.Delivered})
	case r.Method == http.MethodPut && r.URL.Path == "/messages":
		if state.Confirmed {
			http.Error(w, `{"error":"messages are locked during a confirmed event"}`, http.StatusConflict)
			return
		}
		var messages map[string]string
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
		if err := decoder.Decode(&messages); err != nil || len(messages) == 0 {
			http.Error(w, `{"error":"expected a nonempty target-ID to message JSON object"}`, http.StatusBadRequest)
			return
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
			return
		}
		for id, message := range messages {
			found := false
			for _, target := range s.Config.Targets {
				if target.ID == id {
					found = true
					limit := 2000
					if target.Kind == "mastodon" {
						limit = 500
					}
					if strings.TrimSpace(message) == "" || !utf8.ValidString(message) || utf8.RuneCountInString(message) > limit {
						found = false
					}
				}
			}
			if !found {
				http.Error(w, `{"error":"unknown target or invalid message length"}`, http.StatusBadRequest)
				return
			}
		}
		state.Messages = messages
		if err := s.Store.Save(r.Context(), state); err != nil {
			http.Error(w, `{"error":"storage unavailable"}`, http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}
