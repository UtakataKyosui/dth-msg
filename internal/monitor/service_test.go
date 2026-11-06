package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type memoryStore struct {
	state State
	fail  bool
}

func (m *memoryStore) Load(context.Context) (State, error) {
	// Match persistent storage's value semantics.
	body, _ := json.Marshal(m.state)
	var s State
	_ = json.Unmarshal(body, &s)
	return s, nil
}

func (m *memoryStore) Save(_ context.Context, s State) error {
	if m.fail {
		return errors.New("storage failed")
	}
	m.state = s
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixtureConfig() Config {
	return Config{
		AdminToken: strings.Repeat("test-only-", 4), ClientID: "test-client", ClientSecret: "test-secret",
		RefreshToken: "test-refresh", DeviceName: "users/me/pairedDevices/test",
		MissingHours: 1, SyncMinutes: 120, PollMinutes: 15,
		Targets: []Target{{ID: "discord", Kind: "discord", URL: "https://discord.com/api/webhooks/test/fixture"}},
	}
}

func response(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestCheckDeliveryPersistenceAndFailures(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	store := &memoryStore{state: State{
		LastHeartRate: now.Add(-2 * time.Hour), LastCheck: now.Add(-10 * time.Minute),
		Missing: 50 * time.Minute, Eligible: true, Messages: map[string]string{"discord": "goodbye"},
	}}
	s, err := NewService(fixtureConfig(), store)
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return now }
	sends, upstreamFailure, deliveryFailure := 0, false, true
	s.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "oauth2.googleapis.com":
			return response(200, `{"access_token":"test-access","refresh_token":"test-rotated"}`), nil
		case "health.googleapis.com":
			if upstreamFailure {
				return response(429, `{}`), nil
			}
			if strings.HasSuffix(r.URL.Path, "/pairedDevices") {
				return response(200, `{"pairedDevices":[{"name":"users/me/pairedDevices/test","deviceType":"TRACKER","batteryStatus":"High","lastSyncTime":"`+now.Format(time.RFC3339)+`"}]}`), nil
			}
			return response(200, `{"dataPoints":[]}`), nil
		case "discord.com":
			sends++
			var payload map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || string(payload["content"]) != `"goodbye"` || string(payload["allowed_mentions"]) != `{"parse":[]}` {
				t.Errorf("bad payload: %v", payload)
			}
			if deliveryFailure {
				return response(500, `{}`), nil
			}
			return response(204, ""), nil
		}
		t.Fatalf("unexpected URL: %s", r.URL)
		return nil, nil
	})
	if err := s.Check(context.Background()); err == nil || !store.state.Confirmed || store.state.Delivered["discord"] {
		t.Fatal("failed delivery must remain pending")
	}
	deliveryFailure = false
	now = now.Add(5 * time.Minute)
	if err := s.Check(context.Background()); err != nil || !store.state.Delivered["discord"] || store.state.RefreshToken != "test-rotated" {
		t.Fatalf("retry failed: %v %+v", err, store.state)
	}
	// Reconstruct the service to simulate a restart, preserving acknowledgements.
	restarted, _ := NewService(fixtureConfig(), store)
	restarted.HTTP, restarted.Health, restarted.Now = s.HTTP, s.Health, s.Now
	if err := restarted.Check(context.Background()); err != nil || sends != 2 {
		t.Fatalf("successful message resent: sends=%d err=%v", sends, err)
	}
	upstreamFailure = true
	before := store.state.Missing
	now = now.Add(5 * time.Minute)
	if err := s.Check(context.Background()); err == nil || store.state.Eligible || store.state.Missing != before {
		t.Fatal("API failure did not pause detection")
	}
}

func TestRegistrationAuthenticationAndStatusPrivacy(t *testing.T) {
	store := &memoryStore{}
	s, _ := NewService(fixtureConfig(), store)
	request := func(method, path, body, auth string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	auth := "Bearer " + s.Config.AdminToken
	if got := request("PUT", "/messages", `{"discord":"goodbye"}`, "").Code; got != 401 {
		t.Fatalf("unauthenticated registration: %d", got)
	}
	for _, body := range []string{`{"unknown":"x"}`, `{"discord":""}`, `{"discord":"x"} {}`, `{}`, `{"discord":"` + strings.Repeat("x", 2001) + `"}`} {
		if got := request("PUT", "/messages", body, auth).Code; got != 400 {
			t.Fatalf("invalid registration accepted: %d", got)
		}
	}
	if got := request("PUT", "/messages", `{"discord":"goodbye"}`, auth).Code; got != 200 || store.state.Messages["discord"] != "goodbye" {
		t.Fatalf("registration failed: %d", got)
	}
	store.state.RefreshToken = "private-refresh-fixture"
	status := request("GET", "/status", "", auth)
	if status.Code != 200 || strings.Contains(status.Body.String(), "private") || strings.Contains(status.Body.String(), "goodbye") {
		t.Fatal("status leaked private data")
	}
	store.state.Confirmed = true
	if got := request("PUT", "/messages", `{"discord":"changed"}`, auth).Code; got != 409 {
		t.Fatalf("messages must be locked: %d", got)
	}
}

func TestStorageFailurePreventsSend(t *testing.T) {
	store := &memoryStore{fail: true}
	s, _ := NewService(fixtureConfig(), store)
	s.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "discord.com" {
			t.Fatal("sent despite failed persistence")
		}
		return response(200, `{"access_token":"test-access","refresh_token":"test-rotated"}`), nil
	})
	if s.Check(context.Background()) == nil {
		t.Fatal("storage failure ignored")
	}
}

func TestConfirmedDeliverySurvivesMonitoringPause(t *testing.T) {
	for _, reason := range []string{"battery_empty", "sync_stale", "health_api_unavailable"} {
		t.Run(reason, func(t *testing.T) {
			now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
			store := &memoryStore{state: State{
				Confirmed: true, EventID: "existing-event", LastHeartRate: now.Add(-2 * time.Hour),
				LastCheck: now.Add(-5 * time.Minute), Missing: time.Hour,
				Messages: map[string]string{"discord": "farewell"},
			}}
			s, _ := NewService(fixtureConfig(), store)
			s.Now = func() time.Time { return now }
			sends := 0
			s.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "oauth2.googleapis.com" {
					return response(200, `{"access_token":"test-access"}`), nil
				}
				if r.URL.Host == "discord.com" {
					sends++
					return response(204, ""), nil
				}
				if reason == "health_api_unavailable" {
					return response(503, `{}`), nil
				}
				if strings.HasSuffix(r.URL.Path, "/pairedDevices") {
					battery, sync := "Empty", now
					if reason == "sync_stale" {
						battery, sync = "High", now.Add(-3*time.Hour)
					}
					return response(200, `{"pairedDevices":[{"name":"users/me/pairedDevices/test","deviceType":"TRACKER","batteryStatus":"`+battery+`","lastSyncTime":"`+sync.Format(time.RFC3339)+`"}]}`), nil
				}
				return response(200, `{"dataPoints":[]}`), nil
			})
			err := s.Check(context.Background())
			if (err != nil) != (reason == "health_api_unavailable") {
				t.Fatalf("unexpected error: %v", err)
			}
			if sends != 1 || !store.state.Delivered["discord"] || store.state.Eligible || store.state.Reason != reason || store.state.Missing != time.Hour {
				t.Fatalf("pending delivery not preserved during pause: sends=%d state=%+v", sends, store.state)
			}
		})
	}
}
