package panel

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 面板文本响应必须 gzip：app.js 310KB、总览 JSON 在 200 号时也有几百 KB，
// 不压缩的话首屏和 5s 轮询都在传重复度极高的明文。
func TestPanelGzipCompressesText(t *testing.T) {
	p := newTestPanel()
	req := httptest.NewRequest("GET", "/panel/app.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding=%q want gzip", got)
	}
	if vary := rec.Header().Get("Vary"); !strings.Contains(vary, "Accept-Encoding") {
		t.Fatalf("Vary=%q want Accept-Encoding", vary)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if !bytes.Contains(raw, []byte("'use strict'")) {
		t.Fatal("解压后不是 app.js 原文")
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("Content-Type=%q", rec.Header().Get("Content-Type"))
	}

	// 没声明 gzip 的客户端（老工具、curl 默认）拿明文，不受影响。
	plain := httptest.NewRecorder()
	p.ServeHTTP(plain, httptest.NewRequest("GET", "/panel/app.js", nil))
	if enc := plain.Header().Get("Content-Encoding"); enc != "" {
		t.Fatalf("未请求 gzip 却压缩了: %q", enc)
	}
	if !strings.Contains(plain.Body.String(), "'use strict'") {
		t.Fatal("未压缩响应正文异常")
	}
}

// q=0 是显式拒绝（RFC 9110），不能压。
func TestPanelGzipHonorsQZero(t *testing.T) {
	p := newTestPanel()
	req := httptest.NewRequest("GET", "/panel/app.js", nil)
	req.Header.Set("Accept-Encoding", "gzip;q=0, identity")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Fatalf("gzip;q=0 仍压缩: %q", enc)
	}
}

// 字体已是 woff2 压缩格式，再套 gzip 只会变大，必须跳过。
func TestPanelGzipSkipsPrecompressedAssets(t *testing.T) {
	p := newTestPanel()
	req := httptest.NewRequest("GET", "/panel/fonts/wqy-mono.woff2", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Fatalf("woff2 被压缩: %q", enc)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "font/woff2" {
		t.Fatalf("Content-Type=%q", ct)
	}
}

// 带正确内容指纹的脚本 URL 应该被长期缓存；不带指纹仍是 no-store（防旧脚本，issue #33）。
func TestPanelScriptCacheKeyedByFingerprint(t *testing.T) {
	p := newTestPanel()
	cases := []struct {
		path string
		fp   string
	}{
		{"/panel/app.js", appFingerprint},
		{"/panel/theme.js", themeFingerprint},
	}
	for _, c := range cases {
		hit := httptest.NewRecorder()
		p.ServeHTTP(hit, httptest.NewRequest("GET", c.path+"?v="+c.fp, nil))
		if cc := hit.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Errorf("%s?v=<fp> Cache-Control=%q，want immutable", c.path, cc)
		}
		if et := hit.Header().Get("ETag"); et != `"`+c.fp+`"` {
			t.Errorf("%s ETag=%q", c.path, et)
		}

		miss := httptest.NewRecorder()
		p.ServeHTTP(miss, httptest.NewRequest("GET", c.path, nil))
		if cc := miss.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s 无指纹 Cache-Control=%q，want no-store", c.path, cc)
		}

		stale := httptest.NewRecorder()
		p.ServeHTTP(stale, httptest.NewRequest("GET", c.path+"?v=deadbeefdeadbeef", nil))
		if cc := stale.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s 旧指纹 Cache-Control=%q，want no-store", c.path, cc)
		}
	}
}

// JSON 接口同样压缩：401 也走 writeJSON，顺带覆盖鉴权失败路径。
func TestPanelGzipCompressesJSON(t *testing.T) {
	p := newTestPanel()
	req := httptest.NewRequest("GET", "/panel/api/overview", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", rec.Code)
	}
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("401 JSON 未压缩: %q", rec.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	raw, _ := io.ReadAll(zr)
	if !strings.Contains(string(raw), "invalid_api_key") {
		t.Fatalf("解压后正文=%s", raw)
	}
}
