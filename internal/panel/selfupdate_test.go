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
	"strings"
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
					"browser_download_url": "https://" + r.Host + "/asset.zip",
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

// TestSelfUpdateDisabled 默认关：check 给出原因，apply 403。
func TestSelfUpdateDisabled(t *testing.T) {
	pn := New(Config{Version: "2026.10.1.1000", APIKey: "test-key", DataDir: t.TempDir()})
	rec := doAccountMetaRequest(t, pn, "GET", "/panel/api/selfupdate/check", "")
	var resp struct {
		Enabled bool   `json:"enabled"`
		Reason  string `json:"reason"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Enabled || !strings.Contains(resp.Reason, "未启用") {
		t.Fatalf("resp=%+v", resp)
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
