package monitor

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/jwt"
	"github.com/clerk/clerk-sdk-go/v2/user"
)

var healthScopes = []string{
	"https://www.googleapis.com/auth/googlehealth.health_metrics_and_measurements.readonly",
	"https://www.googleapis.com/auth/googlehealth.settings.readonly",
}

type clerkAuth struct{ key *clerk.JSONWebKey }

func (c Config) validateAuth() error {
	if c.ClerkPublishableKey == "" {
		if c.ClerkSecretKey != "" || c.ClerkJWTKey != "" || c.OwnerUserID != "" || c.EnableX {
			return errors.New("Clerk configuration is incomplete")
		}
		return nil
	}
	u, err := url.Parse(c.PublicURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"))) {
		return errors.New("PUBLIC_URL must be an HTTPS origin (HTTP allowed only on localhost)")
	}
	if c.ClerkSecretKey == "" || c.ClerkJWTKey == "" || !strings.HasPrefix(c.OwnerUserID, "user_") {
		return errors.New("Clerk secret key, JWT public key and owner user ID are required")
	}
	return nil
}

func (s *Service) initAuth() error {
	if s.Config.ClerkPublishableKey == "" {
		return nil
	}
	parts := strings.Split(s.Config.ClerkPublishableKey, "_")
	if len(parts) != 3 || parts[0] != "pk" || (parts[1] != "test" && parts[1] != "live") {
		return errors.New("invalid Clerk publishable key")
	}
	domain, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(parts[2], "="))
	if err != nil || !strings.HasSuffix(string(domain), "$") {
		return errors.New("invalid Clerk frontend domain")
	}
	host := strings.TrimSuffix(string(domain), "$")
	u, err := url.Parse("https://" + host)
	if err != nil || u.Host != host || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || !strings.Contains(host, ".") || strings.ContainsAny(host, " \r\n;'") {
		return errors.New("invalid Clerk frontend domain")
	}
	key, err := clerk.JSONWebKeyFromPEM(s.Config.ClerkJWTKey)
	if err != nil {
		return errors.New("invalid Clerk JWT public key")
	}
	if public, ok := key.Key.(*rsa.PublicKey); !ok || public.N.BitLen() < 2048 {
		return errors.New("Clerk JWT key must be an RSA public key of at least 2048 bits")
	}
	s.auth, s.clerkOrigin = &clerkAuth{key: key}, "https://"+host
	return nil
}

func (s *Service) clerkAuthorized(r *http.Request) bool {
	if s.auth == nil {
		return false
	}
	// Explicit bearer tokens avoid ambient-cookie authentication and its CSRF risks.
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	claims, err := jwt.Verify(r.Context(), &jwt.VerifyParams{
		Token: strings.TrimPrefix(header, "Bearer "), JWK: s.auth.key,
		ProxyURL:               clerk.String(s.clerkOrigin),
		AuthorizedPartyHandler: func(party string) bool { return party == s.Config.PublicURL },
	})
	return err == nil && claims.Subject == s.Config.OwnerUserID && claims.Issuer == s.clerkOrigin && claims.SessionID != "" && claims.Expiry != nil && claims.NotBefore != nil
}

// providerToken asks Clerk to refresh stored provider credentials on demand.
// Tokens never reach the browser or our settings API. No active browser session is needed.
func (s *Service) providerToken(ctx context.Context, provider string, scopes []string) (string, error) {
	token, err := s.providerCredential(ctx, provider, scopes)
	if err != nil {
		return "", err
	}
	return token.Token, nil
}

func (s *Service) providerCredential(ctx context.Context, provider string, scopes []string) (*clerk.OAuthAccessToken, error) {
	if s.auth == nil {
		return nil, errors.New("Clerk is not configured")
	}
	client := user.NewClient(&clerk.ClientConfig{BackendConfig: clerk.BackendConfig{Key: clerk.String(s.Config.ClerkSecretKey), HTTPClient: s.HTTP}})
	list, err := client.ListOAuthAccessTokens(ctx, &user.ListOAuthAccessTokensParams{ID: s.Config.OwnerUserID, Provider: provider})
	if err != nil || list == nil || len(list.OAuthAccessTokens) != 1 {
		return nil, errors.New("provider connection unavailable or ambiguous; reconnect in Clerk")
	}
	token := list.OAuthAccessTokens[0]
	if token == nil || token.Token == "" || token.TokenSecret != nil {
		return nil, errors.New("OAuth 2.0 token required")
	}
	granted := map[string]bool{}
	for _, scope := range token.Scopes {
		granted[scope] = true
	}
	for _, scope := range scopes {
		if !granted[scope] {
			return nil, errors.New("required provider permissions missing; reconnect in Clerk")
		}
	}
	return token, nil
}

func (s *Service) clerkHealthToken(ctx context.Context, state *State) (string, error) {
	credential, err := s.providerCredential(ctx, "oauth_google", healthScopes)
	if err != nil {
		return "", err
	}
	if credential.ExternalAccountID == "" {
		return "", errors.New("Google account identity missing")
	}
	if state.GoogleAccountID != credential.ExternalAccountID {
		// Never carry a monitoring baseline across Google accounts, including first migration.
		if state.Confirmed {
			return "", errors.New("Google account changed during a confirmed event")
		}
		state.LastHeartRate, state.LastCheck = time.Time{}, time.Time{}
		state.Missing, state.Eligible, state.EventID, state.Delivered, state.XAttempts = 0, false, "", nil, 0
		state.GoogleAccountID = credential.ExternalAccountID
		if err := s.Store.Save(ctx, *state); err != nil {
			return "", err
		}
	}
	return credential.Token, nil
}

func (s *Service) servePublicAuth(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet && r.URL.Path == "/auth/config" {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"publishableKey": s.Config.ClerkPublishableKey, "frontendOrigin": s.clerkOrigin,
			"discordEnabled": s.auth != nil && s.Config.DiscordClientID != "" && s.Config.DiscordClientSecret != "",
			"xEnabled":       s.Config.EnableX,
		})
		return true
	}
	if r.Method == http.MethodGet && r.URL.Path == "/integrations/discord/callback" {
		s.discordCallback(w, r)
		return true
	}
	return false
}
