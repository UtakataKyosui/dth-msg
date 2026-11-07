package monitor

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Config struct {
	AdminToken          string   `json:"adminToken"`
	ClientID            string   `json:"clientId"`
	ClientSecret        string   `json:"clientSecret"`
	RefreshToken        string   `json:"refreshToken"`
	DeviceName          string   `json:"deviceName"`
	MissingHours        int      `json:"missingHours"`
	SyncMinutes         int      `json:"syncMinutes"`
	PollMinutes         int      `json:"pollMinutes"`
	Targets             []Target `json:"targets"`
	PublicURL           string   `json:"publicUrl"`
	ClerkPublishableKey string   `json:"clerkPublishableKey"`
	ClerkJWTKey         string   `json:"clerkJwtKey"`
	ClerkSecretKey      string   `json:"clerkSecretKey"`
	OwnerUserID         string   `json:"ownerUserId"`
	DiscordClientID     string   `json:"discordClientId"`
	DiscordClientSecret string   `json:"discordClientSecret"`
	EnableX             bool     `json:"enableX"`
}

func (c Config) Validate() error {
	if len(c.AdminToken) < 32 {
		return errors.New("an admin token of at least 32 bytes is required")
	}
	if c.ClerkPublishableKey == "" && (c.ClientID == "" || c.ClientSecret == "" || c.RefreshToken == "") {
		return errors.New("configure Clerk or Google credentials")
	}
	if err := c.validateAuth(); err != nil {
		return err
	}
	if c.MissingHours < 1 || c.MissingHours > 8760 || c.SyncMinutes < 1 || c.SyncMinutes > 1440 || c.PollMinutes < 1 || c.PollMinutes > 1440 {
		return errors.New("invalid monitoring durations")
	}
	if len(c.Targets) > 10 {
		return errors.New("configure at most 10 SNS targets")
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
		Transport: redirectTransport{base: &http.Transport{}},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirects are not allowed")
		},
	}
}

type redirectTransport struct{ base http.RoundTripper }

func (t redirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if runtime.GOOS == "js" {
		r = r.Clone(r.Context())
		r.Header.Set("js.fetch:redirect", "error")
	}
	return t.base.RoundTrip(r)
}

type Service struct {
	Config      Config
	Store       Store
	Health      HealthClient
	HTTP        *http.Client
	Now         func() time.Time
	mu          sync.Mutex
	auth        *clerkAuth
	clerkOrigin string
}

func NewService(c Config, store Store) (*Service, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	client := HTTPClient()
	s := &Service{
		Config: c, Store: store, HTTP: client, Now: time.Now,
		Health: HealthClient{
			HTTP: client, BaseURL: "https://health.googleapis.com/v4",
			TokenURL: "https://oauth2.googleapis.com/token", ClientID: c.ClientID,
			ClientSecret: c.ClientSecret, RefreshToken: c.RefreshToken, DeviceName: c.DeviceName,
		},
	}
	if err := s.initAuth(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) Check(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.Store.Load(ctx)
	if err != nil {
		return err
	}
	now := s.Now().UTC()
	settings := s.settings(state)
	if s.Config.ClerkPublishableKey != "" && state.OwnerUserID != "" && state.OwnerUserID != s.Config.OwnerUserID {
		return errors.New("stored owner differs from configured owner")
	}
	if s.auth != nil && state.OwnerUserID == "" {
		state.OwnerUserID = s.Config.OwnerUserID
		if err := s.Store.Save(ctx, state); err != nil {
			return err
		}
	}
	if settings.DeviceName == "" || len(settings.Targets) == 0 {
		state.LastCheck, state.Eligible, state.Reason = now, false, "setup_required"
		return s.Store.Save(ctx, state)
	}
	health := s.Health
	health.DeviceName = settings.DeviceName
	var token, refresh string
	if s.Config.ClerkPublishableKey != "" {
		token, err = s.clerkHealthToken(ctx, &state)
	} else {
		token, refresh, err = s.Health.Token(ctx, state.RefreshToken)
	}
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
		observation, err = health.Observe(ctx, token, since, now)
	}
	if err != nil {
		state.LastCheck, state.Eligible, state.Reason = now, false, "health_api_unavailable"
		if saveErr := s.Store.Save(ctx, state); saveErr != nil {
			return saveErr
		}
		return errors.Join(err, s.deliver(ctx, &state))
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
	return s.deliver(ctx, &state)
}

func (s *Service) deliver(ctx context.Context, state *State) error {
	// Eligibility gates new decisions, not delivery of an already confirmed event.
	if !state.Confirmed {
		return nil
	}
	if state.Delivered == nil {
		state.Delivered = make(map[string]bool)
	}
	var failures []error
	for _, target := range s.settings(*state).Targets {
		message := state.Messages[target.ID]
		if message == "" || state.Delivered[target.ID] {
			continue
		}
		if target.Kind == "x" {
			if !s.Config.EnableX || state.XAttempts >= 3 {
				continue
			}
			token, err := s.providerToken(ctx, "oauth_x", []string{"tweet.write", "offline.access"})
			if err != nil {
				failures = append(failures, errors.New("unavailable X authorization"))
				continue
			}
			target.Token = token
			// Persist the attempt before posting, including failed/ambiguous requests.
			state.XAttempts++
			if err := s.Store.Save(ctx, *state); err != nil {
				return err
			}
		}
		if err := Deliver(ctx, s.HTTP, target, message, state.EventID); err != nil {
			failures = append(failures, errors.New(target.ID+": delivery failed"))
			continue
		}
		state.Delivered[target.ID] = true
		if err := s.Store.Save(ctx, *state); err != nil {
			return err
		}
	}
	return errors.Join(failures...)
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if s.serveConsole(w, r) {
		return
	}
	if s.servePublicAuth(w, r) {
		return
	}
	got := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
	want := sha256.Sum256([]byte(s.Config.AdminToken))
	admin := strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") && subtle.ConstantTimeCompare(got[:], want[:]) == 1
	if !admin && !s.clerkAuthorized(r) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	if !admin && r.Method != http.MethodGet && r.Header.Get("Origin") != s.Config.PublicURL {
		http.Error(w, `{"error":"invalid origin"}`, http.StatusForbidden)
		return
	}
	// Workers use this before reading an untrusted request body. This path must
	// remain storage-free so authorization cannot wait behind monitoring work.
	if r.URL.Path == "/auth/verify" {
		_, _ = io.WriteString(w, `{"ok":true}`)
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
	if s.Config.ClerkPublishableKey != "" && state.OwnerUserID != "" && state.OwnerUserID != s.Config.OwnerUserID {
		http.Error(w, `{"error":"stored owner differs from configured owner"}`, http.StatusForbidden)
		return
	}
	if s.serveIntegrations(w, r, &state) {
		return
	}
	switch {
	case r.URL.Path == "/settings" && (r.Method == http.MethodGet || r.Method == http.MethodPut):
		s.serveSettings(w, r, state)
	case r.Method == http.MethodGet && r.URL.Path == "/status":
		// Do not expose refresh tokens, message contents or target credentials.
		_ = json.NewEncoder(w).Encode(struct {
			LastHeartRate time.Time       `json:"lastHeartRate"`
			LastCheck     time.Time       `json:"lastCheck"`
			MissingSecs   int64           `json:"missingSeconds"`
			Reason        string          `json:"reason"`
			Confirmed     bool            `json:"suspectedDeath"`
			Delivered     map[string]bool `json:"delivered"`
			XAttempts     int             `json:"xAttempts"`
		}{state.LastHeartRate, state.LastCheck, int64(state.Missing / time.Second), state.Reason, state.Confirmed, state.Delivered, state.XAttempts})
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
			for _, target := range s.settings(state).Targets {
				if target.ID == id {
					found = true
					limit := messageLimit(target.Kind)
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
