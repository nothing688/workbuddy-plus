// 分组级熔断的端到端用例：组内全冷却 → 改道组外 + X-Wb-Group-Failover；
// 组内仍有健康号 / 换模型 / 未指定分组时保持原语义（零回归）。
package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/session"
)

// coolGroup 让 uid 对 model 进入 6004 模型级冷却（不污染账号级 until）。
func coolGroup(t *testing.T, p *pool.Pool, uid, model string) {
	t.Helper()
	p.CooldownSoftForModel(uid, 600*time.Second, time.Now().Add(30*time.Minute), model, "6004 model rate limit")
}

// TestGroupBreakerFailsOverToOtherGroup team-a 的唯一号对该模型冷却 → 请求改道
// team-b，并在响应头标记熔断来源。
func TestGroupBreakerFailsOverToOtherGroup(t *testing.T) {
	p := groupRoutePool(t)
	coolGroup(t, p, "ga", "glm-5.2")
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatReq()))
	req.Header.Set("X-WB2A-Group", "team-a")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
	if got := w.Header().Get("X-Wb-Account"); got != "gb" {
		t.Fatalf("应改道 team-b 的 gb，实际 %q", got)
	}
	if got := w.Header().Get("X-Wb-Group-Failover"); got != "team-a" {
		t.Fatalf("X-Wb-Group-Failover=%q，want team-a", got)
	}
	rows := p.GroupBreakers()
	if len(rows) != 1 || rows[0].Group != "team-a" || rows[0].Model != "glm-5.2" {
		t.Fatalf("熔断台账=%+v，want team-a/glm-5.2 一行", rows)
	}
}

// TestGroupBreakerNotTriggeredWhenGroupHealthy 组内还有健康号 → 仍选本组，不出头。
func TestGroupBreakerNotTriggeredWhenGroupHealthy(t *testing.T) {
	p := groupRoutePool(t)
	// 冷却 team-b 的号不影响 team-a 的判定。
	coolGroup(t, p, "gb", "glm-5.2")
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatReq()))
	req.Header.Set("X-WB2A-Group", "team-a")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
	if got := w.Header().Get("X-Wb-Account"); got != "ga" {
		t.Fatalf("应选 team-a 的 ga，实际 %q", got)
	}
	if got := w.Header().Get("X-Wb-Group-Failover"); got != "" {
		t.Fatalf("未熔断不应带头，实际 %q", got)
	}
}

// TestGroupBreakerModelScoped 熔断只作用于触发它的模型：同一分组换模型仍正常选本组。
func TestGroupBreakerModelScoped(t *testing.T) {
	p := groupRoutePool(t)
	coolGroup(t, p, "ga", "glm-5.2")
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: p, Upstream: up})

	body := `{"model":"hy3","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("X-WB2A-Group", "team-a")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
	if got := w.Header().Get("X-Wb-Account"); got != "ga" {
		t.Fatalf("hy3 应仍选 team-a 的 ga，实际 %q", got)
	}
	if got := w.Header().Get("X-Wb-Group-Failover"); got != "" {
		t.Fatalf("换模型不应带改道头，实际 %q", got)
	}
}

// TestGroupBreakerStickyFollowsFailover 熔断改道后，会话粘性应跟到承接组：
// 组外粘性号在该组对该模型熔断期间继续可用，不被解绑重选。
func TestGroupBreakerStickyFollowsFailover(t *testing.T) {
	p := groupRoutePool(t)
	coolGroup(t, p, "ga", "glm-5.2")
	store := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     store,
		Available: func() []string { return []string{"ga", "gb"} },
	})
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	sess.Bind("conv-breaker", "gb")

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-breaker"}}`))
	req.Header.Set("X-WB2A-Group", "team-a")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
	if got := w.Header().Get("X-Wb-Account"); got != "gb" {
		t.Fatalf("粘性应跟到承接组 gb，实际 %q", got)
	}
	if got := w.Header().Get("X-Wb-Group-Failover"); got != "team-a" {
		t.Fatalf("X-Wb-Group-Failover=%q，want team-a", got)
	}
	if uid, ok := store.lastUID("conv-breaker"); !ok || uid != "gb" {
		t.Fatalf("binding=%s ok=%v，want gb（继续粘住承接号）", uid, ok)
	}
}

// TestGroupBreakerInactiveWithoutGroup 未指定分组时全池选号，不产生改道头。
func TestGroupBreakerInactiveWithoutGroup(t *testing.T) {
	p := groupRoutePool(t)
	coolGroup(t, p, "ga", "glm-5.2")
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatReq()))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
	if got := w.Header().Get("X-Wb-Account"); got != "gb" {
		t.Fatalf("全池应落到健康号 gb，实际 %q", got)
	}
	if got := w.Header().Get("X-Wb-Group-Failover"); got != "" {
		t.Fatalf("全池选号不应带改道头，实际 %q", got)
	}
}
