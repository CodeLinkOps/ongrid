package setting

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ongridio/ongrid/internal/pkg/errs"
	"github.com/ongridio/ongrid/internal/pkg/llm/anthropicoauth"
)

// OAuthStore is the persistence surface the login endpoints need.
type OAuthStore interface {
	Load(ctx context.Context) (anthropicoauth.Credential, error)
	Save(ctx context.Context, cred anthropicoauth.Credential) error
}

// RegisterAnthropicOAuth attaches the subscription-login endpoints.
//
// Two steps, because the browser half happens outside Ongrid:
//
//	POST /v1/system-settings/anthropic-oauth/start     → { url, verifier }
//	POST /v1/system-settings/anthropic-oauth/complete  ← { code, verifier }
//	GET  /v1/system-settings/anthropic-oauth/status    → { logged_in, expires_at }
//
// The verifier round-trips through the caller instead of being held in
// server memory. That keeps the manager stateless across restarts —
// a login started before a redeploy still completes afterwards — and
// the verifier is worthless on its own: PKCE binds it to the code,
// and the code is single-use.
func (h *Handler) RegisterAnthropicOAuth(r chi.Router, store OAuthStore) {
	if store == nil {
		return
	}
	r.Post("/v1/system-settings/anthropic-oauth/start", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAdmin(w, r); !ok {
			return
		}
		ls, err := anthropicoauth.StartLogin()
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, ls)
	})

	r.Post("/v1/system-settings/anthropic-oauth/complete", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAdmin(w, r); !ok {
			return
		}
		var req struct {
			Code     string `json:"code"`
			Verifier string `json:"verifier"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, errs.ErrInvalid)
			return
		}
		if req.Code == "" || req.Verifier == "" {
			writeErr(w, errs.ErrInvalid)
			return
		}
		cred, err := anthropicoauth.Exchange(r.Context(), nil, req.Code, req.Verifier)
		if err != nil {
			writeErr(w, err)
			return
		}
		if err := store.Save(r.Context(), cred); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"logged_in":  true,
			"expires_at": cred.ExpiresAt,
		})
	})

	r.Get("/v1/system-settings/anthropic-oauth/status", func(w http.ResponseWriter, r *http.Request) {
		cred, err := store.Load(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		// Never return the tokens themselves. "Is it working and until
		// when" is all an operator needs, and it is all the UI shows.
		writeJSON(w, http.StatusOK, map[string]any{
			"logged_in":   cred.AccessToken != "" || cred.Refreshable(),
			"valid_now":   cred.Valid(time.Now()),
			"refreshable": cred.Refreshable(),
			"expires_at":  cred.ExpiresAt,
		})
	})
}

