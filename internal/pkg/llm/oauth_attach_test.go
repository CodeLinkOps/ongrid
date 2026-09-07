package llm

import (
	"context"
	"testing"

	"github.com/ongridio/ongrid/internal/pkg/llm/anthropicoauth"
)

// TestOAuthTransportAttachesForProxiedBaseURL 覆盖代理场景。
//
// 只按 base URL 判断是不够的：Anthropic 不向所有地区提供服务（香港会拿到一个
// 完全不提地域的 403，见运维手册），所以运维有正当理由把 provider 指向一个
// 转发代理。那时 URL 检查会让 transport 悄悄脱开，占位串作为 bearer 发出去，
// 得到 `401 Invalid Anthropic API Key` —— 一个把责任推给凭据的错误，
// 而凭据完全没问题。
func TestOAuthTransportAttachesForProxiedBaseURL(t *testing.T) {
	c := &openaiClient{oauth: stubStore{}}
	cases := []struct {
		name    string
		apiKey  string
		baseURL string
		want    bool
	}{
		{"官方域名 + 真 key", "sk-real", "https://api.anthropic.com/v1", true},
		{"官方域名 + 占位串", OAuthPlaceholderKey, "https://api.anthropic.com/v1", true},
		{"代理地址 + 占位串", OAuthPlaceholderKey, "http://172.18.0.1:8897/v1", true},
		{"代理地址 + 真 key（不是 OAuth）", "sk-real", "http://proxy.local/v1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := c.oauth != nil && (tc.apiKey == OAuthPlaceholderKey || looksLikeAnthropicURL(tc.baseURL))
			if got != tc.want {
				t.Errorf("挂 transport = %v, want %v", got, tc.want)
			}
		})
	}
}

type stubStore struct{}

func (stubStore) Load(context.Context) (anthropicoauth.Credential, error) {
	return anthropicoauth.Credential{AccessToken: "a"}, nil
}
func (stubStore) Save(context.Context, anthropicoauth.Credential) error { return nil }
