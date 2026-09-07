// Package slack is the Slack provider for the IM bridge (companion to
// telegram/feishu). It uses Slack Socket Mode for inbound (manager opens
// an outbound WebSocket — no public ingress required, traverses the
// manager's HTTPS_PROXY same as Telegram getUpdates) and the standard
// Web API (chat.postMessage / chat.update) for outbound. Slack needs two
// tokens:
//
//   - app_token (xapp-...) — app-level token, only used to call
//     apps.connections.open and authenticate the WebSocket.
//   - bot_token (xoxb-...) — bot user token, used for chat.postMessage,
//     chat.update, and any other Web API call we make.
//
// Tokens come out of model.ImApp.AppSecret as JSON
// {"app_token":"...","bot_token":"..."} — see ParseSecret.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ongridio/ongrid/internal/manager/biz/imbridge/imformat"
)

// SecretFields is the parsed shape of ImApp.AppSecret for a slack provider.
// Stored as JSON so we can grow tokens (e.g. user token, signing secret
// when we add Events-API webhook mode) without changing the column type.
type SecretFields struct {
	AppToken string `json:"app_token"`
	BotToken string `json:"bot_token"`
}

// ParseSecret decodes ImApp.AppSecret into the two-token shape. Tolerant
// of leading / trailing whitespace and BOM; rejects empty tokens up front
// so the UI gets a clean error before we ever hit Slack. A bare token
// (legacy / mis-pasted "xapp-..." with no JSON envelope) is also rejected
// — the form should validate this, but we double-check so we don't burn
// retries on a misconfigured app.
func ParseSecret(raw string) (SecretFields, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return SecretFields{}, fmt.Errorf("slack app_secret is empty")
	}
	var s SecretFields
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return SecretFields{}, fmt.Errorf("slack app_secret must be JSON {app_token, bot_token}: %w", err)
	}
	s.AppToken = strings.TrimSpace(s.AppToken)
	s.BotToken = strings.TrimSpace(s.BotToken)
	if s.AppToken == "" {
		return SecretFields{}, fmt.Errorf("slack app_secret: app_token missing")
	}
	if s.BotToken == "" {
		return SecretFields{}, fmt.Errorf("slack app_secret: bot_token missing")
	}
	if !strings.HasPrefix(s.AppToken, "xapp-") {
		return SecretFields{}, fmt.Errorf("slack app_token must start with xapp- (Slack app-level token), got %q", redact(s.AppToken))
	}
	if !strings.HasPrefix(s.BotToken, "xoxb-") {
		return SecretFields{}, fmt.Errorf("slack bot_token must start with xoxb- (Slack bot user token), got %q", redact(s.BotToken))
	}
	return s, nil
}

func redact(s string) string {
	if len(s) <= 6 {
		return s
	}
	return s[:6] + "…"
}

// Client is a minimal Slack Web API + Socket Mode client. It uses a
// zero-value http.Client so it honors HTTPS_PROXY / NO_PROXY env (Slack
// from mainland China typically goes through the operator's proxy same as
// Telegram). Per-call timeouts come from the caller's context.
type Client struct {
	appToken string
	botToken string
	hc       *http.Client
	base     string // API root; overridable for tests, defaults to public Slack API
}

// NewClient builds a client for one Slack app (one app_token + bot_token
// pair). Use NewClientFromSecret if you only have the raw AppSecret JSON.
func NewClient(appToken, botToken string) *Client {
	return &Client{
		appToken: appToken,
		botToken: botToken,
		hc:       &http.Client{},
		base:     "https://slack.com/api",
	}
}

// NewClientFromSecret is the convenience wrapper the imbridge stream
// factory uses — parses the JSON secret then constructs.
func NewClientFromSecret(rawSecret string) (*Client, error) {
	s, err := ParseSecret(rawSecret)
	if err != nil {
		return nil, err
	}
	return NewClient(s.AppToken, s.BotToken), nil
}

// SetBaseURL is the test seam — point at an httptest server. Production
// code uses the default.
func (c *Client) SetBaseURL(u string) { c.base = strings.TrimRight(u, "/") }

// AuthTest returns the bot's own Slack user id.
//
// Needed so inbound text can drop the bot's own @-mention: in a channel
// every message addressed to us starts with `<@U…>`, and that markup is
// **addressing, not content**. Left in, the model spends its turn asking
// what `U0BV8744CSF` refers to instead of answering the question — seen
// in production 2026-09-07.
func (c *Client) AuthTest(ctx context.Context) (userID string, err error) {
	var out struct {
		UserID string `json:"user_id"`
	}
	if err := c.call(ctx, "auth.test", c.botToken, struct{}{}, &out); err != nil {
		return "", err
	}
	return out.UserID, nil
}

// Probe validates both tokens required by Socket Mode without sending a message.
func (c *Client) Probe(ctx context.Context) error {
	if err := c.call(ctx, "auth.test", c.botToken, struct{}{}, nil); err != nil {
		return fmt.Errorf("slack bot token: %w", err)
	}
	if _, err := c.OpenConnection(ctx); err != nil {
		return fmt.Errorf("slack app token: %w", err)
	}
	return nil
}

// apiResp is the common envelope for every Slack Web API call. ok=false
// always carries an Error message; we surface it so the caller sees the
// real Slack reason (e.g. "channel_not_found", "invalid_auth") rather
// than a generic "unexpected status".
type apiResp struct {
	OK               bool            `json:"ok"`
	Error            string          `json:"error"`
	Warning          string          `json:"warning"`
	ResponseMetadata json.RawMessage `json:"response_metadata,omitempty"`
}

// call POSTs a JSON body to /api/<method> with the given bearer token and
// decodes the envelope. Slack signals errors via ok=false in the response
// body (HTTP 200 even for app-level errors), so we always read the body
// then inspect ok. dst, if non-nil, gets the full body unmarshaled into it
// so callers can pick out method-specific fields without a second decode.
func (c *Client) call(ctx context.Context, method, token string, body any, dst any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/"+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("slack %s: %w", method, err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack %s: status %s body=%s", method, resp.Status, truncate(raw, 200))
	}
	var env apiResp
	if uerr := json.Unmarshal(raw, &env); uerr != nil {
		return fmt.Errorf("slack %s: decode envelope: %w (body=%s)", method, uerr, truncate(raw, 200))
	}
	if !env.OK {
		return fmt.Errorf("slack %s: %s", method, env.Error)
	}
	if dst != nil {
		if uerr := json.Unmarshal(raw, dst); uerr != nil {
			return fmt.Errorf("slack %s: decode result: %w", method, uerr)
		}
	}
	return nil
}

// PostMessage creates a new text message in channel and returns the
// platform message id (Slack's `ts` field, a high-precision float as
// string — DON'T parse it, just round-trip the string verbatim through
// chat.update). channel accepts a public channel id (C…), private group
// (G…), DM (D…), or a user id for DMs.
func (c *Client) PostMessage(ctx context.Context, channel, text string) (string, error) {
	var resp struct {
		apiResp
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	body := nativeMessageBody(channel, text)
	if err := c.call(ctx, "chat.postMessage", c.botToken, body, &resp); err != nil {
		return "", err
	}
	if resp.TS == "" {
		return "", fmt.Errorf("slack chat.postMessage: empty ts in response")
	}
	return resp.TS, nil
}

// UpdateMessage replaces the text of a previously-posted message. Slack
// silently accepts a no-op (same text) and returns ok=true, so we don't
// need the Telegram "message is not modified" swallow.
func (c *Client) UpdateMessage(ctx context.Context, channel, ts, text string) error {
	body := nativeMessageBody(channel, text)
	body["ts"] = ts
	return c.call(ctx, "chat.update", c.botToken, body, nil)
}

// nativeMessageBody uses Block Kit for the visible message and keeps a plain
// top-level text fallback for notifications and clients that omit blocks.
func nativeMessageBody(channel, markdown string) map[string]any {
	sections := imformat.SlackSections(markdown)
	blocks := make([]map[string]any, 0, len(sections))
	for _, section := range sections {
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]string{
				"type": "mrkdwn",
				"text": section,
			},
		})
	}
	return map[string]any{
		"channel": channel,
		"text":    imformat.PlainExcerpt(markdown, 4000),
		"blocks":  blocks,
	}
}

// OpenConnection asks Slack for a fresh Socket Mode WebSocket URL. The
// returned URL is a short-lived (a few minutes) ticket — callers must dial
// it immediately. apps.connections.open uses the APP token, not the bot
// token; this is the one place app_token gets used.
func (c *Client) OpenConnection(ctx context.Context) (string, error) {
	var resp struct {
		apiResp
		URL string `json:"url"`
	}
	if err := c.call(ctx, "apps.connections.open", c.appToken, struct{}{}, &resp); err != nil {
		return "", err
	}
	if resp.URL == "" {
		return "", fmt.Errorf("slack apps.connections.open: empty url in response")
	}
	return resp.URL, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// ack/poll timeout constants used by the stream client. Defined here so
// the test package can read them without duplicating values.
const (
	// DialTimeout caps how long we wait for the WebSocket TLS handshake.
	// Slack's wss endpoint is generally fast (<5s) — a longer wait means
	// either DNS / network or the operator's proxy is wedged.
	DialTimeout = 10 * time.Second
)

// ThreadParent returns the text of the message a thread hangs off.
//
// # Why the bot needs this
//
// Operators reply **inside the thread under an alert card** and ask
// "这是啥情况". That is the natural thing to do, and it is also the one
// case where the bot has no idea what is being discussed: a Slack
// thread maps to its own Ongrid session, which starts empty. Observed
// in production 2026-09-07 — the bot answered a question about an
// alert it could not see.
//
// # Scope
//
// Needs `channels:history` (public) or `groups:history` (private). An
// app installed before this feature will not have them, and Slack
// answers `missing_scope`. Callers should treat that as "no context
// available" rather than an error — the reply still works, it is just
// less informed — and surface the scope name once so the operator can
// fix it.
func (c *Client) ThreadParent(ctx context.Context, channel, threadTS string) (string, error) {
	var out struct {
		Messages []struct {
			Text string `json:"text"`
			TS   string `json:"ts"`
		} `json:"messages"`
	}
	body := map[string]any{
		"channel": channel,
		"ts":      threadTS,
		"limit":   1,
		"inclusive": true,
	}
	if err := c.call(ctx, "conversations.replies", c.botToken, body, &out); err != nil {
		return "", err
	}
	if len(out.Messages) == 0 {
		return "", nil
	}
	return out.Messages[0].Text, nil
}
