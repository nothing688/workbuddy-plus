package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const upstreamTestKey = "test-key"

// newUpstreamTestPanel 构造只装「一键同步上游」所需字段的面板。
func newUpstreamTestPanel(cfg UpstreamSyncConfig) *Panel {
	return New(Config{APIKey: upstreamTestKey, UpstreamSync: cfg})
}

func doUpstreamRequest(t *testing.T, p *Panel, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+upstreamTestKey)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func upstreamStatus(t *testing.T, p *Panel) upstreamSyncStatus {
	t.Helper()
	rec := doUpstreamRequest(t, p, http.MethodGet, "/panel/api/upstream/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var st upstreamSyncStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode status: %v (%s)", err, rec.Body.String())
	}
	return st
}

// waitUpstreamDone 轮询状态直到任务结束（异步执行，POST 只保证已启动）。
func waitUpstreamDone(t *testing.T, p *Panel) upstreamSyncStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st := upstreamStatus(t, p)
		if !st.Running {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("同步任务在 10s 内未结束")
	return upstreamSyncStatus{}
}

// TestUpstreamSyncDisabled 未启用 → 400 + 可读错误。
func TestUpstreamSyncDisabled(t *testing.T) {
	p := newUpstreamTestPanel(UpstreamSyncConfig{})
	rec := doUpstreamRequest(t, p, http.MethodPost, "/panel/api/upstream/sync")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "upstream sync disabled" {
		t.Fatalf("error = %q", body["error"])
	}
}

// TestUpstreamSyncCNB 假 CNB 接口：断言请求体 / Bearer 头 / 状态里的 sn 与 build_url。
func TestUpstreamSyncCNB(t *testing.T) {
	const token = "cnb-secret-token"
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"sn":"sn-42","buildLogUrl":"https://cnb.example/build/42"}}`))
	}))
	defer srv.Close()

	p := newUpstreamTestPanel(UpstreamSyncConfig{
		Enabled: true,
		Mode:    "cnb",
		Repo:    "org/repo",
		Token:   token,
		APIBase: srv.URL,
	})
	rec := doUpstreamRequest(t, p, http.MethodPost, "/panel/api/upstream/sync")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, want 202; body = %s", rec.Code, rec.Body.String())
	}
	var started map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started["status"] != "started" || started["mode"] != "cnb" {
		t.Fatalf("202 body = %v", started)
	}

	// POST 只保证已启动：等后台任务跑完再断言假 API 收到的请求。
	st := waitUpstreamDone(t, p)
	if st.Mode != "cnb" || !st.Enabled {
		t.Errorf("enabled/mode = %v/%q", st.Enabled, st.Mode)
	}
	if !st.OK || st.Error != "" {
		t.Fatalf("ok=%v error=%q", st.OK, st.Error)
	}
	if st.SN != "sn-42" || st.BuildURL != "https://cnb.example/build/42" {
		t.Errorf("sn=%q build_url=%q", st.SN, st.BuildURL)
	}
	if st.StartedAt == "" || st.FinishedAt == "" {
		t.Errorf("started_at/finished_at 为空: %+v", st)
	}
	if strings.Contains(st.Output, token) {
		t.Errorf("状态输出泄露 token: %q", st.Output)
	}

	want := map[string]any{"branch": "main", "event": "api_trigger_sync_upstream", "sync": false}
	var payload map[string]any
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("请求体不是 JSON: %q", gotBody)
	}
	for k, v := range want {
		if payload[k] != v {
			t.Errorf("%s = %v, want %v (body=%s)", k, payload[k], v, gotBody)
		}
	}
	if gotPath != "/org/repo/-/build/start" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer "+token {
		t.Errorf("Authorization = %q", gotAuth)
	}
}

// TestUpstreamSyncLocal 临时目录 + echo 命令：断言输出与 ok。
func TestUpstreamSyncLocal(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("PATH 中没有 git，local 模式无法运行")
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := newUpstreamTestPanel(UpstreamSyncConfig{
		Enabled: true,
		Mode:    "local",
		Workdir: dir,
		Command: "echo hello-upstream",
	})
	rec := doUpstreamRequest(t, p, http.MethodPost, "/panel/api/upstream/sync")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d; body = %s", rec.Code, rec.Body.String())
	}
	st := waitUpstreamDone(t, p)
	if !st.OK || st.Error != "" {
		t.Fatalf("ok=%v error=%q output=%q", st.OK, st.Error, st.Output)
	}
	if !strings.Contains(st.Output, "hello-upstream") {
		t.Fatalf("output = %q", st.Output)
	}
}

// TestUpstreamSyncLocalNotSourceTree 非源码检出目录 → 可读错误（指引改用 cnb）。
func TestUpstreamSyncLocalNotSourceTree(t *testing.T) {
	dir := t.TempDir() // 没有 .git
	p := newUpstreamTestPanel(UpstreamSyncConfig{
		Enabled: true,
		Mode:    "local",
		Workdir: dir,
		Command: "echo should-not-run",
	})
	rec := doUpstreamRequest(t, p, http.MethodPost, "/panel/api/upstream/sync")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d; body = %s", rec.Code, rec.Body.String())
	}
	st := waitUpstreamDone(t, p)
	if st.OK {
		t.Fatal("缺少 .git 时不应 ok")
	}
	if !strings.Contains(st.Error, "没有源码树") || !strings.Contains(st.Error, "cnb") {
		t.Fatalf("error = %q，应提示容器部署没有源码树并指向 cnb 模式", st.Error)
	}
}

// TestUpstreamSyncAlreadyRunning 第二次触发 → 409。
func TestUpstreamSyncAlreadyRunning(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("PATH 中没有 git，local 模式无法运行")
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := newUpstreamTestPanel(UpstreamSyncConfig{
		Enabled: true,
		Mode:    "local",
		Workdir: dir,
		Command: "sleep 2",
	})
	first := doUpstreamRequest(t, p, http.MethodPost, "/panel/api/upstream/sync")
	if first.Code != http.StatusAccepted {
		t.Fatalf("first code = %d; body = %s", first.Code, first.Body.String())
	}
	second := doUpstreamRequest(t, p, http.MethodPost, "/panel/api/upstream/sync")
	if second.Code != http.StatusConflict {
		t.Fatalf("second code = %d, want 409; body = %s", second.Code, second.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(second.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "sync already running" {
		t.Fatalf("error = %q", body["error"])
	}
}
