package imbridge

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/ongridio/ongrid/internal/manager/biz/aiops/agent"
	model "github.com/ongridio/ongrid/internal/manager/model/imbridge"
	"github.com/ongridio/ongrid/internal/pkg/errs"
)

type seedRepo struct {
	mu      sync.Mutex
	app     *model.ImApp
	thread  *model.ImThread
	created int
}

func (r *seedRepo) GetAppByAppID(context.Context, string, string) (*model.ImApp, error) {
	return r.app, nil
}

func (r *seedRepo) FindThread(context.Context, uint64, string, string) (*model.ImThread, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.thread == nil {
		return nil, errs.ErrNotFound
	}
	return r.thread, nil
}

func (r *seedRepo) CreateThread(_ context.Context, t *model.ImThread) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	t.ID = 1
	r.thread = t
	r.created++
	return nil
}

func (r *seedRepo) TouchThread(context.Context, uint64) error { return nil }

func (r *seedRepo) RotateThreadSession(context.Context, uint64, string) error { return nil }

type seedAgent struct {
	mu   sync.Mutex
	sent []string
}

func (a *seedAgent) EnsureSession(context.Context, uint64, string) (string, error) {
	return "sess-1", nil
}

func (a *seedAgent) StreamMessage(_ context.Context, _ string, content string, emit agent.Emit) error {
	a.mu.Lock()
	a.sent = append(a.sent, content)
	a.mu.Unlock()
	emit(agent.Event{Type: agent.EventDone})
	return nil
}

type seedSender struct{}

func (seedSender) SendText(context.Context, string, string, string) (string, error) {
	return "m1", nil
}

// TestThreadParentSeedsOnlyTheFirstMessage 覆盖生产上真实发生过的一次失败，
// 以及修它时最容易做错的地方。
//
// 现象：运维在告警卡片**下面的线程里**问「这是啥情况」。线程会映射到自己的
// 一个空会话，于是机器人在回答一条它看不见的告警 —— 它只能反问「你指的是
// 哪种对象」。
//
// 修法是把线程根消息作为开场上下文喂一次。**但只能喂一次** —— 每条都带的话，
// 会话里已经有上下文了还重复灌，既烧 token 又会把真正的问题淹掉。
func TestThreadParentSeedsOnlyTheFirstMessage(t *testing.T) {
	repo := &seedRepo{app: &model.ImApp{ID: 7, AppID: "T1", Provider: model.ProviderSlack, Enabled: true}}
	ag := &seedAgent{}
	b := NewBridge(repo, ag, 1, nil)

	base := InboundMessage{
		Provider: model.ProviderSlack, AppID: "T1", ChatID: "C1",
		ThreadID: "111.1", OpenID: "U1", ReceiveIDType: "channel",
	}

	first := base
	first.EventID = "e1"
	first.Text = "这是啥情况"
	first.ThreadParentText = "🔴 严重 · 恢复演练失败\n事件 #47"
	if err := b.HandleInbound(context.Background(), seedSender{}, first); err != nil {
		t.Fatalf("首条: %v", err)
	}

	second := base
	second.EventID = "e2"
	second.Text = "那要怎么修"
	second.ThreadParentText = "🔴 严重 · 恢复演练失败\n事件 #47"
	if err := b.HandleInbound(context.Background(), seedSender{}, second); err != nil {
		t.Fatalf("次条: %v", err)
	}

	if len(ag.sent) != 2 {
		t.Fatalf("agent 收到 %d 条，期望 2", len(ag.sent))
	}
	if !strings.Contains(ag.sent[0], "事件 #47") {
		t.Errorf("首条没有带上线程根消息 —— 机器人会在回答一条它看不见的告警：\n%s", ag.sent[0])
	}
	if strings.Contains(ag.sent[1], "事件 #47") {
		t.Errorf("第二条又灌了一遍上下文 —— 会话里已经有了，重复只会烧 token 并淹掉真正的问题：\n%s", ag.sent[1])
	}
	if repo.created != 1 {
		t.Errorf("建了 %d 个线程，期望 1", repo.created)
	}
}

// TestNoThreadParentIsHarmless：provider 取不到根消息（比如缺 history 权限）
// 时必须照常工作，只是少了上下文。
func TestNoThreadParentIsHarmless(t *testing.T) {
	repo := &seedRepo{app: &model.ImApp{ID: 7, AppID: "T1", Provider: model.ProviderSlack, Enabled: true}}
	ag := &seedAgent{}
	b := NewBridge(repo, ag, 1, nil)

	msg := InboundMessage{
		Provider: model.ProviderSlack, AppID: "T1", ChatID: "C1", ThreadID: "111.1",
		OpenID: "U1", ReceiveIDType: "channel", EventID: "e1", Text: "这是啥情况",
	}
	if err := b.HandleInbound(context.Background(), seedSender{}, msg); err != nil {
		t.Fatalf("取不到根消息时应照常工作: %v", err)
	}
	if len(ag.sent) != 1 || !strings.Contains(ag.sent[0], "这是啥情况") {
		t.Fatalf("用户的问题没送到: %#v", ag.sent)
	}
}
