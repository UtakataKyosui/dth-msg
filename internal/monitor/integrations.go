package monitor

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type OAuthPending struct {
	StateHash   string    `json:"stateHash"`
	BrowserHash string    `json:"browserHash"`
	OwnerUserID string    `json:"ownerUserId"`
	Expires     time.Time `json:"expires"`
}

func randomToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value[:]), nil
}

func tokenHash(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

func (s *Service) discordCookie(value string, age int) *http.Cookie {
	return &http.Cookie{Name: "dth_discord_oauth", Value: value, Path: "/integrations/discord", MaxAge: age, HttpOnly: true, Secure: strings.HasPrefix(s.Config.PublicURL, "https://"), SameSite: http.SameSiteLaxMode}
}

// Called with the service lock held after administrator authorization.
func (s *Service) serveIntegrations(w http.ResponseWriter, r *http.Request, state *State) bool {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/devices":
		var token, refresh string
		var err error
		if s.auth != nil {
			token, err = s.providerToken(r.Context(), "oauth_google", healthScopes)
		} else {
			token, refresh, err = s.Health.Token(r.Context(), state.RefreshToken)
		}
		if err == nil && refresh != "" {
			state.RefreshToken = refresh
			if s.Store.Save(r.Context(), *state) != nil {
				http.Error(w, `{"error":"storage unavailable"}`, 500)
				return true
			}
		}
		if err != nil {
			http.Error(w, `{"error":"Google連携とHealth読み取り権限を確認してください"}`, 502)
			return true
		}
		devices, err := s.Health.Devices(r.Context(), token)
		if err != nil {
			http.Error(w, `{"error":"端末一覧を取得できませんでした"}`, 502)
			return true
		}
		_ = json.NewEncoder(w).Encode(devices)
		return true
	case r.Method == http.MethodPost && r.URL.Path == "/integrations/x":
		if !s.Config.EnableX || s.auth == nil {
			http.Error(w, `{"error":"X連携は無効です"}`, 404)
			return true
		}
		if state.Confirmed {
			http.Error(w, `{"error":"判定成立中は変更できません"}`, 409)
			return true
		}
		if _, err := s.providerToken(r.Context(), "oauth_x", []string{"tweet.write", "offline.access"}); err != nil {
			http.Error(w, `{"error":"Clerkのアカウント設定でXを連携し、投稿権限を許可してください"}`, 502)
			return true
		}
		if err := s.addLinkedTarget(r, state, Target{ID: "x", Kind: "x", URL: "https://api.x.com/2/tweets"}); err != nil {
			http.Error(w, `{"error":"送信先を保存できませんでした（最大10件）"}`, 409)
			return true
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
		return true
	case r.Method == http.MethodPost && r.URL.Path == "/integrations/discord":
		if s.auth == nil || s.Config.DiscordClientID == "" || s.Config.DiscordClientSecret == "" {
			http.Error(w, `{"error":"Discord OAuthを設定してください"}`, 404)
			return true
		}
		if state.Confirmed {
			http.Error(w, `{"error":"判定成立中は変更できません"}`, 409)
			return true
		}
		if len(s.settings(*state).Targets) >= 10 {
			http.Error(w, `{"error":"送信先は最大10件です"}`, 409)
			return true
		}
		nonce, err := randomToken()
		if err != nil {
			http.Error(w, `{"error":"OAuthを開始できません"}`, 500)
			return true
		}
		browser, err := randomToken()
		if err != nil {
			http.Error(w, `{"error":"OAuthを開始できません"}`, 500)
			return true
		}
		state.OwnerUserID = s.Config.OwnerUserID
		state.DiscordOAuth = &OAuthPending{StateHash: tokenHash(nonce), BrowserHash: tokenHash(browser), OwnerUserID: s.Config.OwnerUserID, Expires: s.Now().Add(10 * time.Minute)}
		if s.Store.Save(r.Context(), *state) != nil {
			http.Error(w, `{"error":"storage unavailable"}`, 500)
			return true
		}
		http.SetCookie(w, s.discordCookie(browser, 600))
		query := url.Values{"client_id": {s.Config.DiscordClientID}, "redirect_uri": {s.Config.PublicURL + "/integrations/discord/callback"}, "response_type": {"code"}, "scope": {"webhook.incoming"}, "state": {nonce}}
		_ = json.NewEncoder(w).Encode(map[string]string{"url": "https://discord.com/oauth2/authorize?" + query.Encode()})
		return true
	}
	return false
}

func (s *Service) addLinkedTarget(r *http.Request, state *State, target Target) error {
	settings := s.settings(*state)
	// Copy the slice so a failed save cannot modify an in-memory Store's backing array.
	settings.Targets = append([]Target(nil), settings.Targets...)
	found := false
	for i, t := range settings.Targets {
		if t.ID == target.ID {
			settings.Targets[i] = target
			found = true
		}
	}
	if !found {
		if len(settings.Targets) >= 10 {
			return errors.New("too many targets")
		}
		settings.Targets = append(settings.Targets, target)
	}
	settings.Messages = nil
	state.Settings = &settings
	state.OwnerUserID = s.Config.OwnerUserID
	return s.Store.Save(r.Context(), *state)
}

func (s *Service) discordCallback(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Referrer-Policy", "no-referrer")
	state, err := s.Store.Load(r.Context())
	if err != nil {
		http.Error(w, `{"error":"storage unavailable"}`, 500)
		return
	}
	pending := state.DiscordOAuth
	cookie, cookieErr := r.Cookie("dth_discord_oauth")
	valid := s.auth != nil && pending != nil && cookieErr == nil && pending.OwnerUserID == s.Config.OwnerUserID && state.OwnerUserID == s.Config.OwnerUserID && s.Now().Before(pending.Expires)
	if valid {
		valid = subtle.ConstantTimeCompare([]byte(pending.StateHash), []byte(tokenHash(r.URL.Query().Get("state")))) == 1 && subtle.ConstantTimeCompare([]byte(pending.BrowserHash), []byte(tokenHash(cookie.Value))) == 1
	}
	if !valid {
		http.Error(w, `{"error":"OAuthの有効期限切れ、または不正なリクエストです"}`, 400)
		return
	}
	// Consume before exchanging the code, so a callback cannot be replayed after a failure.
	state.DiscordOAuth = nil
	if s.Store.Save(r.Context(), state) != nil {
		http.Error(w, `{"error":"storage unavailable"}`, 500)
		return
	}
	http.SetCookie(w, s.discordCookie("", -1))
	if state.Confirmed {
		http.Error(w, `{"error":"判定成立中は変更できません"}`, 409)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" || r.URL.Query().Get("error") != "" {
		http.Redirect(w, r, "/?integration=cancelled", http.StatusSeeOther)
		return
	}
	form := url.Values{"client_id": {s.Config.DiscordClientID}, "client_secret": {s.Config.DiscordClientSecret}, "grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {s.Config.PublicURL + "/integrations/discord/callback"}}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "https://discord.com/api/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		http.Error(w, `{"error":"Discord連携に失敗しました"}`, 502)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var result struct {
		Webhook struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		} `json:"webhook"`
	}
	if s.Health.json(req, &result) != nil {
		http.Error(w, `{"error":"Discord連携に失敗しました。再度お試しください"}`, 502)
		return
	}
	target := Target{ID: "discord-" + result.Webhook.ID, Kind: "discord", URL: result.Webhook.URL}
	if result.Webhook.ID == "" || target.Validate() != nil {
		http.Error(w, `{"error":"Discordから有効なWebhookを取得できませんでした"}`, 502)
		return
	}
	if s.addLinkedTarget(r, &state, target) != nil {
		http.Error(w, `{"error":"Webhookを保存できませんでした。Discordで不要なWebhookを削除してください"}`, 500)
		return
	}
	http.Redirect(w, r, "/?integration=connected", http.StatusSeeOther)
}
