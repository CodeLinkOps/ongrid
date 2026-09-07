package alert

import (
	"testing"

	model "github.com/ongridio/ongrid/internal/manager/model/alert"
)

// TestSeededBuiltinRulesPassTheirOwnValidator 防的是一类容易复发的错：
// 种子把规则写进库时不经过 buildRuleRow，所以「能种下」和「能改」是两条
// 不同的路。两者一旦分叉，表现是内置规则在界面上改不了，而报错说的是
// scope_type 不合法 —— 看起来像用户填错了。
//
// 具体案例：scrape_down 用 monitoring_pipeline（抑制逻辑依赖这个 scope），
// 而 allowedScopesForKind(metric_raw) 一度只返回 {global, host}。
func TestSeededBuiltinRulesPassTheirOwnValidator(t *testing.T) {
	cases := []struct {
		name  string
		kind  string
		scope string
	}{
		{"scrape_down", model.RuleKindMetricRaw, model.RuleScopeMonitoringPipeline},
		{"device_offline", model.RuleKindMetricRaw, model.RuleScopeGlobal},
		{"cpu_high", model.RuleKindMetricRaw, model.RuleScopeHost},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !scopeAllowedForKind(c.scope, c.kind) {
				t.Fatalf("内置规则 %s 用 scope=%q kind=%q 种下，但校验器不接受它 —— "+
					"这条规则将无法被编辑，允许的是 %v",
					c.name, c.scope, c.kind, allowedScopesForKind(c.kind))
			}
		})
	}
}
