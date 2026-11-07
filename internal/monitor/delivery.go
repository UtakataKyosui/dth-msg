package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
)

type Target struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	URL   string `json:"url"`
	Token string `json:"token,omitempty"`
}

func (t Target) Validate() error {
	u, err := url.Parse(t.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("target must have a valid HTTPS URL without user info or fragment")
	}
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	if host == "localhost" || strings.HasSuffix(host, ".local") || (ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast())) {
		return errors.New("target must be a public SNS endpoint")
	}
	switch t.Kind {
	case "discord":
		if host != "discord.com" || !strings.HasPrefix(u.Path, "/api/webhooks/") {
			return errors.New("invalid Discord target: require a discord.com webhook")
		}
	case "slack":
		if host != "hooks.slack.com" || !strings.HasPrefix(u.Path, "/services/") {
			return errors.New("invalid Slack target: require a hooks.slack.com webhook")
		}
	case "mastodon":
		if u.Path != "/api/v1/statuses" || t.Token == "" {
			return errors.New("invalid Mastodon target: require /api/v1/statuses and an access token")
		}
	case "x":
		if t.ID != "x" || t.URL != "https://api.x.com/2/tweets" || t.Token != "" {
			return errors.New("invalid X target: require the linked account and fixed API endpoint")
		}
	default:
		return errors.New("unsupported SNS kind")
	}
	if t.ID == "" {
		return errors.New("target ID is required")
	}
	return nil
}

func Deliver(ctx context.Context, client *http.Client, target Target, message, eventID string) error {
	payload := map[string]any{}
	switch target.Kind {
	case "discord":
		payload["content"] = message
		payload["allowed_mentions"] = map[string]any{"parse": []string{}}
	case "slack":
		payload["text"] = message
	case "mastodon":
		payload["status"] = message
	case "x":
		payload["text"] = message
	default:
		return errors.New("unsupported SNS kind")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid SNS endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	if target.Kind == "mastodon" {
		req.Header.Set("Authorization", "Bearer "+target.Token)
		req.Header.Set("Idempotency-Key", eventID+"-"+target.ID)
	}
	if target.Kind == "x" {
		req.Header.Set("Authorization", "Bearer "+target.Token)
	}
	// Go's js/wasm transport delegates redirects to fetch.
	if runtime.GOOS == "js" {
		req.Header.Set("js.fetch:redirect", "error")
	}
	res, err := client.Do(req)
	if err != nil {
		return errors.New("SNS transport failed")
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("SNS HTTP %d", res.StatusCode)
	}
	return nil
}

func messageLimit(kind string) int {
	if kind == "mastodon" {
		return 500
	}
	// Conservative limit: X counts some Unicode characters as two units.
	if kind == "x" {
		return 140
	}
	return 2000
}
