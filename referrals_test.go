package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePanel is the Panel API in memory: the KV store, subscribers and grants.
type fakePanel struct {
	mu     sync.Mutex
	kv     map[string][]byte
	ver    map[string]int64
	users  map[int64][]User // by Telegram id
	grants map[string]grantCall
}

type grantCall struct {
	User int64
	Days int64
	Key  string
}

func newFakePanel() *fakePanel {
	return &fakePanel{kv: map[string][]byte{}, ver: map[string]int64{}, users: map[int64][]User{}, grants: map[string]grantCall{}}
}

func (f *fakePanel) addUser(tg, id int64, tariff, state string) {
	t := tg
	f.users[tg] = append(f.users[tg], User{ID: id, TariffName: tariff, State: state, TgID: &t})
}

func (f *fakePanel) value(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.kv[key]
	return v, ok
}

func (f *fakePanel) grant(key string) (grantCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.grants[key]
	return g, ok
}

func (f *fakePanel) grantCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.grants)
}

func (f *fakePanel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer panel-token" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthorized"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/panel/v2")
	switch {
	case path == "/kv" && r.Method == http.MethodGet:
		prefix := r.URL.Query().Get("prefix")
		keys := []string{}
		for k := range f.kv {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"keys": keys, "next": ""})
	case strings.HasPrefix(path, "/kv/"):
		key := strings.TrimPrefix(path, "/kv/")
		switch r.Method {
		case http.MethodGet:
			v, ok := f.kv[key]
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]string{"code": "not_found"})
				return
			}
			w.Header().Set("ETag", `"`+strconv.FormatInt(f.ver[key], 10)+`"`)
			_, _ = w.Write(v)
		case http.MethodPut:
			_, exists := f.kv[key]
			if r.URL.Query().Get("if_absent") == "1" && exists {
				writeJSON(w, http.StatusConflict, map[string]string{"code": "conflict"})
				return
			}
			if m := strings.Trim(r.Header.Get("If-Match"), `"`); m != "" && m != strconv.FormatInt(f.ver[key], 10) {
				writeJSON(w, http.StatusConflict, map[string]string{"code": "conflict"})
				return
			}
			body, _ := io.ReadAll(r.Body)
			f.kv[key] = body
			f.ver[key]++
			writeJSON(w, http.StatusOK, map[string]int64{"version": f.ver[key]})
		}
	case path == "/users":
		tg, _ := strconv.ParseInt(r.URL.Query().Get("q"), 10, 64)
		users := f.users[tg]
		if users == nil {
			users = []User{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"users": users, "next": nil})
	case strings.HasSuffix(path, "/grant"):
		id, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(path, "/users/"), "/grant"), 10, 64)
		var in struct {
			Days   int64  `json:"days"`
			Reason string `json:"reason"`
			Key    string `json:"key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Reason == "" || in.Key == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "bad_request"})
			return
		}
		_, repeat := f.grants[in.Key]
		if !repeat {
			f.grants[in.Key] = grantCall{User: id, Days: in.Days, Key: in.Key}
		}
		exp := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
		writeJSON(w, http.StatusOK, map[string]any{"user_id": id, "days": in.Days, "expires_at": exp, "repeat": repeat})
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"code": "not_found", "message": path})
	}
}

// fakeTelegram records what the bot sent.
type fakeTelegram struct {
	mu   sync.Mutex
	sent []sentMessage
}

type sentMessage struct {
	Chat int64
	Text string
}

func (t *fakeTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/getMe"):
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"id": 1, "username": "friends_bot"}})
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		var in struct {
			Chat int64  `json:"chat_id"`
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		t.sent = append(t.sent, sentMessage{in.Chat, in.Text})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{}})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error_code": 404, "description": "Not Found"})
	}
}

func (t *fakeTelegram) to(chat int64) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for _, m := range t.sent {
		if m.Chat == chat {
			out = append(out, m.Text)
		}
	}
	return out
}

type fixture struct {
	app   *App
	panel *fakePanel
	tg    *fakeTelegram
	cfg   Config
	bot   *Bot
}

func setup(t *testing.T) *fixture {
	t.Helper()
	fp, ft := newFakePanel(), &fakeTelegram{}
	ps, ts := httptest.NewServer(fp), httptest.NewServer(ft)
	t.Cleanup(ps.Close)
	t.Cleanup(ts.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	app := NewApp(NewPanel(ps.URL+"/panel/v2", "panel-token", ps.Client()), ts.URL, ts.Client(), log)
	app.now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	cfg := defaultConfig()
	cfg.BotToken, cfg.PromoCode = "1:TOKEN", "FRIEND30"
	return &fixture{app: app, panel: fp, tg: ft, cfg: cfg, bot: app.bot(cfg)}
}

func (f *fixture) message(t *testing.T, from int64, text string) {
	t.Helper()
	m := &Message{Text: text}
	m.From = &struct {
		ID           int64  `json:"id"`
		IsBot        bool   `json:"is_bot"`
		LanguageCode string `json:"language_code"`
	}{ID: from}
	m.Chat.ID, m.Chat.Type = from, "private"
	if err := f.app.HandleMessage(context.Background(), f.cfg, f.bot, "friends_bot", m); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) code(t *testing.T, referrer int64) string {
	t.Helper()
	code, ok := f.panel.value(keyOwner(referrer))
	if !ok {
		t.Fatalf("no code for %d", referrer)
	}
	return string(code)
}

func (f *fixture) stats(t *testing.T, referrer int64) Stats {
	t.Helper()
	var st Stats
	v, _ := f.panel.value(keyStats(referrer))
	_ = json.Unmarshal(v, &st)
	return st
}

func paid(tg int64, kind string) Event {
	data, _ := json.Marshal(map[string]any{"kind": kind, "amount": 69000, "currency": "RUB"})
	return Event{ID: 7, Type: "payment.paid", TgID: tg, UserID: 99, Data: data}
}

func last(msgs []string) string {
	if len(msgs) == 0 {
		return ""
	}
	return msgs[len(msgs)-1]
}

func TestSubscriberGetsOneLinkForGood(t *testing.T) {
	f := setup(t)
	f.panel.addUser(100, 1, "Стандарт", "active")

	f.message(t, 100, "/start")
	code := f.code(t, 100)
	if got := last(f.tg.to(100)); !strings.Contains(got, "https://t.me/friends_bot?start=r"+code) {
		t.Fatalf("no link in %q", got)
	}
	f.message(t, 100, "/start")
	if f.code(t, 100) != code {
		t.Fatal("a second /start made another code")
	}
}

func TestNoLinkWithoutSubscription(t *testing.T) {
	f := setup(t)
	f.message(t, 200, "/start")
	if got := last(f.tg.to(200)); !strings.Contains(got, "Оформите подписку") || strings.Contains(got, "?start=r") {
		t.Fatalf("got %q", got)
	}
	if _, ok := f.panel.value(keyOwner(200)); ok {
		t.Fatal("a code was made for an account without a subscription")
	}
}

func TestNewFriendIsRememberedAndGetsTheCode(t *testing.T) {
	f := setup(t)
	f.panel.addUser(100, 1, "Стандарт", "active")
	f.message(t, 100, "/start")
	code := f.code(t, 100)

	f.message(t, 300, "/start r"+code)
	if got := last(f.tg.to(300)); !strings.Contains(got, "FRIEND30") {
		t.Fatalf("no promo code for the friend: %q", got)
	}
	var fr Friend
	v, _ := f.panel.value(keyFriend(300))
	if err := json.Unmarshal(v, &fr); err != nil || fr.Referrer != 100 {
		t.Fatalf("friend record %+v %v", fr, err)
	}
	if st := f.stats(t, 100); st.Invited != 1 {
		t.Fatalf("stats %+v", st)
	}
	if got := last(f.tg.to(100)); !strings.Contains(got, "пришёл друг") {
		t.Fatalf("referrer not told: %q", got)
	}

	// coming again by the same link counts once
	f.message(t, 300, "/start r"+code)
	if st := f.stats(t, 100); st.Invited != 1 {
		t.Fatalf("stats after a repeat %+v", st)
	}
}

func TestTrialUserStillCountsAsNew(t *testing.T) {
	f := setup(t)
	f.panel.addUser(100, 1, "Стандарт", "active")
	f.panel.addUser(301, 5, "Пробный", "active")
	f.message(t, 100, "/start")
	f.message(t, 301, "/start r"+f.code(t, 100))
	if _, ok := f.panel.value(keyFriend(301)); !ok {
		t.Fatal("a trial user was not taken as a new friend")
	}
}

func TestCustomerIsNoFriend(t *testing.T) {
	f := setup(t)
	f.panel.addUser(100, 1, "Стандарт", "active")
	f.panel.addUser(400, 2, "Безлимит", "active")
	f.message(t, 100, "/start")
	f.message(t, 400, "/start r"+f.code(t, 100))
	if _, ok := f.panel.value(keyFriend(400)); ok {
		t.Fatal("a paying customer was taken as a friend")
	}
	if got := last(f.tg.to(400)); !strings.Contains(got, "уже пользуетесь") {
		t.Fatalf("got %q", got)
	}
}

func TestOwnLinkIsNoInvite(t *testing.T) {
	f := setup(t)
	f.panel.addUser(100, 1, "Стандарт", "active")
	f.message(t, 100, "/start")
	f.message(t, 100, "/start r"+f.code(t, 100))
	if _, ok := f.panel.value(keyFriend(100)); ok {
		t.Fatal("a referrer became their own friend")
	}
}

func TestFirstPaymentRewardsTheReferrerOnce(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.panel.addUser(100, 1, "Стандарт", "active")
	f.message(t, 100, "/start")
	f.message(t, 300, "/start r"+f.code(t, 100))

	if err := f.app.HandlePayment(ctx, f.cfg, f.bot, paid(300, "new")); err != nil {
		t.Fatal(err)
	}
	g, ok := f.panel.grant(grantKey(300))
	if !ok || g.User != 1 || g.Days != 14 {
		t.Fatalf("grant %+v %v", g, ok)
	}
	if st := f.stats(t, 100); st.Paid != 1 || st.Days != 14 {
		t.Fatalf("stats %+v", st)
	}
	if got := last(f.tg.to(100)); !strings.Contains(got, "+14 дней") || !strings.Contains(got, "01.12.2026") {
		t.Fatalf("referrer not told: %q", got)
	}

	// the same event again, and a renewal later, give nothing more
	before := len(f.tg.to(100))
	for _, ev := range []Event{paid(300, "new"), paid(300, "renew")} {
		if err := f.app.HandlePayment(ctx, f.cfg, f.bot, ev); err != nil {
			t.Fatal(err)
		}
	}
	if st := f.stats(t, 100); st.Paid != 1 || st.Days != 14 || len(f.tg.to(100)) != before {
		t.Fatalf("rewarded twice: %+v", st)
	}
}

func TestOnlySubscriptionPaymentsOfFriendsCount(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.panel.addUser(100, 1, "Стандарт", "active")
	f.message(t, 100, "/start")
	f.message(t, 300, "/start r"+f.code(t, 100))

	for _, ev := range []Event{paid(300, "package"), paid(300, "trial"), paid(500, "new")} {
		if err := f.app.HandlePayment(ctx, f.cfg, f.bot, ev); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.panel.grantCount(); n != 0 {
		t.Fatalf("grants: %d", n)
	}
}

func TestReferrerWithoutSubscriptionGetsNothing(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.panel.addUser(100, 1, "Стандарт", "active")
	f.message(t, 100, "/start")
	f.message(t, 300, "/start r"+f.code(t, 100))
	f.panel.mu.Lock()
	f.panel.users[100] = nil // the subscription is gone by the time the friend pays
	f.panel.mu.Unlock()

	if err := f.app.HandlePayment(ctx, f.cfg, f.bot, paid(300, "new")); err != nil {
		t.Fatal(err)
	}
	var rw Reward
	v, _ := f.panel.value(keyRewarded(300))
	_ = json.Unmarshal(v, &rw)
	if f.panel.grantCount() != 0 || rw.Skipped == "" {
		t.Fatalf("reward %+v grants %d", rw, f.panel.grantCount())
	}
}

func TestPanelCallsNeedTheToken(t *testing.T) {
	f := setup(t)
	h := f.app.Handler("addon-token")
	req := httptest.NewRequest(http.MethodGet, "/v2/info", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	req.Header.Set("Authorization", "Bearer addon-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out struct {
		ID       string `json:"id"`
		Protocol int    `json:"protocol"`
		UI       struct {
			Entry string `json:"entry"`
		} `json:"ui"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || out.ID != "r00tvpn-referrals" || out.Protocol != 2 || out.UI.Entry != "/ui/index.html" {
		t.Fatalf("info %d %s", rec.Code, rec.Body)
	}
	req = httptest.NewRequest(http.MethodGet, "/ui/index.html", nil)
	req.Header.Set("Authorization", "Bearer addon-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Приведи друга") {
		t.Fatalf("page %d", rec.Code)
	}
}

func TestPageNeverSeesTheToken(t *testing.T) {
	f := setup(t)
	h := f.app.Handler("addon-token")
	rpc := func(method, path string, body any) (int, string) {
		b, _ := json.Marshal(body)
		call, _ := json.Marshal(map[string]any{"method": method, "path": path, "body": json.RawMessage(b), "admin": map[string]string{"lang": "ru"}})
		req := httptest.NewRequest(http.MethodPost, "/v2/ui/rpc", bytes.NewReader(call))
		req.Header.Set("Authorization", "Bearer addon-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	code, body := rpc(http.MethodPut, "/config", map[string]any{
		"bot_token": "1:SECRET", "promo_code": "FRIEND30", "promo_note": "−30%", "reward_days": 14,
		"trial_tariff": "Пробный", "service_bot": "@r00tvpn_bot",
	})
	if code != http.StatusOK || strings.Contains(body, "SECRET") || !strings.Contains(body, `"token_set":true`) || !strings.Contains(body, `"bot_name":"friends_bot"`) {
		t.Fatalf("save %d %s", code, body)
	}
	cfg, _ := f.app.LoadConfig(context.Background())
	if cfg.BotToken != "1:SECRET" || cfg.ServiceBot != "r00tvpn_bot" {
		t.Fatalf("stored %+v", cfg)
	}
	// an empty token field keeps the stored token
	code, _ = rpc(http.MethodPut, "/config", map[string]any{"reward_days": 7, "trial_tariff": "Пробный"})
	cfg, _ = f.app.LoadConfig(context.Background())
	if code != http.StatusOK || cfg.BotToken != "1:SECRET" || cfg.RewardDays != 7 {
		t.Fatalf("second save %d %+v", code, cfg)
	}
	if code, _ := rpc(http.MethodPut, "/config", map[string]any{"reward_days": 0}); code != http.StatusBadRequest {
		t.Fatalf("0 days took: %d", code)
	}
}
