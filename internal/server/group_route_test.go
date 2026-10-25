// 按分组选号（X-WB2A-Group 头 / ?group= query）的端到端用例：三种入站协议共用
// chatCompletions 内核，这里既覆盖选号结果（上游收到的 Authorization / 响应头
// X-Wb-Account），也覆盖错误语义与零回归（未指定分组时全池轮换）。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/session"
)

// groupRoutePool 两个分组各一个健康账号：team-a → ga / team-b → gb。
// 分组只是 pool 台账（SetMeta），与选号算法本身解耦。
func groupRoutePool(t *testing.T) *pool.Pool {
	t.Helper()
	p := testPoolWith(
		&auth.Auth{UID: "ga", AccessToken: "at-ga", ExpiresAt: 9999999999},
		&auth.Auth{UID: "gb", AccessToken: "at-gb", ExpiresAt: 9999999999},
	)
	if !p.SetMeta("ga", "team-a", "") || !p.SetMeta("gb", "team-b", "") {
		t.Fatal("SetMeta failed")
	}
	return p
}

// authRecorder 记录上游收到的 Authorization（fake 客户端与断言并发安全）。
type authRecorder struct {
	mu    sync.Mutex
	auths []string
}

func (a *authRecorder) add(s string) {
	a.mu.Lock()
	a.auths = append(a.auths, s)
	a.mu.Unlock()
}

func (a *authRecorder) list() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.auths...)
}

// chatReq 构造一条最简 chat body（非流式，fake 回 sseOK 可聚合）。
func chatReq() string {
	return `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
}

// TestGroupRouteHeaderPicksOnlyGroup 头指定组 → 只选该组账号（两组各一号，
// 用上游收到的 Authorization 与响应头 X-Wb-Account 双重断言）。
func TestGroupRouteHeaderPicksOnlyGroup(t *testing.T) {
	for _, tc := range []struct {
		group  string
		wantAu string
		wantID string
	}{
		{"team-a", "Bearer at-ga", "ga"},
		{"team-b", "Bearer at-gb", "gb"},
	} {
		rec := &authRecorder{}
		up := newFakeUpstream(t, func(authz string) (int, string, bool) {
			rec.add(authz)
			return 200, sseOK, true
		})
		h := NewHandler(Config{Pool: groupRoutePool(t), Upstream: up})

		// 多跑几次：组过滤后只剩一个候选，任何一次漂到另一组都是回归。
		for i := 0; i < 5; i++ {
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatReq()))
			req.Header.Set("X-WB2A-Group", tc.group)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != 200 {
				t.Fatalf("group=%s run=%d code=%d body=%s", tc.group, i, w.Code, w.Body)
			}
			if got := w.Header().Get("X-Wb-Account"); got != tc.wantID {
				t.Fatalf("group=%s run=%d picked %q, want %q", tc.group, i, got, tc.wantID)
			}
		}
		for _, got := range rec.list() {
			if got != tc.wantAu {
				t.Fatalf("group=%s upstream saw %q, want only %q (calls=%v)", tc.group, got, tc.wantAu, rec.list())
			}
		}
	}
}

// TestGroupRouteQueryAndHeaderPrecedence 覆盖 query 生效、头与 query 同时给出时
// 头优先，以及两端空白被裁掉（头 " team-a " / query "%20team-b%20"）。
func TestGroupRouteQueryAndHeaderPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
		header string
		want   string
	}{
		{"query", "/v1/chat/completions?group=team-b", "", "gb"},
		{"header-wins", "/v1/chat/completions?group=team-b", "team-a", "ga"},
		{"header-trimmed", "/v1/chat/completions", "  team-a  ", "ga"},
		{"query-trimmed", "/v1/chat/completions?group=%20team-b%20", "", "gb"},
	} {
		up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
		h := NewHandler(Config{Pool: groupRoutePool(t), Upstream: up})
		req := httptest.NewRequest("POST", tc.target, strings.NewReader(chatReq()))
		if tc.header != "" {
			req.Header.Set("X-WB2A-Group", tc.header)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("%s: code=%d body=%s", tc.name, w.Code, w.Body)
		}
		if got := w.Header().Get("X-Wb-Account"); got != tc.want {
			t.Fatalf("%s: picked %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestGroupRouteNoAccountInGroup503 指定分组但组内无可用账号 → 503 +
// no_account_in_group（点名分组），且不打上游、不落回全池 no_healthy_account。
func TestGroupRouteNoAccountInGroup503(t *testing.T) {
	for _, group := range []string{"team-empty", "team-b-not-in-pool"} {
		calls := 0
		up := newFakeUpstream(t, func(string) (int, string, bool) {
			calls++
			return 200, sseOK, true
		})
		p := testPoolWith(&auth.Auth{UID: "ga", AccessToken: "at-ga", ExpiresAt: 9999999999})
		p.SetMeta("ga", "team-a", "")
		h := NewHandler(Config{Pool: p, Upstream: up})

		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatReq()))
		req.Header.Set("X-WB2A-Group", group)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("group=%s code=%d body=%s", group, w.Code, w.Body)
		}
		var e map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
			t.Fatalf("group=%s body not json: %v %s", group, err, w.Body)
		}
		errObj, _ := e["error"].(map[string]any)
		if errObj["code"] != "no_account_in_group" {
			t.Fatalf("group=%s code=%v want no_account_in_group (body=%s)", group, errObj["code"], w.Body)
		}
		if msg, _ := errObj["message"].(string); !strings.Contains(msg, "分组 "+group+" 无可用账号") {
			t.Fatalf("group=%s message=%q want 点名分组", group, msg)
		}
		if strings.Contains(w.Body.String(), "no_healthy_account") {
			t.Fatalf("group=%s must not fall back to no_healthy_account: %s", group, w.Body)
		}
		if calls != 0 {
			t.Fatalf("group=%s hit upstream %d time(s), want 0", group, calls)
		}
	}
}

// TestGroupRouteUnspecifiedKeepsFullPool 零回归：未指定分组时全池轮换——
// 首号失败后可见另一分组的账号接单，最终 200。
func TestGroupRouteUnspecifiedKeepsFullPool(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		mu.Lock()
		seen[authz]++
		first := len(seen) == 1
		mu.Unlock()
		if first {
			return 500, `{"code":500,"msg":"boom"}`, false // 首号失败 → 轮换到另一个组
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: groupRoutePool(t), Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatReq()))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("unspecified group must rotate across the whole pool, calls=%v", seen)
	}
}

// TestGroupRouteStickyOutsideGroupReselected 粘性号不在请求分组里 → 忽略粘性、
// 在组内重选，并把会话重绑到新号；粘性号本就在组内时不动（仍用它）。
func TestGroupRouteStickyOutsideGroupReselected(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bind     string
		group    string
		wantID   string
		wantAuth string
	}{
		{"outside", "ga", "team-b", "gb", "Bearer at-gb"},
		{"inside", "gb", "team-b", "gb", "Bearer at-gb"},
	} {
		store := newBindStore()
		sess := session.New(session.Config{
			TTL:       time.Minute,
			Store:     store,
			Available: func() []string { return []string{"ga", "gb"} },
		})
		rec := &authRecorder{}
		up := newFakeUpstream(t, func(authz string) (int, string, bool) {
			rec.add(authz)
			return 200, sseOK, true
		})
		h := NewHandler(Config{
			Pool:         groupRoutePool(t),
			Upstream:     up,
			Session:      sess,
			SoftCooldown: time.Minute,
		})
		sess.Bind("conv-group", tc.bind)

		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-group"}}`))
		req.Header.Set("X-WB2A-Group", tc.group)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != 200 {
			t.Fatalf("%s: code=%d body=%s", tc.name, w.Code, w.Body)
		}
		if got := w.Header().Get("X-Wb-Account"); got != tc.wantID {
			t.Fatalf("%s: picked %q, want %q (calls=%v)", tc.name, got, tc.wantID, rec.list())
		}
		for _, got := range rec.list() {
			if got != tc.wantAuth {
				t.Fatalf("%s: upstream saw %q, want only %q", tc.name, got, tc.wantAuth)
			}
		}
		if uid, ok := store.lastUID("conv-group"); !ok || uid != tc.wantID {
			t.Fatalf("%s: binding=%s ok=%v, want %s", tc.name, uid, ok, tc.wantID)
		}
	}
}

// TestGroupRouteAllInboundProtocols 三种入站协议（/v1/chat/completions、
// /v1/responses、/v1/messages）共用 chatCompletions 内核，同一请求头都必须生效。
func TestGroupRouteAllInboundProtocols(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		body string
	}{
		{"chat", "/v1/chat/completions", chatReq()},
		{"responses", "/v1/responses", `{"model":"glm-5.2","input":"hi","stream":false}`},
		{"messages", "/v1/messages", `{"model":"glm-5.2","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`},
	} {
		rec := &authRecorder{}
		up := newFakeUpstream(t, func(authz string) (int, string, bool) {
			rec.add(authz)
			return 200, sseOK, true
		})
		h := NewHandler(Config{Pool: groupRoutePool(t), Upstream: up})
		req := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
		req.Header.Set("X-WB2A-Group", "team-b")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != 200 {
			t.Fatalf("%s: code=%d body=%s", tc.name, w.Code, w.Body)
		}
		if got := w.Header().Get("X-Wb-Account"); got != "gb" {
			t.Fatalf("%s: picked %q, want gb (calls=%v)", tc.name, got, rec.list())
		}
		for _, got := range rec.list() {
			if got != "Bearer at-gb" {
				t.Fatalf("%s: upstream saw %q, want only team-b account", tc.name, got)
			}
		}
	}
}
