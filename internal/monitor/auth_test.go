package monitor

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func clerkFixture(t *testing.T, store Store) (*Service, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	c := fixtureConfig()
	c.ClientID, c.ClientSecret, c.RefreshToken = "", "", ""
	c.PublicURL, c.OwnerUserID = "https://console.example.com", "user_owner"
	c.ClerkPublishableKey = "pk_test_" + base64.RawStdEncoding.EncodeToString([]byte("test.clerk.accounts.dev$"))
	c.ClerkJWTKey = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	c.ClerkSecretKey = "sk_test_fixture"
	c.DiscordClientID, c.DiscordClientSecret, c.EnableX = "discord-client", "discord-secret", true
	s, err := NewService(c, store)
	if err != nil {
		t.Fatal(err)
	}
	return s, key
}

func sessionJWT(t *testing.T, s *Service, key *rsa.PrivateKey, mutate func(map[string]any)) string {
	t.Helper()
	claims := map[string]any{"iss": s.clerkOrigin, "sub": s.Config.OwnerUserID, "azp": s.Config.PublicURL, "sid": "sess_fixture", "exp": time.Now().Add(time.Minute).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(), "iat": time.Now().Unix(), "v": 2}
	if mutate != nil {
		mutate(claims)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	body, _ := json.Marshal(claims)
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func authRequest(s *Service, method, path, body, bearer, origin string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	for _, cookie := range cookies {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestClerkOwnerAuthentication(t *testing.T) {
	s, key := clerkFixture(t, &memoryStore{})
	s.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("session verification must be networkless")
		return nil, nil
	})
	for name, mutate := range map[string]func(map[string]any){
		"owner":        nil,
		"other user":   func(c map[string]any) { c["sub"] = "user_stranger" },
		"wrong issuer": func(c map[string]any) { c["iss"] = "https://other.clerk.accounts.dev" },
		"wrong origin": func(c map[string]any) { c["azp"] = "https://evil.example.com" },
		"expired":      func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() },
		"future":       func(c map[string]any) { c["nbf"] = time.Now().Add(time.Hour).Unix() },
		"no expiry":    func(c map[string]any) { delete(c, "exp") },
		"no session":   func(c map[string]any) { delete(c, "sid") },
	} {
		t.Run(name, func(t *testing.T) {
			w := authRequest(s, "GET", "/settings", "", sessionJWT(t, s, key, mutate), "")
			want := 401
			if name == "owner" {
				want = 200
			}
			if w.Code != want {
				t.Fatalf("got %d want %d", w.Code, want)
			}
		})
	}
	jwt := sessionJWT(t, s, key, nil)
	if authRequest(s, "GET", "/settings", "", jwt+"tamper", "").Code != 401 {
		t.Fatal("tampered JWT accepted")
	}
	if authRequest(s, "GET", "/settings", "", "", "", &http.Cookie{Name: "__session", Value: jwt}).Code != 401 {
		t.Fatal("ambient cookie accepted")
	}
	if authRequest(s, "POST", "/integrations/discord", "", jwt, "https://evil.example.com").Code != 403 {
		t.Fatal("cross-origin mutation accepted")
	}
	if authRequest(s, "POST", "/integrations/discord", "", jwt, "").Code != 403 {
		t.Fatal("missing origin accepted")
	}
	w := authRequest(s, "GET", "/auth/config", "", "", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), s.Config.ClerkSecretKey) || strings.Contains(w.Body.String(), s.Config.ClerkJWTKey) {
		t.Fatal("public configuration leaked secrets")
	}
	store := s.Store.(*memoryStore)
	store.state.OwnerUserID = "user_previous"
	if authRequest(s, "GET", "/settings", "", jwt, "").Code != 403 {
		t.Fatal("stored owner mismatch accepted")
	}
}

func providerResponse(provider string, scopes []string) *http.Response {
	body, _ := json.Marshal(map[string]any{"data": []any{map[string]any{"provider": provider, "token": "provider-secret", "external_account_id": "external-fixture", "scopes": scopes}}, "total_count": 1})
	return response(200, string(body))
}

type forbiddenAuthStore struct{ t *testing.T }

func (f forbiddenAuthStore) Load(context.Context) (State, error) {
	f.t.Fatal("authorization preflight read storage")
	return State{}, nil
}
func (f forbiddenAuthStore) Save(context.Context, State) error {
	f.t.Fatal("authorization preflight wrote storage")
	return nil
}

func TestAuthorizationPreflightIsStorageFreeAndUsesOriginalMethod(t *testing.T) {
	s, key := clerkFixture(t, forbiddenAuthStore{t})
	owner := sessionJWT(t, s, key, nil)
	for _, scenario := range []struct {
		bearer, origin string
		want           int
	}{
		{"", "", 401},
		{owner, s.Config.PublicURL, 200},
		{owner, "https://evil.example.com", 403},
		{owner, "", 403},
		{s.Config.AdminToken, "", 200},
	} {
		w := authRequest(s, "PUT", "/auth/verify", "unread body", scenario.bearer, scenario.origin)
		if w.Code != scenario.want {
			t.Fatalf("preflight: got %d want %d", w.Code, scenario.want)
		}
	}
}

func TestClerkProviderRefreshWithoutBrowserAndDeviceListing(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	store := &memoryStore{}
	s, _ := clerkFixture(t, store)
	s.Now = func() time.Time { return now }
	clerkCalls := 0
	s.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "api.clerk.com":
			clerkCalls++
			if !strings.Contains(r.URL.Path, "/users/user_owner/oauth_access_tokens/oauth_google") || r.Header.Get("Authorization") != "Bearer sk_test_fixture" {
				t.Fatal("wrong Clerk owner or API key")
			}
			return providerResponse("oauth_google", healthScopes), nil
		case "health.googleapis.com":
			if r.Header.Get("Authorization") != "Bearer provider-secret" {
				t.Fatal("wrong health access token")
			}
			if strings.HasSuffix(r.URL.Path, "pairedDevices") {
				return response(200, `{"pairedDevices":[{"name":"users/me/pairedDevices/test","deviceType":"TRACKER","batteryStatus":"High","lastSyncTime":"`+now.Format(time.RFC3339)+`"},{"name":"scale","deviceType":"SCALE"}]}`), nil
			}
			return response(200, `{"dataPoints":[]}`), nil
		default:
			t.Fatalf("unexpected upstream: %s", r.URL.Host)
		}
		return nil, nil
	})
	if err := s.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := authRequest(s, "GET", "/devices", "", s.Config.AdminToken, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "SCALE") || !strings.Contains(w.Body.String(), s.Config.DeviceName) {
		t.Fatal("invalid tracker list")
	}
	restarted, err := NewService(s.Config, store)
	if err != nil {
		t.Fatal(err)
	}
	restarted.HTTP = s.HTTP
	restarted.Health.HTTP = s.HTTP
	if err := restarted.Check(context.Background()); err != nil || clerkCalls != 3 {
		t.Fatalf("headless restart: %v calls=%d", err, clerkCalls)
	}
	settings := authRequest(s, "GET", "/settings", "", s.Config.AdminToken, "")
	if strings.Contains(settings.Body.String(), "provider-secret") {
		t.Fatal("provider token exposed")
	}
	// Missing permissions must pause monitoring and never contact Google with an invalid token.
	s.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.clerk.com" {
			t.Fatal("used token without required permissions")
		}
		return providerResponse("oauth_google", nil), nil
	})
	if s.Check(context.Background()) == nil || store.state.Eligible {
		t.Fatal("missing permissions did not pause")
	}
}

func TestDiscordOAuthBindingPersistenceAndReplay(t *testing.T) {
	store := &memoryStore{}
	s, key := clerkFixture(t, store)
	jwt := sessionJWT(t, s, key, nil)
	w := authRequest(s, "POST", "/integrations/discord", "", jwt, s.Config.PublicURL)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var result struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	u, _ := url.Parse(result.URL)
	if u.Query().Get("scope") != "webhook.incoming" || u.Query().Get("redirect_uri") != s.Config.PublicURL+"/integrations/discord/callback" {
		t.Fatal("wrong authorization URL")
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatal("insecure OAuth cookie")
	}
	callback := "/integrations/discord/callback?state=" + url.QueryEscape(u.Query().Get("state")) + "&code=fixture"
	if authRequest(s, "GET", callback, "", "", "").Code != 400 {
		t.Fatal("unbound callback accepted")
	}
	wrong := *cookies[0]
	wrong.Value = "wrong"
	if authRequest(s, "GET", callback, "", "", "", &wrong).Code != 400 {
		t.Fatal("wrong browser accepted")
	}
	// Reconstructing simulates eviction/restart between redirect and callback.
	s, _ = NewService(s.Config, store)
	exchanges := 0
	s.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		exchanges++
		if r.URL.String() != "https://discord.com/api/oauth2/token" {
			t.Fatal("unexpected Discord endpoint")
		}
		_ = r.ParseForm()
		if r.Form.Get("client_secret") != "discord-secret" || r.Form.Get("code") != "fixture" {
			t.Fatal("wrong code exchange")
		}
		return response(200, `{"webhook":{"id":"123","url":"https://discord.com/api/webhooks/123/secret"}}`), nil
	})
	s.Health.HTTP = s.HTTP
	w = authRequest(s, "GET", callback, "", "", "", cookies...)
	if w.Code != 303 || w.Header().Get("Location") != "/?integration=connected" || exchanges != 1 {
		t.Fatalf("callback: %d %s", w.Code, w.Body.String())
	}
	if store.state.DiscordOAuth != nil || s.settings(store.state).Targets[1].ID != "discord-123" {
		t.Fatal("webhook not saved")
	}
	if authRequest(s, "GET", callback, "", "", "", cookies...).Code != 400 || exchanges != 1 {
		t.Fatal("callback replayed")
	}
	store.state.Confirmed = true
	if authRequest(s, "POST", "/integrations/discord", "", jwt, s.Config.PublicURL).Code != 409 {
		t.Fatal("confirmed event changed")
	}
}

func TestXPostingCostBoundAndDisabledBehavior(t *testing.T) {
	store := &memoryStore{state: State{Confirmed: true, EventID: "event", Messages: map[string]string{"x": "hello"}, Settings: &Settings{DeviceName: "users/me/pairedDevices/test", Targets: []Target{{ID: "x", Kind: "x", URL: "https://api.x.com/2/tweets"}}}}}
	s, _ := clerkFixture(t, store)
	posts, tokenCalls := 0, 0
	s.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.clerk.com" {
			tokenCalls++
			return providerResponse("oauth_x", []string{"tweet.write", "offline.access"}), nil
		}
		if r.URL.String() != "https://api.x.com/2/tweets" || r.Header.Get("Authorization") != "Bearer provider-secret" {
			t.Fatal("unexpected X request")
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["text"] != "hello" {
			t.Fatal("wrong X post")
		}
		posts++
		return response(500, `{}`), nil
	})
	s.Config.EnableX = false
	if err := s.deliver(context.Background(), &store.state); err != nil || posts != 0 || tokenCalls != 0 {
		t.Fatal("disabled X made requests")
	}
	s.Config.EnableX = true
	for i := 0; i < 5; i++ {
		state, _ := store.Load(context.Background())
		_ = s.deliver(context.Background(), &state)
	}
	if posts != 3 || tokenCalls != 3 || store.state.XAttempts != 3 {
		t.Fatalf("X cap: posts=%d tokens=%d", posts, tokenCalls)
	}
	store.state.XAttempts = 0
	store.fail = true
	state, _ := store.Load(context.Background())
	if s.deliver(context.Background(), &state) == nil || posts != 3 {
		t.Fatal("posted without persisting attempt")
	}
	state = State{Confirmed: false, Settings: store.state.Settings, Messages: store.state.Messages}
	_ = s.deliver(context.Background(), &state)
	if posts != 3 {
		t.Fatal("posted during ordinary monitoring")
	}
}

func TestGoogleAccountChangeDoesNotReuseBaseline(t *testing.T) {
	store := &memoryStore{state: State{GoogleAccountID: "previous", LastHeartRate: time.Now().Add(-time.Hour), LastCheck: time.Now(), Missing: time.Hour, Eligible: true, Messages: map[string]string{"discord": "saved"}}}
	s, _ := clerkFixture(t, store)
	s.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return providerResponse("oauth_google", healthScopes), nil
	})
	state, _ := store.Load(context.Background())
	if _, err := s.clerkHealthToken(context.Background(), &state); err != nil {
		t.Fatal(err)
	}
	if state.GoogleAccountID != "external-fixture" || state.Eligible || state.Missing != 0 || !state.LastHeartRate.IsZero() || state.Messages["discord"] != "saved" {
		t.Fatal("account switch reused old baseline or lost settings")
	}
	state.GoogleAccountID = "previous"
	state.Confirmed = true
	state.EventID = "pending"
	if _, err := s.clerkHealthToken(context.Background(), &state); err == nil || !state.Confirmed || state.EventID != "pending" {
		t.Fatal("account switch discarded a pending event")
	}
}

func TestDiscordOAuthExpirationCancellationAndPersistenceFailure(t *testing.T) {
	store := &memoryStore{}
	s, key := clerkFixture(t, store)
	jwt := sessionJWT(t, s, key, nil)
	s.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected token exchange"); return nil, nil })
	for _, expired := range []bool{true, false} {
		w := authRequest(s, "POST", "/integrations/discord", "", jwt, s.Config.PublicURL)
		var result struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &result)
		u, _ := url.Parse(result.URL)
		callback := "/integrations/discord/callback?state=" + url.QueryEscape(u.Query().Get("state")) + "&error=access_denied"
		if expired {
			store.state.DiscordOAuth.Expires = time.Now().Add(-time.Minute)
		}
		w = authRequest(s, "GET", callback, "", "", "", w.Result().Cookies()...)
		want := 303
		if expired {
			want = 400
		}
		if w.Code != want {
			t.Fatalf("cancel/expiry: %d", w.Code)
		}
		if len(s.settings(store.state).Targets) != 1 {
			t.Fatal("cancel/expiry changed targets")
		}
	}
	store.fail = true
	w := authRequest(s, "POST", "/integrations/discord", "", jwt, s.Config.PublicURL)
	if w.Code != 500 || len(w.Result().Cookies()) != 0 {
		t.Fatal("redirect flow started before persistence")
	}
}
