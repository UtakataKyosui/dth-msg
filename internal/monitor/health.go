package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type HealthClient struct {
	HTTP         *http.Client
	BaseURL      string
	TokenURL     string
	ClientID     string
	ClientSecret string
	RefreshToken string
	DeviceName   string
}

func (h HealthClient) Token(ctx context.Context, refresh string) (string, string, error) {
	if refresh == "" {
		refresh = h.RefreshToken
	}
	form := url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refresh},
		"client_id": {h.ClientID}, "client_secret": {h.ClientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", errors.New("invalid token endpoint")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := h.json(req, &result); err != nil {
		return "", "", fmt.Errorf("token refresh: %w", err)
	}
	if result.AccessToken == "" {
		return "", "", errors.New("token response missing access token")
	}
	return result.AccessToken, result.RefreshToken, nil
}

func (h HealthClient) json(req *http.Request, result any) error {
	if runtime.GOOS == "js" {
		req.Header.Set("js.fetch:redirect", "error")
	}
	res, err := h.HTTP.Do(req)
	if err != nil {
		// Transport errors may contain credential-bearing URLs.
		return errors.New("upstream transport failed")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("upstream HTTP %d", res.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(result); err != nil {
		return errors.New("invalid upstream JSON")
	}
	return nil
}

func (h HealthClient) get(ctx context.Context, token, path string, query url.Values, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.BaseURL+path+"?"+query.Encode(), nil)
	if err != nil {
		return errors.New("invalid health endpoint")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return h.json(req, result)
}

func (h HealthClient) Observe(ctx context.Context, token string, since, now time.Time) (Observation, error) {
	var o Observation
	found := false
	query := url.Values{"pageSize": {"100"}}
	for page := 0; ; page++ {
		if page >= 100 {
			return o, errors.New("too many device pages")
		}
		var result struct {
			Devices []struct {
				Name    string    `json:"name"`
				Type    string    `json:"deviceType"`
				Battery string    `json:"batteryStatus"`
				Level   *int      `json:"batteryLevel"`
				Sync    time.Time `json:"lastSyncTime"`
			} `json:"pairedDevices"`
			Next string `json:"nextPageToken"`
		}
		if err := h.get(ctx, token, "/users/me/pairedDevices", query, &result); err != nil {
			return o, err
		}
		for _, d := range result.Devices {
			if d.Name == h.DeviceName && d.Type == "TRACKER" {
				o.Sync, o.Battery, o.Level = d.Sync, d.Battery, d.Level
				found = true
			}
		}
		if result.Next == "" {
			break
		}
		query.Set("pageToken", result.Next)
	}
	if !found {
		return o, errors.New("configured Fitbit tracker not found")
	}
	query = url.Values{
		"pageSize":         {"10000"},
		"dataSourceFamily": {"users/me/dataSourceFamilies/google-wearables"},
		"filter": {fmt.Sprintf(`heart_rate.sample_time.physical_time >= "%s" AND heart_rate.sample_time.physical_time < "%s"`,
			since.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano))},
	}
	for page := 0; ; page++ {
		if page >= 100 {
			return o, errors.New("too many heart rate pages")
		}
		var result struct {
			Points []struct {
				Heart struct {
					Time struct {
						Physical time.Time `json:"physicalTime"`
					} `json:"sampleTime"`
					BPM string `json:"beatsPerMinute"`
				} `json:"heartRate"`
			} `json:"dataPoints"`
			Next string `json:"nextPageToken"`
		}
		if err := h.get(ctx, token, "/users/me/dataTypes/heart-rate/dataPoints", query, &result); err != nil {
			return o, err
		}
		for _, point := range result.Points {
			bpm, err := strconv.Atoi(point.Heart.BPM)
			t := point.Heart.Time.Physical
			if err != nil || bpm < 1 || bpm > 300 || t.IsZero() || t.After(now) || t.Before(since) {
				return o, errors.New("invalid heart rate sample")
			}
			if t.After(o.HeartRate) {
				o.HeartRate = t
			}
		}
		if result.Next == "" {
			return o, nil
		}
		query.Set("pageToken", result.Next)
	}
}
