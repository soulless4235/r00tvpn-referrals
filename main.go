// Command referrals is r00tvpn's refer-a-friend program as a mikan addon (protocol 2).
//
// A Telegram bot of its own hands every subscriber a personal invite link. A new customer
// who comes by one is remembered as that subscriber's friend and gets a promo code; when
// the friend first pays for a subscription (the panel's payment.paid event), the referrer's
// term is extended by the configured days. The admin sets the bot token, the code and the
// days on the addon's page in the panel.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	listen := os.Getenv("MIKAN_ADAPTER_LISTEN")
	token := os.Getenv("MIKAN_ADAPTER_TOKEN")
	panelURL := os.Getenv("MIKAN_PANEL_URL")
	panelToken := os.Getenv("MIKAN_PANEL_TOKEN")
	if listen == "" || token == "" || panelURL == "" || panelToken == "" {
		log.Error("MIKAN_ADAPTER_LISTEN, MIKAN_ADAPTER_TOKEN, MIKAN_PANEL_URL and MIKAN_PANEL_TOKEN are required")
		os.Exit(2)
	}

	// Long polls of both the panel (50 s) and Telegram (50 s) stay well inside this
	hc := &http.Client{Timeout: 70 * time.Second}
	app := NewApp(NewPanel(panelURL, panelToken, hc), "https://api.telegram.org", hc, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go app.RunEvents(ctx)
	go app.RunBot(ctx)

	srv := &http.Server{Addr: listen, Handler: app.Handler(token), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("referrals addon", "version", Version, "listen", listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
