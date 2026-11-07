package monitor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestConsoleSettingsPersistenceAndDeviceReset(t *testing.T) {
	c := fixtureConfig()
	c.DeviceName, c.Targets = "", nil
	store := &memoryStore{}
	s, err := NewService(c, store)
	if err != nil {
		t.Fatal(err)
	}
	request := func(service *Service, method, path, body string, auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if auth {
			r.Header.Set("Authorization", "Bearer "+c.AdminToken)
		}
		w := httptest.NewRecorder()
		service.ServeHTTP(w, r)
		return w
	}
	if err := s.Check(context.Background()); err != nil || store.state.Reason != "setup_required" {
		t.Fatalf("setup check: %v", err)
	}
	for _, path := range []string{"/", "/console.js"} {
		w := request(s, "GET", path, "", false)
		if w.Code != 200 || w.Header().Get("Content-Security-Policy") == "" || strings.Contains(w.Body.String(), c.AdminToken) {
			t.Fatalf("invalid public asset: %s", path)
		}
	}
	settings := Settings{DeviceName: fixtureConfig().DeviceName, Targets: fixtureConfig().Targets, Messages: map[string]string{"discord": "saved message"}}
	body, _ := json.Marshal(settings)
	for _, method := range []string{"GET", "PUT"} {
		if request(s, method, "/settings", string(body), false).Code != 401 {
			t.Fatal("unauthorized settings access")
		}
	}
	if w := request(s, "PUT", "/settings", string(body), true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	restarted, _ := NewService(c, store)
	w := request(restarted, "GET", "/settings", "", true)
	var saved Settings
	if json.Unmarshal(w.Body.Bytes(), &saved) != nil || saved.DeviceName != settings.DeviceName || saved.Messages["discord"] != "saved message" {
		t.Fatal("settings not persisted")
	}
	store.state.LastHeartRate = time.Now().Add(-time.Hour)
	store.state.Missing, store.state.Eligible = time.Hour, true
	if request(s, "PUT", "/settings", string(body), true).Code != 200 || store.state.Missing != time.Hour {
		t.Fatal("same device reset baseline")
	}
	store.state.Confirmed = true
	if request(s, "PUT", "/settings", string(body), true).Code != 409 {
		t.Fatal("confirmed settings not locked")
	}
	store.state.Confirmed = false
	store.state.RefreshToken = "rotated"
	settings.DeviceName += "-new"
	body, _ = json.Marshal(settings)
	if request(s, "PUT", "/settings", string(body), true).Code != 200 || !store.state.LastHeartRate.IsZero() || store.state.Missing != 0 || store.state.Eligible || store.state.RefreshToken != "rotated" {
		t.Fatal("device change did not reset safely")
	}
	store.fail = true
	settings.DeviceName += "-unsaved"
	body, _ = json.Marshal(settings)
	if request(s, "PUT", "/settings", string(body), true).Code != 500 || store.state.Settings.DeviceName == settings.DeviceName {
		t.Fatal("failed write changed settings")
	}
}

func TestInvalidSettingsDoNotReplaceConfiguration(t *testing.T) {
	s, _ := NewService(fixtureConfig(), &memoryStore{})
	valid := Settings{DeviceName: fixtureConfig().DeviceName, Targets: fixtureConfig().Targets}
	for _, mutate := range []func(*Settings){
		func(v *Settings) { v.DeviceName = "" },
		func(v *Settings) { v.Targets = nil },
		func(v *Settings) { v.Targets = append(v.Targets, v.Targets[0]) },
		func(v *Settings) { v.Targets = []Target{{ID: "bad", Kind: "discord", URL: "http://localhost/"}} },
		func(v *Settings) { v.Messages = map[string]string{"unknown": "message"} },
		func(v *Settings) { v.Messages = map[string]string{"discord": strings.Repeat("x", 2001)} },
	} {
		v := valid
		mutate(&v)
		body, _ := json.Marshal(v)
		r := httptest.NewRequest("PUT", "/settings", strings.NewReader(string(body)))
		r.Header.Set("Authorization", "Bearer "+s.Config.AdminToken)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("invalid settings accepted: %s", body)
		}
	}
}

func TestDeliveryUsesSavedTargets(t *testing.T) {
	c := fixtureConfig()
	store := &memoryStore{state: State{Confirmed: true, EventID: "event", Messages: map[string]string{"saved": "hello"}, Settings: &Settings{DeviceName: c.DeviceName, Targets: []Target{{ID: "saved", Kind: "slack", URL: "https://hooks.slack.com/services/saved"}}}}}
	s, _ := NewService(c, store)
	sends := 0
	s.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://hooks.slack.com/services/saved" {
			t.Fatal("used environment target")
		}
		sends++
		return response(200, ""), nil
	})
	if err := s.deliver(context.Background(), &store.state); err != nil || sends != 1 {
		t.Fatalf("saved target delivery: %v", err)
	}
}
