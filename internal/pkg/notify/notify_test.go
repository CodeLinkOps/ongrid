package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRouterSendUsesDefaultChannels(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type = %q", r.Header.Get("Content-Type"))
		}
		var payload Message
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		if payload.Subject != "CPU high" {
			t.Errorf("subject = %q", payload.Subject)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	router := NewRouter(
		true,
		time.Second,
		[]string{"webhook"},
		NewGenericWebhookSender("webhook", srv.URL+"/notify", "secret", srv.Client()),
	)
	err := router.Send(context.Background(), Message{
		Subject:  "CPU high",
		Severity: SeverityWarning,
		Source:   "alert",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotPath != "/notify" {
		t.Errorf("path = %q, want /notify", gotPath)
	}
}

func TestRouterSendUnknownChannel(t *testing.T) {
	router := NewRouter(true, time.Second, []string{"missing"})
	err := router.Send(context.Background(), Message{Subject: "x"})
	if err == nil || !strings.Contains(err.Error(), `channel "missing" not configured`) {
		t.Fatalf("err = %v", err)
	}
}

func TestRouterDisabledDropsMessage(t *testing.T) {
	router := NewRouter(false, time.Second, []string{"missing"})
	err := router.Send(context.Background(), Message{Subject: "x"})
	if err != nil {
		t.Fatalf("Send disabled: %v", err)
	}
}

func TestFeishuSenderSignsPayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		if payload["timestamp"] == "" {
			t.Errorf("timestamp missing")
		}
		if payload["sign"] == "" {
			t.Errorf("sign missing")
		}
		if payload["msg_type"] != "text" {
			t.Errorf("msg_type = %v", payload["msg_type"])
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := NewFeishuSender("feishu", srv.URL, "secret", srv.Client())
	err := sender.Send(context.Background(), Message{Subject: "edge offline"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func TestDingTalkSenderSignsURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("timestamp") == "" {
			t.Errorf("timestamp query missing")
		}
		if r.URL.Query().Get("sign") == "" {
			t.Errorf("sign query missing")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := NewDingTalkSender("dingtalk", srv.URL, "secret", srv.Client())
	err := sender.Send(context.Background(), Message{Subject: "edge offline"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
}

// TestSlackSenderEmitsAttachments locks the new Slack payload shape:
// the operator sees a colored side-bar + structured fields (Severity /
// Source / Rule / Incident / Device / Dedupe) instead of an unstyled
// paragraph. The top-level "text" stays populated as Slack's fallback
// preview (push, email digest, sidebar) for clients that strip attachments.
func TestSlackSenderEmitsAttachments(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := NewSlackSender("slack-ops", srv.URL, srv.Client())
	occurred := time.Date(2026, 5, 30, 10, 0, 0, 0, time.UTC)
	err := sender.Send(context.Background(), Message{
		Subject:    "Swap usage 84% on VM-4-8",
		Severity:   SeverityCritical,
		Source:     "alert-evaluator",
		DedupeKey:  "pipeline:swap_high:device_id=1",
		OccurredAt: occurred,
		Labels: map[string]string{
			"rule":        "swap_high",
			"incident_id": "42",
			"device_id":   "1",
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Fallback text: Slack uses this for push / sidebar / email digest.
	if want := "[CRITICAL] Swap usage 84% on VM-4-8"; got["text"] != want {
		t.Errorf("text = %q, want %q", got["text"], want)
	}

	atts, ok := got["attachments"].([]any)
	if !ok || len(atts) != 1 {
		t.Fatalf("attachments = %v, want exactly 1", got["attachments"])
	}
	att := atts[0].(map[string]any)

	if att["color"] != "#d92f2f" {
		t.Errorf("color = %v, want #d92f2f", att["color"])
	}
	if att["title"] != "Swap usage 84% on VM-4-8" {
		t.Errorf("title = %v", att["title"])
	}
	if att["footer"] != "ongrid" {
		t.Errorf("footer = %v, want ongrid", att["footer"])
	}
	if got, want := att["ts"], float64(occurred.Unix()); got != want {
		t.Errorf("ts = %v, want %v", got, want)
	}

	fields, ok := att["fields"].([]any)
	if !ok {
		t.Fatalf("fields missing or wrong type: %v", att["fields"])
	}
	want := map[string]string{
		"Severity":   "CRITICAL",
		"Source":     "alert-evaluator",
		"Rule":       "swap_high",
		"Incident":   "#42",
		"Device":     "#1",
		"Dedupe key": "pipeline:swap_high:device_id=1",
	}
	saw := map[string]string{}
	for _, f := range fields {
		m := f.(map[string]any)
		saw[m["title"].(string)] = m["value"].(string)
	}
	for k, v := range want {
		if saw[k] != v {
			t.Errorf("field %q = %q, want %q", k, saw[k], v)
		}
	}
}

// TestSlackSenderColorByUnknownSeverity guards the slate fallback so a
// rogue severity string doesn't break the rail's render contract (Slack
// silently drops a malformed color and the operator gets a bare card).
func TestSlackSenderColorByUnknownSeverity(t *testing.T) {
	if got := slackColor(Severity("bogus")); got == "" {
		t.Errorf("color empty for unknown severity")
	}
	cases := map[Severity]string{
		SeverityCritical: "#d92f2f",
		SeverityWarning:  "#f2c037",
		SeverityInfo:     "#36a64f",
	}
	for sev, want := range cases {
		if got := slackColor(sev); got != want {
			t.Errorf("slackColor(%q) = %q, want %q", sev, got, want)
		}
	}
}

func TestFormatSlackLocalizedLabelsAndRuleName(t *testing.T) {
	t.Cleanup(func() { SetDefaultLocale("en") })

	msg := Message{
		Subject:   "restore_drill_failed: codelinkops_restore_drill_ok == 0",
		Severity:  SeverityCritical,
		Source:    "host",
		DedupeKey: "pipeline:restore_drill_failed:device_id=3",
		Labels: map[string]string{
			"rule":            "restore_drill_failed",
			"rule_name":       "恢复演练失败",
			"incident_id":     "30",
			"device_id":       "3",
			"device_hostname": "ip-10-0-10-15",
		},
	}

	SetDefaultLocale("zh-CN")
	fields := slackFields(t, formatSlack(msg))

	// 字段名走本地化表
	for key, want := range map[string]string{
		"级别": "CRITICAL", "来源": "host", "事件": "#30",
	} {
		if got, ok := fields[key]; !ok || got != want {
			t.Errorf("字段 %q = %q (存在=%v)，期望 %q", key, got, ok, want)
		}
	}

	// 规则字段优先显示人类可读名，而不是 rule_key
	if got := fields["规则"]; got != "恢复演练失败" {
		t.Errorf("规则字段 = %q，期望人类可读名而不是 rule_key", got)
	}
	// 设备字段优先主机名 —— "#3" 说明不了哪台机器着火了
	if got := fields["设备"]; got != "ip-10-0-10-15" {
		t.Errorf("设备字段 = %q，期望主机名", got)
	}

	// 回退到英文时字段名恢复，内容不变
	SetDefaultLocale("en")
	en := slackFields(t, formatSlack(msg))
	if got := en["Severity"]; got != "CRITICAL" {
		t.Errorf("en Severity = %q", got)
	}
	if got := en["Rule"]; got != "恢复演练失败" {
		t.Errorf("en Rule = %q — 规则名是用户写的内容，不该随 locale 变", got)
	}
	if _, ok := en["级别"]; ok {
		t.Error("英文 locale 下不该出现中文字段名")
	}
}

// TestFormatSlackFallsBackToRuleKey 覆盖没起名字的规则：卡片仍然可读，
// 而不是留一个空的规则字段。
func TestFormatSlackFallsBackToRuleKey(t *testing.T) {
	t.Cleanup(func() { SetDefaultLocale("en") })
	SetDefaultLocale("zh-CN")
	fields := slackFields(t, formatSlack(Message{
		Severity: SeverityWarning,
		Labels:   map[string]string{"rule": "swap_high", "device_id": "7"},
	}))
	if got := fields["规则"]; got != "swap_high" {
		t.Errorf("规则字段 = %q，无名规则应回退到 rule_key", got)
	}
	if got := fields["设备"]; got != "#7" {
		t.Errorf("设备字段 = %q，无主机名时应回退到 #id", got)
	}
}

// TestUnknownLocaleFallsBackToEnglish：缺翻译时宁可显示英文，
// 也不要显示空字段名（那会让卡片看起来像坏了）。
func TestUnknownLocaleFallsBackToEnglish(t *testing.T) {
	t.Cleanup(func() { SetDefaultLocale("en") })
	SetDefaultLocale("de-DE")
	if got := label("dedupe_key"); got != "Dedupe key" {
		t.Errorf("未知 locale 下 label = %q，期望回退英文", got)
	}
}

// slackFields 把 formatSlack 的输出摊平成 title -> value，方便断言。
func slackFields(t *testing.T, payload map[string]any) map[string]string {
	t.Helper()
	atts, ok := payload["attachments"].([]any)
	if !ok || len(atts) == 0 {
		t.Fatalf("payload 里没有 attachments: %#v", payload)
	}
	att, ok := atts[0].(map[string]any)
	if !ok {
		t.Fatalf("attachment 类型不对: %#v", atts[0])
	}
	out := map[string]string{}
	raw, _ := att["fields"].([]map[string]any)
	for _, f := range raw {
		title, _ := f["title"].(string)
		value, _ := f["value"].(string)
		out[title] = value
	}
	return out
}
