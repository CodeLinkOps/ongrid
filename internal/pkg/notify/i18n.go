package notify

import "sync/atomic"

// 通知卡片的字段名本地化。
//
// **为什么放在 notify 包里而不是复用 web/src/i18n**：那份是前端的，
// 通知是后端直接发给 Slack / 飞书的，两条路不共用任何东西。
//
// **为什么是包级变量**：locale 在启动时定一次，之后只读。senders 是在
// 各处按需构造的（NewSlackSender 等有六七个构造函数），把 locale 逐个
// 穿进去会改一大片调用点，而收益只是避免一个启动期写入的全局值。
// 这与本仓库 internal/pkg/prom 的做法一致。
//
// 未在表中的 locale 回退到英文 —— 缺翻译时宁可显示英文，也不要显示
// 一个空字段名（那会让卡片看起来像坏了）。
var defaultLocale atomic.Value // string

// SetDefaultLocale 在启动时调用一次，来源是 ONGRID_DEFAULT_LOCALE。
//
// **必须在任何 Sender 开始发消息之前调用。** 运行期改这个值不会崩，
// 但会让同一批告警里一部分中文一部分英文，看起来像随机行为。
func SetDefaultLocale(loc string) {
	if loc == "" {
		loc = "en"
	}
	defaultLocale.Store(loc)
}

func currentLocale() string {
	if v, ok := defaultLocale.Load().(string); ok && v != "" {
		return v
	}
	return "en"
}

// cardLabels 是各语言的字段名表。key 是稳定的内部标识，不要拿显示文本
// 当 key —— 那样改一次英文文案就要同步改所有语言。
var cardLabels = map[string]map[string]string{
	"zh-CN": {
		"severity":   "级别",
		"source":     "来源",
		"rule":       "规则",
		"incident":   "事件",
		"device":     "设备",
		"dedupe_key": "去重键",
	},
	"zh-Hans": {
		"severity":   "级别",
		"source":     "来源",
		"rule":       "规则",
		"incident":   "事件",
		"device":     "设备",
		"dedupe_key": "去重键",
	},
}

// enLabels 同时充当回退表和「有哪些 key」的唯一真相。
var enLabels = map[string]string{
	"severity":   "Severity",
	"source":     "Source",
	"rule":       "Rule",
	"incident":   "Incident",
	"device":     "Device",
	"dedupe_key": "Dedupe key",
}

// label 返回当前 locale 下的字段名，缺失时回退英文。
func label(key string) string {
	if tbl, ok := cardLabels[currentLocale()]; ok {
		if v, ok := tbl[key]; ok && v != "" {
			return v
		}
	}
	if v, ok := enLabels[key]; ok {
		return v
	}
	return key
}
