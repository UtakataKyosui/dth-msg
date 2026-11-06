package monitor

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestSNSPayloads(t *testing.T) {
	for _, kind := range []string{"discord", "slack", "mastodon"} {
		t.Run(kind, func(t *testing.T) {
			target := Target{ID: "test-id", Kind: kind, URL: "https://example.com/api/v1/statuses", Token: "test-credential"}
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				field := map[string]string{"discord": "content", "slack": "text", "mastodon": "status"}[kind]
				if payload[field] != "farewell" || r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
					t.Fatal("incorrect SNS request")
				}
				if kind == "mastodon" && (r.Header.Get("Idempotency-Key") != "event-test-id" || r.Header.Get("Authorization") == "") {
					t.Fatal("missing Mastodon authorization or idempotency key")
				}
				return response(200, `{}`), nil
			})}
			if err := Deliver(context.Background(), client, target, "farewell", "event"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHTTPRedirectsRefused(t *testing.T) {
	client := HTTPClient()
	if client.CheckRedirect(nil, nil) == nil {
		t.Fatal("credential-bearing redirects must be rejected")
	}
}
