package anthropicoauth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Store persists the credential. Implementations must be safe for
// concurrent use.
//
// Save is called after every refresh because **the refresh rotates the
// refresh token**. An implementation that silently drops the write
// turns a working login into a one-hour login: the old refresh token is
// dead the moment the new one is issued, so the next refresh fails with
// invalid_grant and the operator has to log in again — with nothing in
// the logs pointing at the missing write.
type Store interface {
	Load(ctx context.Context) (Credential, error)
	Save(ctx context.Context, cred Credential) error
}

// Transport injects a fresh OAuth bearer on every outbound request.
//
// Shape deliberately mirrors zhipuJWTTransport in the parent package:
// the SDK's static apiKey becomes irrelevant because this always
// overrides Authorization. Keeping the two transports similar means one
// reading teaches both.
type Transport struct {
	Store Store
	Base  http.RoundTripper
	// HTTPClient is used for the token endpoint itself. Left nil it
	// falls back to http.DefaultClient. Kept separate from Base so a
	// test can stub the token endpoint without also intercepting the
	// model call.
	HTTPClient *http.Client
	// Now is injectable for tests.
	Now func() time.Time

	// mu serializes refreshes. Without it a burst of concurrent
	// investigations would each fire their own refresh, and since every
	// refresh rotates the token, all but one would be invalidated
	// immediately — the classic thundering-herd-meets-rotation bug.
	mu sync.Mutex
}

func (t *Transport) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *Transport) base() http.RoundTripper {
	if t.Base != nil {
		return t.Base
	}
	return http.DefaultTransport
}

// token returns a usable access token, refreshing when needed.
//
// force skips the freshness check. Used after a 401: the token looked
// valid by its expiry but the server rejected it anyway, which happens
// when it was revoked or rotated out from under us.
func (t *Transport) token(ctx context.Context, force bool) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	cred, err := t.Store.Load(ctx)
	if err != nil {
		return "", fmt.Errorf("load oauth credential: %w", err)
	}
	if !force && cred.Valid(t.now()) {
		return cred.AccessToken, nil
	}
	if !cred.Refreshable() {
		// Say what to do, not just what failed. This message is what an
		// operator sees when the AI stops answering.
		return "", fmt.Errorf("anthropic oauth: not logged in (no refresh token) — run the login flow again")
	}
	fresh, err := Refresh(ctx, t.HTTPClient, cred.RefreshToken)
	if err != nil {
		return "", fmt.Errorf("anthropic oauth refresh: %w", err)
	}
	if fresh.RefreshToken == "" {
		// Defensive: never blank out a working refresh token because a
		// response omitted it.
		fresh.RefreshToken = cred.RefreshToken
	}
	if err := t.Store.Save(ctx, fresh); err != nil {
		// Refusing here is deliberate. The rotation already happened
		// server-side, so continuing with an unsaved token would work
		// for this one call and then lock us out permanently — a
		// failure that surfaces an hour later, far from its cause.
		return "", fmt.Errorf("persist refreshed oauth credential: %w", err)
	}
	return fresh.AccessToken, nil
}

// RoundTrip implements http.RoundTripper.
//
// On a 401 it refreshes once and retries. Expiry-based refresh alone is
// not enough: a token can be rejected while still looking fresh — the
// usual cause is that another client logged in and rotated it. Without
// the retry the AI stays broken until the local expiry passes, which
// can be the better part of an hour, and the only symptom is a 401 that
// looks like a configuration problem.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.attempt(req, false)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	// Drain and close so the connection can be reused for the retry.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()

	retried, rerr := t.attempt(req, true)
	if rerr != nil {
		// Report the refresh failure, not the original 401 — the
		// refresh error carries Anthropic's own wording and is what
		// actually needs fixing.
		return nil, rerr
	}
	return retried, nil
}

func (t *Transport) attempt(req *http.Request, force bool) (*http.Response, error) {
	tok, err := t.token(req.Context(), force)
	if err != nil {
		return nil, err
	}
	// Clone first — Go's RoundTripper contract forbids mutating the
	// request the caller passed in.
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Bearer "+tok)
	return t.base().RoundTrip(cloned)
}
