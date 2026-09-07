package main

import (
	"context"
	"errors"
	"testing"

	"github.com/ongridio/ongrid/internal/pkg/llm/anthropicoauth"
)

// TestAnthropicOAuthConfigured 锁住「有 OAuth 凭据就该注册 provider」这条。
//
// 起因：provider 注册原本只看 APIKey。订阅登录成功之后，anthropic 依然
// 不在目录里，调用报
//
//   llm: provider "anthropic" not configured
//
// 那句话完全不提「因为没有 API key」——运维会去翻登录状态、翻 token，
// 而那些全是对的。2026-09-07 用探针凭据实测撞到。
func TestAnthropicOAuthConfigured(t *testing.T) {
	cases := []struct {
		name  string
		store anthropicoauth.Store
		want  bool
	}{
		{"没有 store", nil, false},
		{"空凭据", stubOAuthStore{}, false},
		{"只有 access token", stubOAuthStore{cred: anthropicoauth.Credential{AccessToken: "a"}}, true},
		{"只有 refresh token（过期但可刷新）", stubOAuthStore{cred: anthropicoauth.Credential{RefreshToken: "r"}}, true},
		{"读取出错时保守返回 false", stubOAuthStore{err: errors.New("boom")}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := anthropicOAuthConfigured(context.Background(), c.store); got != c.want {
				t.Errorf("anthropicOAuthConfigured() = %v, want %v", got, c.want)
			}
		})
	}
}

type stubOAuthStore struct {
	cred anthropicoauth.Credential
	err  error
}

func (s stubOAuthStore) Load(context.Context) (anthropicoauth.Credential, error) {
	return s.cred, s.err
}

func (s stubOAuthStore) Save(context.Context, anthropicoauth.Credential) error { return nil }
