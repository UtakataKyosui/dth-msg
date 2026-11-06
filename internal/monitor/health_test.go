package monitor

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestGoogleHealthPaginationAndSchema(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	h := HealthClient{BaseURL: "https://health.googleapis.com/v4", DeviceName: fixtureConfig().DeviceName}
	requests := 0
	h.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != "test-access" {
			t.Error("missing OAuth token")
		}
		q := r.URL.Query()
		if strings.HasSuffix(r.URL.Path, "/pairedDevices") {
			if q.Get("pageToken") == "" {
				return response(200, `{"pairedDevices":[],"nextPageToken":"device-page"}`), nil
			}
			return response(200, `{"pairedDevices":[{"name":"users/me/pairedDevices/test","deviceType":"TRACKER","batteryLevel":0,"batteryStatus":"Empty","lastSyncTime":"2026-11-06T11:59:00Z"}]}`), nil
		}
		if q.Get("dataSourceFamily") != "users/me/dataSourceFamilies/google-wearables" || !strings.Contains(q.Get("filter"), `physical_time < "2026-11-06T12:00:00Z"`) {
			t.Error("wrong source or time filter")
		}
		if q.Get("pageToken") == "" {
			return response(200, `{"dataPoints":[{"heartRate":{"beatsPerMinute":"65","sampleTime":{"physicalTime":"2026-11-06T11:00:00Z"}}}],"nextPageToken":"heart-page"}`), nil
		}
		return response(200, `{"dataPoints":[{"heartRate":{"beatsPerMinute":"70","sampleTime":{"physicalTime":"2026-11-06T11:30:00Z"}}}]}`), nil
	})}
	o, err := h.Observe(context.Background(), "test-access", now.Add(-24*time.Hour), now)
	if err != nil || requests != 4 || o.HeartRate != now.Add(-30*time.Minute) || o.Level == nil || *o.Level != 0 || o.Battery != "Empty" {
		t.Fatalf("incorrect observation: %+v requests=%d err=%v", o, requests, err)
	}
}

func TestGoogleHealthFailsClosed(t *testing.T) {
	now := time.Now().UTC()
	for _, body := range []string{
		`{"dataPoints":[{"heartRate":{"beatsPerMinute":"0","sampleTime":{"physicalTime":"2026-11-06T11:00:00Z"}}}]}`,
		`{"dataPoints":[{"heartRate":{"beatsPerMinute":"70","sampleTime":{"physicalTime":"2099-01-01T00:00:00Z"}}}]}`,
		`not JSON`,
	} {
		h := HealthClient{BaseURL: "https://health.googleapis.com/v4", DeviceName: fixtureConfig().DeviceName}
		h.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/pairedDevices") {
				return response(200, `{"pairedDevices":[{"name":"users/me/pairedDevices/test","deviceType":"TRACKER","batteryStatus":"High"}]}`), nil
			}
			return response(200, body), nil
		})}
		if _, err := h.Observe(context.Background(), "test-access", now.Add(-24*time.Hour), now); err == nil {
			t.Fatalf("invalid data accepted: %s", body)
		}
	}
}

func TestTargetValidation(t *testing.T) {
	for _, target := range []Target{
		{ID: "a", Kind: "discord", URL: "http://discord.com/api/webhooks/a/b"},
		{ID: "a", Kind: "discord", URL: "https://example.com/api/webhooks/a/b"},
		{ID: "a", Kind: "mastodon", URL: "https://127.0.0.1/api/v1/statuses", Token: "test"},
		{ID: "a", Kind: "unknown", URL: "https://example.com/"},
	} {
		if target.Validate() == nil {
			t.Fatalf("unsafe target accepted: %+v", target)
		}
	}
}
