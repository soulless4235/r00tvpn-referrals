package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Bot is the little of the Telegram Bot API the referral bot needs.
type Bot struct {
	base  string // https://api.telegram.org
	token string
	hc    *http.Client
}

// TelegramError is the Bot API's refusal.
type TelegramError struct {
	Code        int    `json:"error_code"`
	Description string `json:"description"`
}

func (e *TelegramError) Error() string { return fmt.Sprintf("telegram %d: %s", e.Code, e.Description) }

func (b *Bot) call(ctx context.Context, method string, params any, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(b.base, "/")+"/bot"+b.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.hc.Do(req)
	if err != nil {
		// the URL holds the token: never let it reach a log
		return fmt.Errorf("telegram %s: %w", method, unwrapURL(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		TelegramError
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("telegram %s: status %d", method, resp.StatusCode)
	}
	if !envelope.OK {
		return &envelope.TelegramError
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(envelope.Result, out)
}

// unwrapURL drops the request URL (and the token in it) from a transport error.
func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// Me is the bot's own account.
type Me struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

func (b *Bot) GetMe(ctx context.Context) (Me, error) {
	var me Me
	return me, b.call(ctx, "getMe", map[string]any{}, &me)
}

// Update is an incoming update; only private messages matter here.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

type Message struct {
	Text string `json:"text"`
	From *struct {
		ID           int64  `json:"id"`
		IsBot        bool   `json:"is_bot"`
		LanguageCode string `json:"language_code"`
	} `json:"from"`
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
}

func (b *Bot) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	var ups []Update
	err := b.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": timeout, "allowed_updates": []string{"message"},
	}, &ups)
	return ups, err
}

// Button is an inline button that opens a link.
type Button struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

// Send writes text (HTML) to chat, with a row of buttons each when buttons are given.
func (b *Bot) Send(ctx context.Context, chat int64, text string, buttons ...Button) error {
	params := map[string]any{
		"chat_id": chat, "text": text, "parse_mode": "HTML",
		"link_preview_options": map[string]bool{"is_disabled": true},
	}
	if len(buttons) > 0 {
		rows := make([][]Button, 0, len(buttons))
		for _, btn := range buttons {
			rows = append(rows, []Button{btn})
		}
		params["reply_markup"] = map[string]any{"inline_keyboard": rows}
	}
	return b.call(ctx, "sendMessage", params, nil)
}
