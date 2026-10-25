// 默认调用池（active group）的端到端用例：不带分组参数时走 active group、
// 显式头覆盖它、active 组内无号报 no_account_in_group、active 为空时零回归。
// 构造方式与 group_route_test.go 一致（两分组各一个健康号）。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// TestActiveGroupDefaultPicksActiveGroup 未带分组参数 → 走 active group（team-a）。
func TestActiveGroupDefaultPicksActiveGroup(t *testing.T) {
	rec := &authRecorder{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		rec.add(authz)
		return 200, sseOK, true
	})
	p := groupRoutePool(t)
	p.SetActiveGroup("team-a")
	h := NewHandler(Config{Pool: p, Upstream: up})

	// 多跑几次：组过滤后只剩 ga，任何一次漂到 team-b 都是回归。
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatReq()))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("run=%d code=%d body=%s", i, w.Code, w.Body)
		}
		if got := w.Header().Get("X-Wb-Account"); got != "ga" {
			t.Fatalf("run=%d picked %q, want ga (active group)", i, got)
		}
	}
	for _, got := range rec.list() {
		if got != "Bearer at-ga" {
			t.Fatalf("upstream saw %q, want only Bearer at-ga (calls=%v)", got, rec.list())
		}
	}
}

// TestActiveGroupExplicitHeaderOverrides 显式头覆盖 active group（active=team-a，头=team-b）。
func TestActiveGroupExplicitHeaderOverrides(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := groupRoutePool(t)
	p.SetActiveGroup("team-a")
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions?group=team-a", strings.NewReader(chatReq()))
	req.Header.Set("X-WB2A-Group", "team-b")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
	if got := w.Header().Get("X-Wb-Account"); got != "gb" {
		t.Fatalf("picked %q, want gb (explicit header overrides active group)", got)
	}
}

// TestActiveGroupNoAccount503 active group 里没有可用账号 → 503 no_account_in_group，
// 且不打上游、不落回全池。
func TestActiveGroupNoAccount503(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "ga", AccessToken: "at-ga", ExpiresAt: 9999999999})
	p.SetMeta("ga", "team-a", "")
	p.SetActiveGroup("team-empty")
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatReq()))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
	var e map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("body not json: %v %s", err, w.Body)
	}
	errObj, _ := e["error"].(map[string]any)
	if errObj["code"] != "no_account_in_group" {
		t.Fatalf("code=%v want no_account_in_group (body=%s)", errObj["code"], w.Body)
	}
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "分组 team-empty 无可用账号") {
		t.Fatalf("message=%q want 点名 active 分组", msg)
	}
	if calls != 0 {
		t.Fatalf("hit upstream %d time(s), want 0", calls)
	}
}

// TestActiveGroupEmptyKeepsFullPool active 为空 → 旧行为：未指定分组时全池轮换。
func TestActiveGroupEmptyKeepsFullPool(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		mu.Lock()
		seen[authz]++
		first := len(seen) == 1
		mu.Unlock()
		if first {
			return 500, `{"code":500,"msg":"boom"}`, false // 首号失败 → 轮换到另一组
		}
		return 200, sseOK, true
	})
	p := groupRoutePool(t)
	p.SetActiveGroup("") // 显式回到全池（与从未设置等价）
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatReq()))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("empty active group must rotate across the whole pool, calls=%v", seen)
	}
}
