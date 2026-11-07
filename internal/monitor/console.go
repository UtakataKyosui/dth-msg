package monitor

import (
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

//go:embed console.html
var consoleHTML string

type Settings struct {
	DeviceName string            `json:"deviceName"`
	Targets    []Target          `json:"targets"`
	Messages   map[string]string `json:"messages"`
}

func (s *Service) settings(state State) Settings {
	settings := Settings{DeviceName: s.Config.DeviceName, Targets: s.Config.Targets}
	if state.Settings != nil {
		settings = *state.Settings
	}
	settings.Messages = state.Messages
	return settings
}

func serveConsole(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet || (r.URL.Path != "/" && r.URL.Path != "/console.js") {
		return false
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	if r.URL.Path == "/console.js" {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = io.WriteString(w, consoleJS)
	} else {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, consoleHTML)
	}
	return true
}

//go:embed console.js
var consoleJS string

func (s *Service) serveSettings(w http.ResponseWriter, r *http.Request, state State) {
	if r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(s.settings(state))
		return
	}
	if state.Confirmed {
		http.Error(w, `{"error":"判定成立中は設定を変更できません"}`, http.StatusConflict)
		return
	}
	var settings Settings
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	var extra any
	if decoder.Decode(&settings) != nil || decoder.Decode(&extra) != io.EOF {
		http.Error(w, `{"error":"設定のJSONが不正です"}`, http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(settings.DeviceName, "users/me/pairedDevices/") || strings.TrimPrefix(settings.DeviceName, "users/me/pairedDevices/") == "" || strings.ContainsAny(settings.DeviceName, " ?#\r\n") || len(settings.Targets) < 1 || len(settings.Targets) > 10 {
		http.Error(w, `{"error":"端末リソース名と1〜10件の送信先が必要です"}`, http.StatusBadRequest)
		return
	}
	ids := map[string]bool{}
	for _, target := range settings.Targets {
		if err := target.Validate(); err != nil {
			http.Error(w, `{"error":"送信先の種類・HTTPS URL・トークンを確認してください"}`, http.StatusBadRequest)
			return
		}
		limit := 2000
		if target.Kind == "mastodon" {
			limit = 500
		}
		message := settings.Messages[target.ID]
		if ids[target.ID] || (message != "" && (strings.TrimSpace(message) == "" || !utf8.ValidString(message) || utf8.RuneCountInString(message) > limit)) {
			http.Error(w, `{"error":"送信先IDの重複またはメッセージの文字数が不正です"}`, http.StatusBadRequest)
			return
		}
		ids[target.ID] = true
	}
	for id := range settings.Messages {
		if !ids[id] {
			http.Error(w, `{"error":"未登録の送信先のメッセージがあります"}`, http.StatusBadRequest)
			return
		}
	}
	if settings.DeviceName != s.settings(state).DeviceName {
		// A different device must establish its own monitoring baseline.
		state = State{RefreshToken: state.RefreshToken}
	}
	state.Messages = settings.Messages
	settings.Messages = nil
	state.Settings = &settings
	if s.Store.Save(r.Context(), state) != nil {
		http.Error(w, `{"error":"設定を保存できませんでした"}`, http.StatusInternalServerError)
		return
	}
	_, _ = io.WriteString(w, `{"ok":true}`)
}
