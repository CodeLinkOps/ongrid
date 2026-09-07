package setting

import (
	"context"
	"reflect"
	"testing"

	settingmodel "github.com/ongridio/ongrid/internal/manager/model/setting"
)

func TestDedupeStrings(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		// The out-of-box bug: the OpenAI catalog was seeded with the
		// configured model (defaulting to gpt-4o) plus a base list that
		// already contained gpt-4o → two gpt-4o rows in the picker.
		{"out-of-box openai dup", []string{"gpt-4o", "gpt-4o", "gpt-4-turbo"}, []string{"gpt-4o", "gpt-4-turbo"}},
		{"empty entries dropped", []string{"", "a", "", "b"}, []string{"a", "b"}},
		{"order preserved, later dups dropped", []string{"b", "a", "b", "c", "a"}, []string{"b", "a", "c"}},
		{"nil -> empty", nil, []string{}},
		{"no dups untouched", []string{"x", "y"}, []string{"x", "y"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dedupeStrings(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("dedupeStrings(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestLLMSettingsResolver_EmptyStoredAPIKeyOverridesEnvironment(t *testing.T) {
	t.Parallel()

	svc := New(newFakeRepo(), nil)
	resolver := NewLLMSettingsResolver(svc, map[string]EnvProviderDefaults{
		settingmodel.LLMProviderOpenAI: {
			Label:  "OpenAI",
			APIKey: "env-key",
			Model:  "env-model",
			Models: []string{"env-model"},
		},
	}, settingmodel.LLMProviderOpenAI)

	providers, _, err := resolver.ResolveProviders(context.Background())
	if err != nil {
		t.Fatalf("ResolveProviders before override: %v", err)
	}
	if len(providers) != 1 || providers[0].ID != settingmodel.LLMProviderOpenAI {
		t.Fatalf("providers before override = %+v", providers)
	}

	if err := svc.Set(context.Background(), settingmodel.CategoryLLM, settingmodel.KeyOpenAIAPIKey, "", true); err != nil {
		t.Fatalf("Set empty override: %v", err)
	}
	providers, _, err = resolver.ResolveProviders(context.Background())
	if err != nil {
		t.Fatalf("ResolveProviders after override: %v", err)
	}
	if len(providers) != 0 {
		t.Fatalf("empty stored key did not disable env provider: %+v", providers)
	}
}

// TestLLMSettingsResolver_AltConfiguredAllowsOAuthOnlyProvider 锁住订阅登录
// 这条路。
//
// 起因：解析器按 API key 为空跳过 provider。订阅登录（OAuth）成功之后，
// anthropic 依然被跳过，调用报
//
//   llm: provider "anthropic" not configured
//
// 那句话从不提 API key —— 运维会去翻刚刚成功的登录、翻 token，而那些全是
// 对的。2026-09-07 用探针凭据实测撞到。
func TestLLMSettingsResolver_AltConfiguredAllowsOAuthOnlyProvider(t *testing.T) {
	t.Parallel()

	svc := New(newFakeRepo(), nil)
	defaults := map[string]EnvProviderDefaults{
		settingmodel.LLMProviderAnthropic: {
			Label:   "Anthropic",
			APIKey:  "", // 没有 API key —— 只有订阅登录
			Model:   "claude-sonnet-4-6",
			Models:  []string{"claude-sonnet-4-6"},
			BaseURL: "https://api.anthropic.com/v1",
		},
	}

	// 没有钩子时：跳过，行为与历史一致
	plain := NewLLMSettingsResolver(svc, defaults, settingmodel.LLMProviderAnthropic)
	got, _, err := plain.ResolveProviders(context.Background())
	if err != nil {
		t.Fatalf("ResolveProviders: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("没有钩子时不该注册：%+v", got)
	}

	// 有钩子且报告已登录时：注册，并带一个非空占位 key
	withOAuth := NewLLMSettingsResolver(svc, defaults, settingmodel.LLMProviderAnthropic).
		WithAltConfigured(func(_ context.Context, id string) bool {
			return id == settingmodel.LLMProviderAnthropic
		})
	got, _, err = withOAuth.ResolveProviders(context.Background())
	if err != nil {
		t.Fatalf("ResolveProviders: %v", err)
	}
	if len(got) != 1 || got[0].ID != settingmodel.LLMProviderAnthropic {
		t.Fatalf("订阅登录后应注册 anthropic，得到 %+v", got)
	}
	if got[0].APIKey == "" {
		t.Fatal("APIKey 为空会被下游当成未配置再次跳过 —— 必须填非空占位")
	}

	// 钩子报告未登录时：仍然跳过
	off := NewLLMSettingsResolver(svc, defaults, settingmodel.LLMProviderAnthropic).
		WithAltConfigured(func(context.Context, string) bool { return false })
	got, _, err = off.ResolveProviders(context.Background())
	if err != nil {
		t.Fatalf("ResolveProviders: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("未登录时不该注册：%+v", got)
	}
}
