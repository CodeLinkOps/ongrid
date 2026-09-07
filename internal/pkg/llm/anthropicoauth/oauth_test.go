package anthropicoauth

import (
	"context"
	"encoding/json"
	"io"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type memStore struct {
	mu    sync.Mutex
	cred  Credential
	saves int
	err   error
}

func (m *memStore) Load(context.Context) (Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cred, nil
}

func (m *memStore) Save(_ context.Context, c Credential) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.cred = c
	m.saves++
	return nil
}

// tokenServer stands in for Anthropic's token endpoint, handing out a
// NEW refresh token every time — which is what the real one does.
func tokenServer(t *testing.T, issued *int32, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	n := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		seq := n
		mu.Unlock()
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["grant_type"] == "refresh_token" && body["refresh_token"] == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "invalid_grant", "error_description": "Refresh token not found or invalid"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-" + itoa(seq),
			"refresh_token": "refresh-" + itoa(seq),
			"expires_in":    3600,
		})
	}))
}

func itoa(i int) string { return strings.TrimSpace(strings.Repeat("", 0) + string(rune('0'+i))) }

// TestTransportPersistsRotatedRefreshToken 是这个包最重要的测试。
//
// 刷新会**轮换** refresh token。存不回去的话，登录会从「长期有效」变成
// 「只有一小时」：旧的 refresh token 在新的签发那一刻就死了，下次刷新
// 报 invalid_grant，而日志里没有任何东西指向「那次没写盘」。
func TestTransportPersistsRotatedRefreshToken(t *testing.T) {
	var mu sync.Mutex
	var issued int32
	srv := tokenServer(t, &issued, &mu)
	defer srv.Close()

	store := &memStore{cred: Credential{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Minute), // 已过期
	}}
	tr := &Transport{
		Store:      store,
		HTTPClient: srv.Client(),
		Base:       roundTripFunc(func(r *http.Request) (*http.Response, error) { return okResp(r), nil }),
	}
	withTokenURL(t, srv.URL)

	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/chat/completions", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()

	if store.saves != 1 {
		t.Fatalf("Save 调用了 %d 次，期望 1 —— 轮换后的 refresh token 必须存回去", store.saves)
	}
	if store.cred.RefreshToken == "old-refresh" {
		t.Fatal("存回去的还是旧 refresh token —— 下次刷新会 invalid_grant")
	}
	if got := resp.Request.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer access-") {
		t.Fatalf("Authorization = %q，期望注入刷新后的 access token", got)
	}
}

// TestTransportRefusesWhenSaveFails：存不进去就必须失败，不能「这次能用、
// 一小时后永久锁死」。
func TestTransportRefusesWhenSaveFails(t *testing.T) {
	var mu sync.Mutex
	var issued int32
	srv := tokenServer(t, &issued, &mu)
	defer srv.Close()
	withTokenURL(t, srv.URL)

	store := &memStore{
		cred: Credential{AccessToken: "old", RefreshToken: "r", ExpiresAt: time.Now().Add(-time.Minute)},
		err:  errors.New("disk full"),
	}
	tr := &Transport{Store: store, HTTPClient: srv.Client()}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/chat/completions", nil)
	if _, err := tr.RoundTrip(req); err == nil {
		t.Fatal("Save 失败时 RoundTrip 必须报错 —— 否则这次成功、下次永久锁死")
	}
}

// TestTransportNotLoggedInSaysSo：没有 refresh token 时不要反复重试，
// 直接告诉运维要重新登录。
func TestTransportNotLoggedInSaysSo(t *testing.T) {
	tr := &Transport{Store: &memStore{}}
	req, _ := http.NewRequest(http.MethodPost, "https://x/", nil)
	_, err := tr.RoundTrip(req)
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("err = %v，期望明确说「没登录」", err)
	}
}

// TestTransportConcurrentRefreshesOnce：一批并发调查各自刷新的话，
// 每次轮换都会让前一个失效 —— 只能刷新一次。
func TestTransportConcurrentRefreshesOnce(t *testing.T) {
	var mu sync.Mutex
	var issued int32
	srv := tokenServer(t, &issued, &mu)
	defer srv.Close()
	withTokenURL(t, srv.URL)

	store := &memStore{cred: Credential{
		AccessToken: "old", RefreshToken: "r", ExpiresAt: time.Now().Add(-time.Minute)}}
	tr := &Transport{
		Store:      store,
		HTTPClient: srv.Client(),
		Base:       roundTripFunc(func(r *http.Request) (*http.Response, error) { return okResp(r), nil }),
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/x", nil)
			if resp, err := tr.RoundTrip(req); err == nil {
				_ = resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	if store.saves != 1 {
		t.Fatalf("刷新了 %d 次，期望 1 —— 每次轮换都会让上一次失效", store.saves)
	}
}

func TestStartLoginProducesPKCEChallenge(t *testing.T) {
	ls, err := StartLogin()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"code_challenge=", "code_challenge_method=S256", "client_id=" + ClientID} {
		if !strings.Contains(ls.URL, want) {
			t.Errorf("授权 URL 缺少 %q", want)
		}
	}
	if len(ls.Verifier) < 32 {
		t.Errorf("verifier 太短：%d", len(ls.Verifier))
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func okResp(r *http.Request) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Body:       http.NoBody,
		Header:     make(http.Header),
		Request:    r,
	}
}

// withTokenURL 把 token 端点指向测试服务器，并在测试结束时还原。
func withTokenURL(t *testing.T, u string) {
	t.Helper()
	old := tokenURL
	tokenURL = u
	t.Cleanup(func() { tokenURL = old })
}

// TestErrorTextHandlesBothShapes 覆盖线上探到的一个不一致：
// token 端点在不同失败下用两种 error 形状。
//
// refresh token 不对时是字符串：      {"error":"invalid_grant", ...}
// authorization code 不对时是对象：   {"error":{"type":"...","message":"..."}}
//
// 把它写成 string 的话，对象那种会在**解码阶段**就失败，运维看到的是
//
//   decode token response (status 403): json: cannot unmarshal object
//   into Go struct field tokenResp.error of type string
//
// —— 这句话对「code 贴错了」这个真实原因只字未提。
func TestErrorTextHandlesBothShapes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "字符串形状（刷新失败）",
			raw:  `{"error":"invalid_grant","error_description":"Refresh token not found or invalid"}`,
			want: "invalid_grant: Refresh token not found or invalid",
		},
		{
			name: "对象形状（code 无效）",
			raw:  `{"error":{"type":"invalid_request_error","message":"Invalid authorization code"}}`,
			want: "invalid_request_error: Invalid authorization code",
		},
		{
			name: "没有 error 字段",
			raw:  `{"access_token":"x","expires_in":3600}`,
			want: "",
		},
		{
			name: "error 为 null",
			raw:  `{"error":null,"access_token":"x"}`,
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var tr tokenResp
			if err := json.Unmarshal([]byte(c.raw), &tr); err != nil {
				t.Fatalf("解码失败（这本身就是那个 bug）: %v", err)
			}
			if got := tr.errorText(); got != c.want {
				t.Errorf("errorText() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestPostSurfacesObjectShapedError 端到端确认：对象形状的错误要变成一句
// 人能看懂的话，而不是解码失败。
func TestPostSurfacesObjectShapedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"Invalid authorization code"}}`))
	}))
	defer srv.Close()
	withTokenURL(t, srv.URL)

	_, err := Exchange(context.Background(), srv.Client(), "bad-code", "verifier")
	if err == nil {
		t.Fatal("期望报错")
	}
	if !strings.Contains(err.Error(), "Invalid authorization code") {
		t.Fatalf("错误信息没带上真实原因: %v", err)
	}
	if strings.Contains(err.Error(), "cannot unmarshal") {
		t.Fatalf("仍然是解码失败 —— 那句话对真实原因只字未提: %v", err)
	}
}

// TestTransportRetriesOnceOn401 覆盖「token 看起来没过期，但服务端拒绝」。
//
// 只按过期时间刷新是不够的：另一个客户端登录会把 token 轮换掉，而本地那份
// 的 expires_at 还没到。没有这条重试的话，AI 会一直坏到本地过期为止 ——
// 可能是大半个小时，而唯一的症状是一个看起来像配置错误的 401。
func TestTransportRetriesOnceOn401(t *testing.T) {
	var mu sync.Mutex
	var issued int32
	srv := tokenServer(t, &issued, &mu)
	defer srv.Close()
	withTokenURL(t, srv.URL)

	store := &memStore{cred: Credential{
		AccessToken:  "stale-but-unexpired",
		RefreshToken: "r",
		ExpiresAt:    time.Now().Add(time.Hour), // 本地看还很新鲜
	}}

	var calls int
	var seen []string
	tr := &Transport{
		Store:      store,
		HTTPClient: srv.Client(),
		Base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			seen = append(seen, r.Header.Get("Authorization"))
			if calls == 1 {
				return &http.Response{
					StatusCode: http.StatusUnauthorized,
					Body:       io.NopCloser(strings.NewReader(`{"error":"unauthorized"}`)),
					Header:     make(http.Header),
					Request:    r,
				}, nil
			}
			return okResp(r), nil
		}),
	}

	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/chat/completions", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()

	if calls != 2 {
		t.Fatalf("底层请求了 %d 次，期望 2（原始 + 重试一次）", calls)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("重试后 status = %d", resp.StatusCode)
	}
	if seen[0] == seen[1] {
		t.Fatal("重试用的还是同一个 token —— 401 之后必须强制刷新")
	}
	if store.saves != 1 {
		t.Fatalf("Save 调用了 %d 次，期望 1", store.saves)
	}
}

// TestTransportDoesNotLoopOn401：刷新之后仍然 401 时不能无限重试。
func TestTransportDoesNotLoopOn401(t *testing.T) {
	var mu sync.Mutex
	var issued int32
	srv := tokenServer(t, &issued, &mu)
	defer srv.Close()
	withTokenURL(t, srv.URL)

	store := &memStore{cred: Credential{
		AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour)}}
	var calls int
	tr := &Transport{
		Store:      store,
		HTTPClient: srv.Client(),
		Base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Body:       io.NopCloser(strings.NewReader("{}")),
				Header:     make(http.Header),
				Request:    r,
			}, nil
		}),
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/x", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()
	if calls != 2 {
		t.Fatalf("请求了 %d 次，期望恰好 2 —— 不能无限重试", calls)
	}
}
