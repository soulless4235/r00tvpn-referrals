package main

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
)

// Version is the addon's version, set at build time.
var Version = "1.0.0"

//go:embed ui
var uiFiles embed.FS

// info is the addon's /v2/info: who it is, its page, the events it reads.
func info() map[string]any {
	return map[string]any{
		"id":          "r00tvpn-referrals",
		"protocol":    2,
		"version":     Version,
		"kind":        "generic",
		"name":        map[string]string{"ru": "Приведи друга", "en": "Refer a friend"},
		"description": map[string]string{"ru": "Личные ссылки-приглашения через отдельного бота и дни подписки за оплативших друзей", "en": "Personal invite links through a bot of its own, and subscription days for friends who pay"},
		"permissions": []string{"users:all", "users:grant", "events", "kv", "ui"},
		"ui":          map[string]any{"entry": "/ui/index.html", "title": map[string]string{"ru": "Приведи друга", "en": "Refer a friend"}},
		"events":      []string{"payment.paid"},
	}
}

// Handler serves the panel: every request carries the addon's token.
func (a *App) Handler(token string) http.Handler {
	pages, _ := fs.Sub(uiFiles, "ui")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/info", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, info()) })
	mux.HandleFunc("GET /v2/health", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) })
	mux.HandleFunc("GET /ui/{file}", func(w http.ResponseWriter, r *http.Request) { servePage(w, pages, r.PathValue("file")) })
	mux.HandleFunc("POST /v2/ui/rpc", a.rpc)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthorized", "message": "bad token"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// servePage answers a file of the page as it is: the panel follows no redirects, and Go's
// file server sends index.html to "./".
func servePage(w http.ResponseWriter, pages fs.FS, name string) {
	data, err := fs.ReadFile(pages, name)
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	types := map[string]string{".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8"}
	w.Header().Set("Content-Type", types[path.Ext(name)])
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// rpcCall is a call of the addon's page, as the panel forwards it.
type rpcCall struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body"`
}

// pageConfig is the config as the page sees it: never the token, only whether one is set.
type pageConfig struct {
	BotToken    string `json:"bot_token,omitempty"` // write only: empty keeps the stored one
	TokenSet    bool   `json:"token_set"`
	PromoCode   string `json:"promo_code"`
	PromoNote   string `json:"promo_note"`
	RewardDays  int64  `json:"reward_days"`
	TrialTariff string `json:"trial_tariff"`
	ServiceBot  string `json:"service_bot"`
}

type status struct {
	Config    pageConfig `json:"config"`
	BotName   string     `json:"bot_name"`
	BotError  string     `json:"bot_error"`
	Referrers int        `json:"referrers"`
	Invited   int64      `json:"invited"`
	Paid      int64      `json:"paid"`
	Days      int64      `json:"days"`
	Recent    []Reward   `json:"recent"`
}

func (a *App) rpc(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var call rpcCall
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&call); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "bad_request", "message": "bad call"})
		return
	}
	fail := func(err error) {
		a.log.Warn("page call", "path", call.Path, "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"code": "panel_error", "message": err.Error()})
	}
	switch {
	case call.Method == http.MethodGet && call.Path == "/status":
		st, err := a.status(r)
		if err != nil {
			fail(err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	case call.Method == http.MethodPut && call.Path == "/config":
		var in pageConfig
		if err := json.Unmarshal(call.Body, &in); err != nil || in.RewardDays < 1 || in.RewardDays > 365 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "bad_request", "message": "reward_days is 1 to 365"})
			return
		}
		cfg, err := a.LoadConfig(ctx)
		if err != nil {
			fail(err)
			return
		}
		if t := strings.TrimSpace(in.BotToken); t != "" {
			cfg.BotToken = t
		}
		cfg.PromoCode = strings.TrimSpace(in.PromoCode)
		cfg.PromoNote = strings.TrimSpace(in.PromoNote)
		cfg.RewardDays = in.RewardDays
		cfg.TrialTariff = strings.TrimSpace(in.TrialTariff)
		cfg.ServiceBot = strings.TrimPrefix(strings.TrimSpace(in.ServiceBot), "@")
		if err := a.SaveConfig(ctx, cfg); err != nil {
			fail(err)
			return
		}
		if cfg.BotToken != "" {
			_, _ = a.botNameFor(ctx, cfg) // the page shows at once whether the token works
		}
		st, err := a.status(r)
		if err != nil {
			fail(err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"code": "not_found", "message": call.Method + " " + call.Path})
	}
}

// status sums the stats and lists the latest rewards; built for a small service, it reads
// every key it counts.
func (a *App) status(r *http.Request) (status, error) {
	ctx := r.Context()
	cfg, err := a.LoadConfig(ctx)
	if err != nil {
		return status{}, err
	}
	st := status{Config: pageConfig{
		TokenSet: cfg.BotToken != "", PromoCode: cfg.PromoCode, PromoNote: cfg.PromoNote,
		RewardDays: cfg.RewardDays, TrialTariff: cfg.TrialTariff, ServiceBot: cfg.ServiceBot,
	}}
	st.BotName, st.BotError = a.BotStatus()
	if cfg.BotToken == "" {
		st.BotName, st.BotError = "", ""
	}
	keys, err := a.panel.KVKeys(ctx, "stats/", 500)
	if err != nil {
		return st, err
	}
	for _, k := range keys {
		var s Stats
		if found, _, err := a.getJSON(ctx, k, &s); err != nil {
			return st, err
		} else if found {
			st.Referrers++
			st.Invited += s.Invited
			st.Paid += s.Paid
			st.Days += s.Days
		}
	}
	keys, err = a.panel.KVKeys(ctx, "rewarded/", 500)
	if err != nil {
		return st, err
	}
	for _, k := range keys {
		var rw Reward
		if found, _, err := a.getJSON(ctx, k, &rw); err != nil {
			return st, err
		} else if found {
			st.Recent = append(st.Recent, rw)
		}
	}
	sort.Slice(st.Recent, func(i, j int) bool { return st.Recent[i].At > st.Recent[j].At })
	if len(st.Recent) > 20 {
		st.Recent = st.Recent[:20]
	}
	return st, nil
}
