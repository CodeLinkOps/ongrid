package anthropicoauth

import (
	"context"
	"strconv"
	"time"
)

// SecretName is the managed-secret row this package reads and writes.
// Managed (not operator-created) so it does not clutter the secrets
// page and cannot be edited by hand into an inconsistent state.
const SecretName = "anthropic-oauth"

// SecretFields are the field keys inside that row.
const (
	FieldAccess  = "access_token"
	FieldRefresh = "refresh_token"
	FieldExpires = "expires_at_unix"
)

// SecretResolver is the slice of the secret usecase this store needs.
// Declared here (consumer side) so the llm package does not drag in the
// whole secret biz surface.
type SecretResolver interface {
	ResolveFields(ctx context.Context, name string) (map[string]string, error)
	CreateManaged(ctx context.Context, name, credType, description string, fields map[string]string) error
}

// SecretStore persists the credential in Ongrid's managed secret store.
type SecretStore struct{ Secrets SecretResolver }

// Load returns the zero Credential when nothing is stored yet — "not
// logged in" is a normal state, not an error.
func (s SecretStore) Load(ctx context.Context) (Credential, error) {
	if s.Secrets == nil {
		return Credential{}, nil
	}
	f, err := s.Secrets.ResolveFields(ctx, SecretName)
	if err != nil || f == nil {
		return Credential{}, nil //nolint:nilerr // absent == not logged in
	}
	exp, _ := strconv.ParseInt(f[FieldExpires], 10, 64)
	return Credential{
		AccessToken:  f[FieldAccess],
		RefreshToken: f[FieldRefresh],
		ExpiresAt:    time.Unix(exp, 0),
	}, nil
}

// Save overwrites the row. CreateManaged is upsert-shaped, which is what
// we want: every refresh rewrites all three fields together so the
// access token and its expiry can never disagree.
func (s SecretStore) Save(ctx context.Context, c Credential) error {
	if s.Secrets == nil {
		return nil
	}
	return s.Secrets.CreateManaged(ctx, SecretName, "oauth",
		"Anthropic OAuth（订阅登录）。由 Ongrid 自己刷新，不要手工编辑 —— "+
			"刷新会轮换 refresh token，手改会导致下次刷新 invalid_grant。",
		map[string]string{
			FieldAccess:  c.AccessToken,
			FieldRefresh: c.RefreshToken,
			FieldExpires: strconv.FormatInt(c.ExpiresAt.Unix(), 10),
		})
}
