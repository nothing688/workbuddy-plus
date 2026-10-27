package panel

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// buildReleaseZip 造一个和 CI 同构的发布包（只放 wb2api，别的文件本测试不关心）。
func buildReleaseZip(t *testing.T, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	name := "wb2api"
	if runtime.GOOS == "windows" {
		name = "wb2api.exe"
	}
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newSelfUpdatePanel 起一个假 GitHub（TLS）：/repos/o/r/releases/latest + /asset.zip。
func newSelfUpdatePanel(t *testing.T, curVersion string, zipBytes []byte, digest string) (*Panel, string, <-chan string) {
	t.Helper()
	assetName := fmt.Sprintf("wb2api-panel-2026.10.25.1900-%s-%s.zip", runtime.GOOS, runtime.GOARCH)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			rel := map[string]any{
				"tag_name":     "v2026.10.25.1900",
				"published_at": "2026-10-25T10:00:00Z",
				"body":         "## 修复\n- 一堆东西",
				"assets": []map[string]any{{
					"name":                 assetName,
					"size":                 len(zipBytes),
					"browser_download_url": "https://" + r.Host + "/browser-download.zip", // 私有仓库这个地址回 404
					"url":                  "https://" + r.Host + "/asset.zip",            // 真正可下载的是 API 地址
					"digest":               digest,
				}},
			}
			json.NewEncoder(w).Encode(rel)
		case r.URL.Path == "/asset.zip":
			w.Header().Set("Content-Type", "application/zip")
			w.Write(zipBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	dataDir := t.TempDir()
	execCh := make(chan string, 1)
	pn := New(Config{
		Version:    curVersion,
		APIKey:     "test-key",
		Pool:       nil,
		DataDir:    dataDir,
		HTTPClient: srv.Client(),
		SelfUpdate: SelfUpdateConfig{
			Enabled: true,
			Repo:    "o/r",
			APIBase: srv.URL,
		},
		ExecSelf: func(path string, args []string) error {
			select {
			case execCh <- path:
			default:
			}
			return nil
		},
	})
	return pn, dataDir, execCh
}

// TestSelfUpdateCheckFindsNewer 检查更新：新版本 → update_available，资产按平台挑。
func TestSelfUpdateCheckFindsNewer(t *testing.T) {
	zipBytes := buildReleaseZip(t, "BIN")
	sum := sha256.Sum256(zipBytes)
	pn, _, _ := newSelfUpdatePanel(t, "2026.10.1.1000", zipBytes, "sha256:"+hex.EncodeToString(sum[:]))

	rec := doAccountMetaRequest(t, pn, "GET", "/panel/api/selfupdate/check", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Enabled         bool   `json:"enabled"`
		Current         string `json:"current"`
		LatestVersion   string `json:"latest_version"`
		UpdateAvailable bool   `json:"update_available"`
		Status          string `json:"status"`
		Asset           struct {
			Name      string `json:"name"`
			HasDigest bool   `json:"has_digest"`
		} `json:"asset"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Enabled || !resp.UpdateAvailable || resp.LatestVersion != "2026.10.25.1900" {
		t.Fatalf("resp=%+v", resp)
	}
	if resp.Status != "update_available" {
		t.Fatalf("status=%q, want update_available", resp.Status)
	}
	if !strings.Contains(resp.Asset.Name, runtime.GOOS+"-"+runtime.GOARCH) || !resp.Asset.HasDigest {
		t.Fatalf("asset=%+v", resp.Asset)
	}
}

// TestSelfUpdateApplyInstallsAndExecs 下载→校验→落盘→exec，全链路。
func TestSelfUpdateApplyInstallsAndExecs(t *testing.T) {
	zipBytes := buildReleaseZip(t, "NEW-BINARY")
	sum := sha256.Sum256(zipBytes)
	pn, dataDir, execCh := newSelfUpdatePanel(t, "2026.10.1.1000", zipBytes, "sha256:"+hex.EncodeToString(sum[:]))

	rec := doAccountMetaRequest(t, pn, "POST", "/panel/api/selfupdate/apply", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
		Path    string `json:"path"`
		Sha256  string `json:"sha256"`
		Restart string `json:"restart"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Version != "2026.10.25.1900" || resp.Restart != "exec" {
		t.Fatalf("resp=%+v", resp)
	}
	if resp.Sha256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha256=%s want %x", resp.Sha256, sum[:])
	}
	raw, err := os.ReadFile(resp.Path)
	if err != nil || string(raw) != "NEW-BINARY" {
		t.Fatalf("落盘内容不对: %q err=%v", raw, err)
	}
	select {
	case got := <-execCh:
		if got != resp.Path {
			t.Fatalf("exec 路径 %q want %q", got, resp.Path)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("没有调用 exec 换进程")
	}
	// 状态文件要能让人看出来源
	rawState, err := os.ReadFile(filepath.Join(dataDir, "updates", "current.json"))
	if err != nil || !strings.Contains(string(rawState), "2026.10.25.1900") {
		t.Fatalf("current.json=%s err=%v", rawState, err)
	}
}

// TestSelfUpdateRejectsBadDigest 校验不过时不许落盘，正在跑的版本不受影响。
func TestSelfUpdateRejectsBadDigest(t *testing.T) {
	zipBytes := buildReleaseZip(t, "TAMPERED")
	pn, dataDir, execCh := newSelfUpdatePanel(t, "2026.10.1.1000", zipBytes, "sha256:"+strings.Repeat("0", 64))

	rec := doAccountMetaRequest(t, pn, "POST", "/panel/api/selfupdate/apply", "")
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "校验失败") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if entries, _ := os.ReadDir(filepath.Join(dataDir, "updates")); len(entries) != 0 {
		t.Fatalf("校验失败不该落盘: %v", entries)
	}
	select {
	case <-execCh:
		t.Fatal("校验失败不该 exec")
	default:
	}
}

// newSelfUpdatePanelAt 起一个自定义 TLS 服务并装配面板（限流/缓存用例共用）。
func newSelfUpdatePanelAt(t *testing.T, h http.Handler, version string) *Panel {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return New(Config{
		Version:    version,
		APIKey:     "test-key",
		DataDir:    t.TempDir(),
		HTTPClient: srv.Client(),
		SelfUpdate: SelfUpdateConfig{
			Enabled: true,
			Repo:    "o/r",
			APIBase: srv.URL,
		},
	})
}

// TestSelfUpdateRateLimitAndCache 403 + 配额为 0 时报「限流」并给出重置时间，
// 不能误报成 token 无效；同一分钟内重复查询命中 60 秒缓存，force=1 强制刷新。
func TestSelfUpdateRateLimitAndCache(t *testing.T) {
	var hits atomic.Int64
	reset := time.Now().Add(5 * time.Minute).Unix()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 只统计 API 调用；网页兜底这里故意 404（模拟私有/不存在），
		// 验证失败时仍返回 API 的限流文案而不是别的东西。
		if !strings.HasPrefix(r.URL.Path, "/repos/o/r/releases/latest") {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
		w.WriteHeader(http.StatusForbidden)
	})
	pn := newSelfUpdatePanelAt(t, h, "2026.10.1.1000")

	type checkResp struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	get := func(path string) checkResp {
		t.Helper()
		rec := doAccountMetaRequest(t, pn, "GET", path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s code=%d body=%s", path, rec.Code, rec.Body.String())
		}
		var resp checkResp
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := get("/panel/api/selfupdate/check")
	if resp.Status != "unknown" {
		t.Fatalf("status=%q, want unknown", resp.Status)
	}
	if !strings.Contains(resp.Reason, "限流") || !strings.Contains(resp.Reason, "重置") {
		t.Fatalf("reason=%q，应说明限流与重置时间，且不能误导成 token 问题", resp.Reason)
	}
	if strings.Contains(resp.Reason, "token 无效") {
		t.Fatalf("reason=%q 不应把配额用尽报成 token 无效", resp.Reason)
	}
	if hits.Load() != 1 {
		t.Fatalf("首次查询应打 1 次上游，实际 %d", hits.Load())
	}

	// 60 秒内再查（维护页第二条查询）：命中缓存，不再消耗 GitHub 配额。
	_ = get("/panel/api/selfupdate/check")
	if hits.Load() != 1 {
		t.Fatalf("缓存期内不应重复打上游，实际 %d", hits.Load())
	}

	// 手点「检查更新」：force=1 绕过缓存。
	_ = get("/panel/api/selfupdate/check?force=1")
	if hits.Load() != 2 {
		t.Fatalf("force=1 应强制刷新，实际 %d", hits.Load())
	}
}

// TestSelfUpdateWebFallbackWithoutToken API 被限流（无 token）时，退回网页 302
// 解析最新 tag 并拼出资产地址——公开仓库的用户不建 PAT 也能检查更新。
func TestSelfUpdateWebFallbackWithoutToken(t *testing.T) {
	var apiHits, webHits atomic.Int64
	const tag = "v2026.10.25.1900"
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// API 路径也以 /releases/latest 结尾，必须先判前缀。
		if strings.HasPrefix(r.URL.Path, "/repos/o/r/releases/latest") {
			apiHits.Add(1)
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			webHits.Add(1)
			w.Header().Set("Location", "https://"+r.Host+"/o/r/releases/tag/"+tag)
			w.WriteHeader(http.StatusFound)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/o/r/releases/download/") {
			w.Header().Set("Content-Length", "1234")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	})
	pn := newSelfUpdatePanelAt(t, h, "2026.10.1.1000")

	rec := doAccountMetaRequest(t, pn, "GET", "/panel/api/selfupdate/check?force=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status          string `json:"status"`
		UpdateAvailable bool   `json:"update_available"`
		LatestVersion   string `json:"latest_version"`
		Reason          string `json:"reason"`
		Asset           struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"asset"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.UpdateAvailable || resp.Status != "update_available" || resp.LatestVersion != "2026.10.25.1900" {
		t.Fatalf("resp=%+v，网页兜底应能给出最新版本", resp)
	}
	wantAsset := fmt.Sprintf("wb2api-panel-2026.10.25.1900-%s-%s.zip", runtime.GOOS, runtime.GOARCH)
	if resp.Asset.Name != wantAsset || resp.Asset.Size != 1234 {
		t.Fatalf("asset=%+v，want %s/1234", resp.Asset, wantAsset)
	}
	if apiHits.Load() != 1 || webHits.Load() != 1 {
		t.Fatalf("apiHits=%d webHits=%d，want 各 1 次（先 API 后网页兜底）", apiHits.Load(), webHits.Load())
	}
}

// TestSelfUpdateCheckUpToDate 已是最新：status=latest（前端据此显示「已是最新」，
// 而不是把 reason 里的版本说明误判成「无法确认」）。
func TestSelfUpdateCheckUpToDate(t *testing.T) {
	zipBytes := buildReleaseZip(t, "BIN")
	sum := sha256.Sum256(zipBytes)
	pn, _, _ := newSelfUpdatePanel(t, "2026.10.25.1900", zipBytes, "sha256:"+hex.EncodeToString(sum[:]))

	rec := doAccountMetaRequest(t, pn, "GET", "/panel/api/selfupdate/check", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		UpdateAvailable bool   `json:"update_available"`
		Status          string `json:"status"`
		Reason          string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.UpdateAvailable {
		t.Fatalf("同版本不应判有更新：%+v", resp)
	}
	if resp.Status != "latest" {
		t.Fatalf("status=%q, want latest（reason=%q）", resp.Status, resp.Reason)
	}
	if !strings.Contains(resp.Reason, "已是最新") {
		t.Fatalf("reason=%q，应保留版本对比说明", resp.Reason)
	}
}

// TestSelfUpdateDisabled 默认关：check 给出原因，apply 403。
func TestSelfUpdateDisabled(t *testing.T) {
	pn := New(Config{Version: "2026.10.1.1000", APIKey: "test-key", DataDir: t.TempDir()})
	rec := doAccountMetaRequest(t, pn, "GET", "/panel/api/selfupdate/check", "")
	var resp struct {
		Enabled bool   `json:"enabled"`
		Reason  string `json:"reason"`
		Status  string `json:"status"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Enabled || !strings.Contains(resp.Reason, "未启用") {
		t.Fatalf("resp=%+v", resp)
	}
	if resp.Status != "disabled" {
		t.Fatalf("status=%q, want disabled", resp.Status)
	}
	rec = doAccountMetaRequest(t, pn, "POST", "/panel/api/selfupdate/apply", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d want 403", rec.Code)
	}
}

// TestSelfUpdateApplyRequiresAPIKey 默认启用之后，api_key 就是唯一兜底：
// 无鉴权的面板（本机/内网裸跑）不允许对外的请求替换二进制。
func TestSelfUpdateApplyRequiresAPIKey(t *testing.T) {
	zipBytes := buildReleaseZip(t, "BIN")
	pn, _, _ := newSelfUpdatePanel(t, "2026.10.1.1000", zipBytes, "")
	pn.cfg.APIKey = ""
	req := httptest.NewRequest("POST", "/panel/api/selfupdate/apply", nil)
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "api_key") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}
