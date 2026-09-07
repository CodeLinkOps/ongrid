package slack

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestThreadParentUsesFormEncoding 锁住 conversations.replies 的编码方式。
//
// **不是每个 Slack Web API 方法都接受 JSON。** chat.* 系列接受，
// conversations.replies 不接受 —— 它回 `invalid_arguments`，而那句话完全
//不提编码，读起来像 channel 或 ts 写错了。
//
// 代价是一个「看起来已部署」的坏功能：线程上下文每次都取不到，而失败只体现
// 为一行没人看的 warning。2026-09-07 实测发现。
func TestThreadParentUsesFormEncoding(t *testing.T) {
	var gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"messages":[{"text":"🔴 严重 · 恢复演练失败\n事件 #47","ts":"1.1"}]}`))
	}))
	defer srv.Close()

	c := NewClient("xapp-x", "xoxb-x")
	c.SetBaseURL(srv.URL)

	got, err := c.ThreadParent(context.Background(), "C123", "1.1")
	if err != nil {
		t.Fatalf("ThreadParent: %v", err)
	}
	if !strings.Contains(gotCT, "x-www-form-urlencoded") {
		t.Errorf("Content-Type = %q —— conversations.replies 只接受 form 编码，"+
			"用 JSON 会得到 invalid_arguments，而那句话不提编码", gotCT)
	}
	if !strings.Contains(gotBody, "channel=C123") || !strings.Contains(gotBody, "ts=1.1") {
		t.Errorf("请求体 = %q，期望 form 参数", gotBody)
	}
	if !strings.Contains(got, "事件 #47") {
		t.Errorf("没取到根消息内容: %q", got)
	}
}
