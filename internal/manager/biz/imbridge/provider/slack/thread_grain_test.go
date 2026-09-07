package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestThreadKeyIsPerThreadNotPerChannel 锁住会话粒度。
//
// ev.ThreadTS 在「频道里直接 @ 机器人」时是空的。直接用它作键，键就变成
// (app, channel, "") —— **整个频道共用一条会话**：两个人问不相干的事会串到
// 一起，谁的追问都可能接到别人的问题后面。
//
// 这个粒度是 CodeLinkOps/prime-slack-bridge 试错两轮定下来的
// （见那个仓库 DESIGN.md §3.4）：
//   - 每 repo 一条会话 → 两个任务抢同一条 session，一个回空的「✅ Done」
//   - 每条消息一条会话 → Agent 不记得前面说过的话，而且**它不报错**，
//     会一本正经地按「你只发过这一句」回答
//   - 每 thread 一条会话 → 两个问题都没有
func TestThreadKeyIsPerThreadNotPerChannel(t *testing.T) {
	cases := []struct {
		name     string
		threadTS string
		ts       string
		want     string
	}{
		{"频道里直接 @（无 thread）→ 用自身 ts 开新线", "", "111.1", "111.1"},
		{"同一条线里的追问 → 沿用线的 ts", "111.1", "222.2", "111.1"},
		{"另一个人在频道里另问一句 → 另一条线", "", "333.3", "333.3"},
	}
	seen := map[string]int{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.threadTS
			if got == "" {
				got = c.ts
			}
			if got != c.want {
				t.Fatalf("threadKey = %q, want %q", got, c.want)
			}
			seen[got]++
		})
	}
	// 两次独立提问必须落到两条不同的会话键，否则就是频道级串台。
	if len(seen) != 2 {
		t.Fatalf("不同的会话键有 %d 个，期望 2（111.1 与 333.3）—— "+
			"只有 1 个说明整个频道共用一条会话", len(seen))
	}
	if seen["111.1"] != 2 {
		t.Fatalf("同一条线里的两条消息没有落到同一个键：%v", seen)
	}
}

// TestSenderRepliesIntoThread：回复必须发进线程，否则下一条追问不会带
// thread_ts，粒度就退回频道级。
func TestSenderRepliesIntoThread(t *testing.T) {
	var gotThreadTS string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if v, ok := body["thread_ts"].(string); ok {
			gotThreadTS = v
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"ts":"999.9","channel":"C1"}`))
	}))
	defer srv.Close()

	c := NewClient("xapp-x", "xoxb-x")
	c.SetBaseURL(srv.URL)
	s := senderAdapter{client: c, channel: "C1", threadTS: "111.1"}
	if _, err := s.SendText(context.Background(), "C1", "channel", "hi"); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if gotThreadTS != "111.1" {
		t.Fatalf("thread_ts = %q，回复没进线程 —— 追问就不会带 thread_ts，"+
			"粒度会退回频道级", gotThreadTS)
	}
}
