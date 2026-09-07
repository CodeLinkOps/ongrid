// Package anthropicoauth implements the OAuth authorization-code flow
// Anthropic uses for subscription (non-API-key) access.
//
// # Why this exists
//
// Ongrid's anthropic provider already talks to
// https://api.anthropic.com/v1 through the OpenAI-compatible SDK, and
// that endpoint accepts an OAuth bearer token in exactly the same
// `Authorization: Bearer …` header the SDK already sends. Verified
// against the live API on 2026-09-07, including tool calling — the
// provider returns normal `tool_calls`, so Ongrid's investigator loop
// is unaffected.
//
// The only thing missing is lifetime management: access tokens expire
// in about an hour, so a static key field goes stale mid-shift and the
// AI stops working with a 401 that reads like a bad key.
//
// # The one trap worth writing down
//
// **A refresh rotates the refresh token.** Two processes sharing one
// credential will fight: whoever refreshes second gets
// `invalid_grant`, and the symptom is "the other tool suddenly can't
// log in any more" — with nothing pointing back at this one. Ongrid
// therefore runs its own login and stores its own credential; never
// copy one in from another client.
package anthropicoauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// ClientID is the public client id Anthropic publishes for the
	// authorization-code + PKCE flow. Public clients carry no secret —
	// PKCE is what binds the code to the requester.
	ClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

	authorizeURL = "https://claude.ai/oauth/authorize"
	redirectURI  = "https://console.anthropic.com/oauth/code/callback"
	scopes       = "org:create_api_key user:profile user:inference"

	// refreshSkew is how long before real expiry we treat a token as
	// stale. A request that starts 20s before expiry can easily finish
	// after it; refreshing early costs one extra round trip, letting it
	// expire costs a failed investigation.
	refreshSkew = 2 * time.Minute
)

// tokenURL is a var, not a const, purely so tests can point it at an
// httptest server. Nothing in production should assign to it.
var tokenURL = "https://console.anthropic.com/v1/oauth/token"

// Credential is what callers persist. Zero value means "not logged in".
type Credential struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// Valid reports whether the access token can still be used, accounting
// for refreshSkew.
func (c Credential) Valid(now time.Time) bool {
	return c.AccessToken != "" && now.Add(refreshSkew).Before(c.ExpiresAt)
}

// Refreshable reports whether a refresh is even possible. A credential
// with neither a live access token nor a refresh token needs a fresh
// interactive login — callers should say that plainly rather than
// retrying.
func (c Credential) Refreshable() bool { return c.RefreshToken != "" }

// LoginStart carries what the operator needs to complete a login.
type LoginStart struct {
	// URL the operator opens in a browser.
	URL string `json:"url"`
	// Verifier must be handed back to Exchange along with the code.
	// It never leaves the server in the browser flow, which is the
	// whole point of PKCE.
	Verifier string `json:"verifier"`
	State    string `json:"state"`
}

// StartLogin builds the authorize URL plus the PKCE verifier.
func StartLogin() (LoginStart, error) {
	verifier, err := randomURLSafe(32)
	if err != nil {
		return LoginStart{}, fmt.Errorf("generate verifier: %w", err)
	}
	state, err := randomURLSafe(16)
	if err != nil {
		return LoginStart{}, fmt.Errorf("generate state: %w", err)
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	q := url.Values{}
	q.Set("code", "true")
	q.Set("client_id", ClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", scopes)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)

	return LoginStart{
		URL:      authorizeURL + "?" + q.Encode(),
		Verifier: verifier,
		State:    state,
	}, nil
}

// Exchange turns the pasted authorization code into a Credential.
//
// The code the operator copies out of the browser is often of the form
// "<code>#<state>" — Anthropic appends the state with a fragment
// separator. We split it here so operators can paste verbatim instead
// of being told to trim it, which is the kind of instruction everyone
// gets wrong once.
func Exchange(ctx context.Context, hc *http.Client, code, verifier string) (Credential, error) {
	code = strings.TrimSpace(code)
	state := ""
	if i := strings.Index(code, "#"); i >= 0 {
		state = code[i+1:]
		code = code[:i]
	}
	body := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     ClientID,
		"code":          code,
		"redirect_uri":  redirectURI,
		"code_verifier": verifier,
	}
	if state != "" {
		body["state"] = state
	}
	return post(ctx, hc, body)
}

// Refresh swaps the refresh token for a new pair.
//
// **The returned credential carries a NEW refresh token — persist it.**
// Dropping it strands the login: the old refresh token is already dead
// server-side, so the next attempt fails with invalid_grant and the
// only way out is a fresh interactive login.
func Refresh(ctx context.Context, hc *http.Client, refreshToken string) (Credential, error) {
	return post(ctx, hc, map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     ClientID,
		"refresh_token": refreshToken,
	})
}

type tokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func post(ctx context.Context, hc *http.Client, body map[string]string) (Credential, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Credential{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(string(raw)))
	if err != nil {
		return Credential{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return Credential{}, fmt.Errorf("oauth token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var tr tokenResp
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return Credential{}, fmt.Errorf("decode token response (status %d): %w", resp.StatusCode, err)
	}
	if tr.Error != "" {
		// Surface Anthropic's own wording — "invalid_grant: Refresh
		// token not found or invalid" tells an operator far more than
		// a generic failure, and it is the exact string they will see
		// if a second client rotated the token out from under this one.
		return Credential{}, fmt.Errorf("oauth: %s: %s", tr.Error, tr.ErrorDesc)
	}
	if resp.StatusCode != http.StatusOK || tr.AccessToken == "" {
		return Credential{}, fmt.Errorf("oauth: unexpected response (status %d)", resp.StatusCode)
	}
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	return Credential{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    time.Now().Add(ttl),
	}, nil
}

func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
