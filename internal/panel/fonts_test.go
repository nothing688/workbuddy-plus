package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEmbeddedFonts 内嵌字体要能直接拿到，且带长缓存与正确的 MIME。
//
// 这条链路容易在重构里坏掉：字体走独立路由（不是 /panel/api/*），CSP 也必须
// 单独放行 font-src，否则浏览器会静默拒绝加载，页面只是「退回系统字体」——
// 看起来没问题，实际所有人都退回了不同字形。
func TestEmbeddedFonts(t *testing.T) {
	pn := New(Config{Version: "test", APIKey: "test-key"})

	for _, c := range []struct {
		path string
		min  int
	}{
		{"/panel/fonts/wqy-sans.woff2", 500_000}, // GB2312 子集约 1MB
		{"/panel/fonts/wqy-mono.woff2", 4_000},   // ASCII 子集约 8.7KB
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, c.path, nil)
		pn.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s code=%d", c.path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "font/woff2" {
			t.Fatalf("%s Content-Type=%q", c.path, ct)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Fatalf("%s Cache-Control=%q（字体内容是进制固定的，应长缓存）", c.path, cc)
		}
		if n := rec.Body.Len(); n < c.min {
			t.Fatalf("%s 只回了 %d 字节，子集看起来被截断了", c.path, n)
		}
	}

	// 字体路由不带鉴权（登录页也要能加载）；同时 CSP 必须放行同源字体。
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/fonts/wqy-sans.woff2", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("无鉴权时应可读取字体，code=%d", rec.Code)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "font-src 'self'") {
		t.Fatalf("CSP 未放行同源字体: %s", csp)
	}
}
