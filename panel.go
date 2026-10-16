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
	"strconv"
	"strings"
	"time"
)

// Panel is the mikan Panel API (protocol 2) as this addon uses it: its key-value store, the
// payment events, the subscribers and their grants.
type Panel struct {
	base, token string
	hc          *http.Client
}

func NewPanel(base, token string, hc *http.Client) *Panel {
	return &Panel{base: strings.TrimRight(base, "/"), token: token, hc: hc}
}

// PanelError is the panel's refusal: {"code", "message"} with the HTTP status.
type PanelError struct {
	Status  int
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *PanelError) Error() string {
	return fmt.Sprintf("panel %d %s: %s", e.Status, e.Code, e.Message)
}

// IsStatus reports whether err is the panel's answer with that status.
func IsStatus(err error, status int) bool {
	var pe *PanelError
	return errors.As(err, &pe) && pe.Status == status
}

// call sends one request and returns the body of a 2xx answer, or a *PanelError.
func (p *Panel) call(ctx context.Context, method, path string, body []byte, hdr http.Header) (*http.Response, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base+path, rd)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode/100 != 2 {
		pe := &PanelError{Status: resp.StatusCode}
		_ = json.Unmarshal(data, pe)
		return resp, data, pe
	}
	return resp, data, nil
}

// ErrNoKey: the key is not in the store.
var ErrNoKey = errors.New("no such key")

// KVGet is a value and its version; ErrNoKey when absent.
func (p *Panel) KVGet(ctx context.Context, key string) ([]byte, string, error) {
	resp, data, err := p.call(ctx, http.MethodGet, "/kv/"+key, nil, nil)
	if IsStatus(err, http.StatusNotFound) {
		return nil, "", ErrNoKey
	}
	if err != nil {
		return nil, "", err
	}
	return data, strings.Trim(resp.Header.Get("ETag"), `"`), nil
}

// KVPut stores value under key. ifAbsent: only when the key is not there (else conflict).
// version: only over that version (else conflict). The answer is the new version.
func (p *Panel) KVPut(ctx context.Context, key string, value []byte, ifAbsent bool, version string) (string, error) {
	path := "/kv/" + key
	if ifAbsent {
		path += "?if_absent=1"
	}
	hdr := http.Header{"Content-Type": {"application/octet-stream"}}
	if version != "" {
		hdr.Set("If-Match", `"`+version+`"`)
	}
	_, data, err := p.call(ctx, http.MethodPut, path, value, hdr)
	if err != nil {
		return "", err
	}
	var out struct {
		Version json.RawMessage `json:"version"`
	}
	_ = json.Unmarshal(data, &out)
	return strings.Trim(string(out.Version), `"`), nil
}

// ErrConflict: the key was there (if_absent) or at another version (If-Match).
func IsConflict(err error) bool {
	return IsStatus(err, http.StatusConflict) || IsStatus(err, http.StatusPreconditionFailed)
}

// KVKeys is every key under prefix, up to limit.
func (p *Panel) KVKeys(ctx context.Context, prefix string, limit int) ([]string, error) {
	var keys []string
	after := ""
	for len(keys) < limit {
		q := url.Values{"prefix": {prefix}, "limit": {"1000"}}
		if after != "" {
			q.Set("after", after)
		}
		_, data, err := p.call(ctx, http.MethodGet, "/kv?"+q.Encode(), nil, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Keys []string `json:"keys"`
			Next *string  `json:"next"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, err
		}
		keys = append(keys, page.Keys...)
		if page.Next == nil || *page.Next == "" {
			break
		}
		after = *page.Next
	}
	if len(keys) > limit {
		keys = keys[:limit]
	}
	return keys, nil
}

// Event is one of the panel's events.
type Event struct {
	ID     int64           `json:"id"`
	Type   string          `json:"type"`
	At     time.Time       `json:"at"`
	UserID int64           `json:"user_id"`
	TgID   int64           `json:"tg_id"`
	Data   json.RawMessage `json:"data"`
}

// Events waits up to wait for events of the types after the addon's cursor.
func (p *Panel) Events(ctx context.Context, types []string, wait time.Duration) ([]Event, error) {
	q := url.Values{"types": {strings.Join(types, ",")}, "limit": {"100"}, "wait": {strconv.Itoa(int(wait.Seconds()))}}
	_, data, err := p.call(ctx, http.MethodGet, "/events?"+q.Encode(), nil, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Events []Event `json:"events"`
	}
	return out.Events, json.Unmarshal(data, &out)
}

// Ack moves the addon's cursor past event id.
func (p *Panel) Ack(ctx context.Context, id int64) error {
	body, _ := json.Marshal(map[string]int64{"id": id})
	_, _, err := p.call(ctx, http.MethodPost, "/events/ack", body, nil)
	return err
}

// User is a subscriber as the panel lists them.
type User struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	State      string     `json:"state"` // active, expiring, limited, expired, disabled
	TariffName string     `json:"tariff_name"`
	ExpiresAt  *time.Time `json:"expires_at"`
	TgID       *int64     `json:"tg_id"`
}

// UsersOf is the subscriptions linked to the Telegram account tg.
func (p *Panel) UsersOf(ctx context.Context, tg int64) ([]User, error) {
	q := url.Values{"q": {strconv.FormatInt(tg, 10)}, "limit": {"100"}}
	_, data, err := p.call(ctx, http.MethodGet, "/users?"+q.Encode(), nil, nil)
	if err != nil {
		return nil, err
	}
	var page struct {
		Users []User `json:"users"`
	}
	if err := json.Unmarshal(data, &page); err != nil {
		return nil, err
	}
	// the search also matches names and ids: keep the account's own
	var own []User
	for _, u := range page.Users {
		if u.TgID != nil && *u.TgID == tg {
			own = append(own, u)
		}
	}
	return own, nil
}

// Grant is what a grant of days answered.
type Grant struct {
	UserID    int64      `json:"user_id"`
	Days      int64      `json:"days"`
	ExpiresAt *time.Time `json:"expires_at"`
	Repeat    bool       `json:"repeat"`
}

// GrantDays extends user's term by days. key names the grant: a repeat answers the first
// grant again instead of giving twice.
func (p *Panel) GrantDays(ctx context.Context, user, days int64, reason, key string) (Grant, error) {
	body, _ := json.Marshal(map[string]any{"days": days, "reason": reason, "key": key})
	_, data, err := p.call(ctx, http.MethodPost, "/users/"+strconv.FormatInt(user, 10)+"/grant", body, nil)
	if err != nil {
		return Grant{}, err
	}
	var g Grant
	return g, json.Unmarshal(data, &g)
}
