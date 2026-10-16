package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config is what the admin sets on the addon's page. It lives in the KV store under
// "config", the bot's token with it: the store is the panel's, backed up with it.
type Config struct {
	BotToken    string `json:"bot_token"`
	PromoCode   string `json:"promo_code"`   // the code a friend gets, e.g. FRIEND30; empty: none
	PromoNote   string `json:"promo_note"`   // what the code gives, e.g. "−30% на первую оплату"
	RewardDays  int64  `json:"reward_days"`  // days the referrer gets for each friend who pays
	TrialTariff string `json:"trial_tariff"` // a friend only on this plan still counts as new
	ServiceBot  string `json:"service_bot"`  // the service's own bot, where a friend buys
}

func defaultConfig() Config {
	return Config{PromoNote: "−30% на первую оплату", RewardDays: 14, TrialTariff: "Пробный", ServiceBot: "r00tvpn_bot"}
}

// Stats is one referrer's count.
type Stats struct {
	Invited int64 `json:"invited"`
	Paid    int64 `json:"paid"`
	Days    int64 `json:"days"`
}

// Friend is who brought an account, and when.
type Friend struct {
	Referrer int64 `json:"referrer"`
	At       int64 `json:"at"`
}

// Reward is a friend's first payment and what the referrer got for it.
type Reward struct {
	Friend   int64  `json:"friend"`
	Referrer int64  `json:"referrer"`
	UserID   int64  `json:"user_id,omitempty"`
	Days     int64  `json:"days,omitempty"`
	At       int64  `json:"at"`
	Skipped  string `json:"skipped,omitempty"` // why nothing was given
}

// KV keys. A code is the part of a link after ?start=r, so links never show a Telegram id.
const (
	keyConfig = "config"
	keyOffset = "tg/offset"
)

func keyCode(code string) string   { return "ref/code/" + code }
func keyOwner(tg int64) string     { return "ref/owner/" + strconv.FormatInt(tg, 10) }
func keyFriend(tg int64) string    { return "friend/" + strconv.FormatInt(tg, 10) }
func keyRewarded(tg int64) string  { return "rewarded/" + strconv.FormatInt(tg, 10) }
func keyStats(tg int64) string     { return "stats/" + strconv.FormatInt(tg, 10) }
func grantKey(friend int64) string { return "ref-" + strconv.FormatInt(friend, 10) }

// App is the addon: the referral bot and the payment events.
type App struct {
	panel   *Panel
	tgBase  string
	hc      *http.Client
	log     *slog.Logger
	now     func() time.Time
	pollGap time.Duration // pause after a failed poll

	mu       sync.Mutex
	botName  string // the bot's username for the token below
	botFor   string
	botError string
}

func NewApp(panel *Panel, tgBase string, hc *http.Client, log *slog.Logger) *App {
	return &App{panel: panel, tgBase: tgBase, hc: hc, log: log, now: time.Now, pollGap: 5 * time.Second}
}

// LoadConfig is the stored config over the defaults.
func (a *App) LoadConfig(ctx context.Context) (Config, error) {
	cfg := defaultConfig()
	data, _, err := a.panel.KVGet(ctx, keyConfig)
	if errors.Is(err, ErrNoKey) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return defaultConfig(), err
	}
	if cfg.RewardDays <= 0 {
		cfg.RewardDays = defaultConfig().RewardDays
	}
	return cfg, nil
}

func (a *App) SaveConfig(ctx context.Context, cfg Config) error {
	data, _ := json.Marshal(cfg)
	_, err := a.panel.KVPut(ctx, keyConfig, data, false, "")
	return err
}

func (a *App) bot(cfg Config) *Bot { return &Bot{base: a.tgBase, token: cfg.BotToken, hc: a.hc} }

// getJSON reads key into v; found is false when the key is absent.
func (a *App) getJSON(ctx context.Context, key string, v any) (found bool, version string, err error) {
	data, version, err := a.panel.KVGet(ctx, key)
	if errors.Is(err, ErrNoKey) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, version, json.Unmarshal(data, v)
}

// updateStats changes a referrer's count, retrying while another write gets in first.
func (a *App) updateStats(ctx context.Context, referrer int64, change func(*Stats)) error {
	for range 5 {
		var st Stats
		found, version, err := a.getJSON(ctx, keyStats(referrer), &st)
		if err != nil {
			return err
		}
		change(&st)
		data, _ := json.Marshal(st)
		_, err = a.panel.KVPut(ctx, keyStats(referrer), data, !found, version)
		if IsConflict(err) {
			continue
		}
		return err
	}
	return errors.New("stats kept changing")
}

const codeAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

func newCode() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)]
	}
	return string(b)
}

// codeOf is referrer's link code, made on first use.
func (a *App) codeOf(ctx context.Context, tg int64) (string, error) {
	data, _, err := a.panel.KVGet(ctx, keyOwner(tg))
	if err == nil {
		return string(data), nil
	}
	if !errors.Is(err, ErrNoKey) {
		return "", err
	}
	for range 5 {
		code := newCode()
		if _, err := a.panel.KVPut(ctx, keyCode(code), []byte(strconv.FormatInt(tg, 10)), true, ""); IsConflict(err) {
			continue // taken by someone else
		} else if err != nil {
			return "", err
		}
		if _, err := a.panel.KVPut(ctx, keyOwner(tg), []byte(code), true, ""); IsConflict(err) {
			// another request of the same account was quicker: keep its code
			data, _, err := a.panel.KVGet(ctx, keyOwner(tg))
			return string(data), err
		} else if err != nil {
			return "", err
		}
		return code, nil
	}
	return "", errors.New("no free code")
}

// referrerOf is the account behind a link code; 0 when the code is unknown.
func (a *App) referrerOf(ctx context.Context, code string) (int64, error) {
	if code == "" || strings.ContainsAny(code, "/:") {
		return 0, nil
	}
	data, _, err := a.panel.KVGet(ctx, keyCode(code))
	if errors.Is(err, ErrNoKey) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	tg, _ := strconv.ParseInt(string(data), 10, 64)
	return tg, nil
}

// isCustomer: the account holds a subscription other than the trial one, so it is no new
// customer a friend could bring.
func isCustomer(users []User, cfg Config) bool {
	for _, u := range users {
		if cfg.TrialTariff == "" || !strings.EqualFold(strings.TrimSpace(u.TariffName), strings.TrimSpace(cfg.TrialTariff)) {
			return true
		}
	}
	return false
}

// hasLiveSubscription: something to extend for a referrer.
func hasLiveSubscription(users []User) bool {
	for _, u := range users {
		if u.State != "disabled" {
			return true
		}
	}
	return false
}

// HandleMessage answers one private message to the referral bot.
func (a *App) HandleMessage(ctx context.Context, cfg Config, bot *Bot, botName string, m *Message) error {
	if m.From == nil || m.From.IsBot || m.Chat.Type != "private" {
		return nil
	}
	tg := m.From.ID
	payload := ""
	if rest, ok := strings.CutPrefix(strings.TrimSpace(m.Text), "/start"); ok {
		payload = strings.TrimSpace(rest)
	}
	if code, ok := strings.CutPrefix(payload, "r"); ok && code != "" {
		return a.invite(ctx, cfg, bot, botName, tg, code)
	}
	return a.ownLink(ctx, cfg, bot, botName, tg)
}

func serviceLink(cfg Config) string {
	return "@" + strings.TrimPrefix(strings.TrimSpace(cfg.ServiceBot), "@")
}

// ownLink: a subscriber gets their link and their count; anyone else is sent to subscribe.
func (a *App) ownLink(ctx context.Context, cfg Config, bot *Bot, botName string, tg int64) error {
	users, err := a.panel.UsersOf(ctx, tg)
	if err != nil {
		return err
	}
	if !hasLiveSubscription(users) {
		return bot.Send(ctx, tg, "Приглашать друзей могут подписчики r00tvpn.\n\nОформите подписку в "+
			html.EscapeString(serviceLink(cfg))+" — и возвращайтесь за своей ссылкой.")
	}
	code, err := a.codeOf(ctx, tg)
	if err != nil {
		return err
	}
	var st Stats
	if _, _, err := a.getJSON(ctx, keyStats(tg), &st); err != nil {
		return err
	}
	link := "https://t.me/" + botName + "?start=r" + code
	var b strings.Builder
	b.WriteString("🎁 <b>Приведи друга</b>\n\n")
	b.WriteString("Ваша личная ссылка:\n" + html.EscapeString(link) + "\n\n")
	if cfg.PromoCode != "" {
		b.WriteString("• <b>другу</b> — промокод " + html.EscapeString(cfg.PromoCode))
		if cfg.PromoNote != "" {
			b.WriteString(": " + html.EscapeString(cfg.PromoNote))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "• <b>вам</b> — +%d дней подписки за каждого друга, который оплатит\n\n", cfg.RewardDays)
	fmt.Fprintf(&b, "Перешли по ссылке: %d · оплатили: %d · получено дней: %d", st.Invited, st.Paid, st.Days)
	share := "https://t.me/share/url?" + url.Values{
		"url":  {link},
		"text": {"Попробуй r00tvpn — быстрый VPN. По моей ссылке скидка на первую оплату:"},
	}.Encode()
	return bot.Send(ctx, tg, b.String(), Button{Text: "📤 Поделиться ссылкой", URL: share})
}

// invite: a friend came by a link. A new customer is remembered as the referrer's (once:
// the first link wins) and gets the gift; anyone else is told why not.
func (a *App) invite(ctx context.Context, cfg Config, bot *Bot, botName string, friend int64, code string) error {
	referrer, err := a.referrerOf(ctx, code)
	if err != nil {
		return err
	}
	switch {
	case referrer == 0:
		return bot.Send(ctx, friend, "Эта ссылка недействительна. Узнать об r00tvpn можно в "+html.EscapeString(serviceLink(cfg))+".")
	case referrer == friend:
		return a.ownLink(ctx, cfg, bot, botName, friend)
	}
	users, err := a.panel.UsersOf(ctx, friend)
	if err != nil {
		return err
	}
	if isCustomer(users, cfg) {
		return bot.Send(ctx, friend, "Вы уже пользуетесь r00tvpn — ссылки друзей работают для новых клиентов.\n\n"+
			"Пригласите своих друзей: отправьте /start, чтобы получить свою ссылку.")
	}
	data, _ := json.Marshal(Friend{Referrer: referrer, At: a.now().Unix()})
	_, err = a.panel.KVPut(ctx, keyFriend(friend), data, true, "")
	fresh := err == nil
	if err != nil && !IsConflict(err) {
		return err
	}
	if fresh {
		if err := a.updateStats(ctx, referrer, func(s *Stats) { s.Invited++ }); err != nil {
			a.log.Warn("stats", "err", err)
		}
		_ = bot.Send(ctx, referrer, fmt.Sprintf("👋 По вашей ссылке пришёл друг. Когда он оплатит подписку, вы получите +%d дней.", cfg.RewardDays))
	}
	var b strings.Builder
	b.WriteString("👋 <b>Вас пригласили в r00tvpn!</b>\n\n")
	if cfg.PromoCode != "" {
		b.WriteString("Ваш подарок — промокод <code>" + html.EscapeString(cfg.PromoCode) + "</code>")
		if cfg.PromoNote != "" {
			b.WriteString(": " + html.EscapeString(cfg.PromoNote))
		}
		b.WriteString(".\n\n")
	}
	b.WriteString("<b>Как подключиться:</b>\n")
	b.WriteString("1. Откройте " + html.EscapeString(serviceLink(cfg)) + " — можно сначала взять 3 дня бесплатно\n")
	if cfg.PromoCode != "" {
		b.WriteString("2. Введите промокод в «Промокоды» (кнопка «Подписка» слева от поля ввода)\n")
		b.WriteString("3. Выберите тариф и оплатите — скидка применится сама")
	} else {
		b.WriteString("2. Выберите тариф и оплатите")
	}
	service := "https://t.me/" + strings.TrimPrefix(strings.TrimSpace(cfg.ServiceBot), "@")
	return bot.Send(ctx, friend, b.String(), Button{Text: "Открыть " + serviceLink(cfg), URL: service})
}

// paymentData is the part of a payment.paid event this addon reads.
type paymentData struct {
	Kind string `json:"kind"` // new | renew | package | trial
}

// HandlePayment rewards the referrer of a friend's first paid subscription. A repeat of the
// event finds the friend rewarded, and the panel answers a repeated grant key once.
func (a *App) HandlePayment(ctx context.Context, cfg Config, bot *Bot, ev Event) error {
	if ev.Type != "payment.paid" || ev.TgID == 0 {
		return nil
	}
	var pd paymentData
	if err := json.Unmarshal(ev.Data, &pd); err != nil {
		return nil
	}
	if pd.Kind != "new" && pd.Kind != "renew" {
		return nil
	}
	friend := ev.TgID
	var fr Friend
	found, _, err := a.getJSON(ctx, keyFriend(friend), &fr)
	if err != nil || !found {
		return err
	}
	var prior Reward
	if done, _, err := a.getJSON(ctx, keyRewarded(friend), &prior); err != nil || done {
		return err
	}
	reward := Reward{Friend: friend, Referrer: fr.Referrer, At: a.now().Unix()}
	users, err := a.panel.UsersOf(ctx, fr.Referrer)
	if err != nil {
		return err
	}
	var target *User
	for i := range users {
		if users[i].State != "disabled" {
			target = &users[i]
			break
		}
	}
	var grant Grant
	if target == nil {
		reward.Skipped = "у пригласившего нет подписки"
	} else {
		grant, err = a.panel.GrantDays(ctx, target.ID, cfg.RewardDays, "Приведи друга: оплатил друг "+strconv.FormatInt(friend, 10), grantKey(friend))
		switch {
		case IsStatus(err, http.StatusConflict):
			reward.Skipped = "подписка пригласившего отключена"
		case err != nil:
			return err
		default:
			reward.UserID, reward.Days = target.ID, cfg.RewardDays
		}
	}
	data, _ := json.Marshal(reward)
	if _, err := a.panel.KVPut(ctx, keyRewarded(friend), data, true, ""); err != nil && !IsConflict(err) {
		return err
	}
	if reward.Days == 0 {
		a.log.Info("friend paid, nothing given", "skipped", reward.Skipped)
		return nil
	}
	if err := a.updateStats(ctx, fr.Referrer, func(s *Stats) { s.Paid++; s.Days += reward.Days }); err != nil {
		a.log.Warn("stats", "err", err)
	}
	text := fmt.Sprintf("🎉 Ваш друг оплатил подписку — вам +%d дней!", reward.Days)
	if grant.ExpiresAt != nil {
		text += "\nПодписка теперь действует до " + grant.ExpiresAt.Local().Format("02.01.2006") + "."
	}
	if cfg.BotToken != "" {
		_ = bot.Send(ctx, fr.Referrer, text)
	}
	return nil
}

// botName is the username of the configured bot, asked once per token.
func (a *App) botNameFor(ctx context.Context, cfg Config) (string, error) {
	a.mu.Lock()
	if a.botFor == cfg.BotToken && a.botName != "" {
		name := a.botName
		a.mu.Unlock()
		return name, nil
	}
	a.mu.Unlock()
	me, err := a.bot(cfg).GetMe(ctx)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.botError = err.Error()
		return "", err
	}
	a.botName, a.botFor, a.botError = me.Username, cfg.BotToken, ""
	return me.Username, nil
}

// BotStatus is what the admin page shows of the bot.
func (a *App) BotStatus() (name, failure string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.botName, a.botError
}

// RunBot answers the referral bot's messages until ctx ends. Without a token it waits for
// the admin to set one.
func (a *App) RunBot(ctx context.Context) {
	for ctx.Err() == nil {
		cfg, err := a.LoadConfig(ctx)
		if err != nil || cfg.BotToken == "" {
			if err != nil {
				a.log.Warn("config", "err", err)
			}
			sleep(ctx, 15*time.Second)
			continue
		}
		name, err := a.botNameFor(ctx, cfg)
		if err != nil {
			a.log.Warn("telegram getMe", "err", err)
			sleep(ctx, 30*time.Second)
			continue
		}
		var offset int64
		if data, _, err := a.panel.KVGet(ctx, keyOffset); err == nil {
			offset, _ = strconv.ParseInt(string(data), 10, 64)
		}
		bot := a.bot(cfg)
		ups, err := bot.GetUpdates(ctx, offset, 50)
		if err != nil {
			if ctx.Err() == nil {
				a.log.Warn("telegram getUpdates", "err", err)
				sleep(ctx, a.pollGap)
			}
			continue
		}
		for _, up := range ups {
			if up.Message != nil {
				if err := a.HandleMessage(ctx, cfg, bot, name, up.Message); err != nil {
					a.log.Warn("message", "err", err)
				}
			}
			offset = up.UpdateID + 1
		}
		if len(ups) > 0 {
			if _, err := a.panel.KVPut(ctx, keyOffset, []byte(strconv.FormatInt(offset, 10)), false, ""); err != nil {
				a.log.Warn("offset", "err", err)
			}
		}
	}
}

// RunEvents handles payment events until ctx ends; an event is acknowledged once handled.
func (a *App) RunEvents(ctx context.Context) {
	for ctx.Err() == nil {
		evs, err := a.panel.Events(ctx, []string{"payment.paid"}, 50*time.Second)
		if err != nil {
			if ctx.Err() == nil {
				a.log.Warn("events", "err", err)
				sleep(ctx, a.pollGap)
			}
			continue
		}
		if len(evs) == 0 {
			continue
		}
		cfg, err := a.LoadConfig(ctx)
		if err != nil {
			a.log.Warn("config", "err", err)
			sleep(ctx, a.pollGap)
			continue
		}
		bot := a.bot(cfg)
		var last int64
		for _, ev := range evs {
			if err := a.HandlePayment(ctx, cfg, bot, ev); err != nil {
				a.log.Warn("payment event", "id", ev.ID, "err", err)
				break // not acknowledged: comes again
			}
			last = ev.ID
		}
		if last > 0 {
			if err := a.panel.Ack(ctx, last); err != nil {
				a.log.Warn("ack", "err", err)
			}
		}
		if last != evs[len(evs)-1].ID {
			sleep(ctx, a.pollGap)
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
