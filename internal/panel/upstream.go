// upstream.go 面板「一键同步上游」：两种部署形态各走一条路。
//
//   - cnb：面板只负责调 CNB 的构建接口（POST /{repo}/-/build/start），真正的
//     合并/编译/提交在构建环境里由 .cnb.yml 跑 scripts/sync_upstream.sh --auto。
//     容器/镜像部署没有 git 与源码树，走这条。
//   - local：本机就是源码检出，直接在 workdir 里跑同步脚本。要求 .git 与 PATH
//     里有 git，否则脚本自己第一步就退出。
//
// POST 立即返回（后台 goroutine 执行），状态靠 GET 轮询；单飞锁保证同一时间
// 只有一个任务。命令/仓库/token 全部来自配置，请求体不参与任何决策。
package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// UpstreamSyncConfig 「一键同步上游」配置。面板与 cmd/server 共用这一份定义
// （cmd/server/config.go 的 panel.upstream_sync 直接内嵌本结构体），避免两处
// 字段/JSON 名漂移。
type UpstreamSyncConfig struct {
	Enabled bool   `json:"enabled"`  // 默认 false
	Mode    string `json:"mode"`     // "cnb"（默认）| "local"
	Repo    string `json:"repo"`     // cnb 模式：organization/repo
	Event   string `json:"event"`    // 默认 "api_trigger_sync_upstream"
	Branch  string `json:"branch"`   // 默认 "main"
	APIBase string `json:"api_base"` // 默认 "https://api.cnb.cool"
	Token   string `json:"token"`    // 空则回退环境变量 CNB_TOKEN
	Workdir string `json:"workdir"`  // local 模式：仓库根目录，默认 "."
	Command string `json:"command"`  // local 模式：默认 "bash scripts/sync_upstream.sh --auto"
	Timeout string `json:"timeout"`  // 默认 "10m"
}

// normalized 归一默认值（面板侧再兜一次底：直接构造 Config 的调用方未必经过
// cmd/server 的 normalize）。token 回退 CNB_TOKEN 在此完成——配置里不落明文
// 时，部署环境注入即可。
func (c UpstreamSyncConfig) normalized() UpstreamSyncConfig {
	c.Mode = strings.ToLower(strings.TrimSpace(c.Mode))
	if c.Mode == "" {
		c.Mode = "cnb"
	}
	if strings.TrimSpace(c.Event) == "" {
		c.Event = "api_trigger_sync_upstream"
	}
	if strings.TrimSpace(c.Branch) == "" {
		c.Branch = "main"
	}
	c.APIBase = strings.TrimRight(strings.TrimSpace(c.APIBase), "/")
	if c.APIBase == "" {
		c.APIBase = "https://api.cnb.cool"
	}
	c.Repo = strings.Trim(strings.TrimSpace(c.Repo), "/")
	if strings.TrimSpace(c.Workdir) == "" {
		c.Workdir = "."
	}
	if strings.TrimSpace(c.Command) == "" {
		c.Command = "bash scripts/sync_upstream.sh --auto"
	}
	if strings.TrimSpace(c.Timeout) == "" {
		c.Timeout = "10m"
	}
	if c.Token == "" {
		c.Token = os.Getenv("CNB_TOKEN")
	}
	return c
}

// timeoutDuration 解析 timeout；空/非法/非正 回落 10m（配置校验在
// cmd/server 侧已 fail fast，这里只做运行期兜底）。
func (c UpstreamSyncConfig) timeoutDuration() time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(c.Timeout))
	if err != nil || d <= 0 {
		return 10 * time.Minute
	}
	return d
}

const (
	// upstreamSyncOutputLimit 输出上限 64 KiB：超出保留尾部并标注截断。
	// 同步脚本会打印编译/测试流水，冲突时可能刷很久，留尾部才看得到结论。
	upstreamSyncOutputLimit = 64 << 10
	// upstreamSyncResponseLimit CNB 响应体读取上限（解析 JSON 用，足够大）。
	upstreamSyncResponseLimit = 1 << 20
)

// upstreamSyncState 一次任务的状态。status 读取与后台写入并发，必须过锁。
type upstreamSyncState struct {
	mu         sync.Mutex
	running    bool
	startedAt  time.Time
	finishedAt time.Time
	ok         bool
	output     string
	sn         string
	buildURL   string
	errMsg     string
}

// begin 抢占单飞锁并清空上一轮结果；已在跑返回 false。
func (s *upstreamSyncState) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return false
	}
	s.running = true
	s.startedAt = time.Now()
	s.finishedAt = time.Time{}
	s.ok = false
	s.output = ""
	s.sn = ""
	s.buildURL = ""
	s.errMsg = ""
	return true
}

func (s *upstreamSyncState) finish(sn, buildURL, output string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	s.finishedAt = time.Now()
	s.ok = err == nil
	s.output = output
	s.sn = sn
	s.buildURL = buildURL
	s.errMsg = ""
	if err != nil {
		s.errMsg = err.Error()
	}
}

// upstreamSyncStatus 是 GET /panel/api/upstream/status 的响应体。
type upstreamSyncStatus struct {
	Enabled    bool   `json:"enabled"`
	Mode       string `json:"mode"`
	Running    bool   `json:"running"`
	StartedAt  string `json:"started_at"`  // RFC3339；未跑过为空
	FinishedAt string `json:"finished_at"` // RFC3339；未跑过为空
	OK         bool   `json:"ok"`
	Output     string `json:"output"`
	SN         string `json:"sn"`
	BuildURL   string `json:"build_url"`
	Error      string `json:"error"`
}

func (s *upstreamSyncState) snapshot(cfg UpstreamSyncConfig) upstreamSyncStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return upstreamSyncStatus{
		Enabled:    cfg.Enabled,
		Mode:       cfg.Mode,
		Running:    s.running,
		StartedAt:  rfc3339OrEmpty(s.startedAt),
		FinishedAt: rfc3339OrEmpty(s.finishedAt),
		OK:         s.ok,
		Output:     s.output,
		SN:         s.sn,
		BuildURL:   s.buildURL,
		Error:      s.errMsg,
	}
}

func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// upstreamSyncStart POST /panel/api/upstream/sync：立即返回 202，后台执行。
func (p *Panel) upstreamSyncStart(w http.ResponseWriter, r *http.Request) {
	cfg := p.cfg.UpstreamSync.normalized()
	if !cfg.Enabled {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "upstream sync disabled"})
		return
	}
	if !p.syncState.begin() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "sync already running"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started", "mode": cfg.Mode})
	go p.runUpstreamSync(cfg)
}

// upstreamSyncStatusHandler GET /panel/api/upstream/status。
func (p *Panel) upstreamSyncStatusHandler(w http.ResponseWriter, r *http.Request) {
	cfg := p.cfg.UpstreamSync.normalized()
	writeJSON(w, http.StatusOK, p.syncState.snapshot(cfg))
}

func (p *Panel) runUpstreamSync(cfg UpstreamSyncConfig) {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeoutDuration())
	defer cancel()

	var (
		sn, buildURL, output string
		err                  error
	)
	switch cfg.Mode {
	case "local":
		output, err = runLocalUpstreamSync(ctx, cfg)
	case "cnb":
		sn, buildURL, output, err = runCNBUpstreamSync(ctx, cfg)
	default:
		err = fmt.Errorf("未知同步模式 %q（支持 cnb / local）", cfg.Mode)
	}
	p.syncState.finish(sn, buildURL, output, err)
}

// runCNBUpstreamSync 调 CNB 构建接口触发同步流水线。
//
// 注意：token 只进 Authorization 头，绝不写进 output/error——状态接口是面板可读的。
func runCNBUpstreamSync(ctx context.Context, cfg UpstreamSyncConfig) (sn, buildURL, output string, err error) {
	if cfg.Repo == "" {
		return "", "", "", fmt.Errorf("panel.upstream_sync.repo 未配置（cnb 模式需要 organization/repo）")
	}
	if cfg.Token == "" {
		return "", "", "", fmt.Errorf("panel.upstream_sync.token 未配置，且环境变量 CNB_TOKEN 为空")
	}
	payload, mErr := json.Marshal(map[string]any{
		"branch": cfg.Branch,
		"event":  cfg.Event,
		"sync":   false,
	})
	if mErr != nil {
		return "", "", "", fmt.Errorf("构造 CNB 请求体失败: %w", mErr)
	}
	endpoint := cfg.APIBase + "/" + cfg.Repo + "/-/build/start"
	req, rErr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if rErr != nil {
		return "", "", "", fmt.Errorf("构造 CNB 请求失败: %w", rErr)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, dErr := http.DefaultClient.Do(req)
	if dErr != nil {
		return "", "", "", fmt.Errorf("请求 CNB 构建接口失败: %w", dErr)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamSyncResponseLimit))
	output = tailTruncate(strings.TrimSpace(string(body)))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", output, fmt.Errorf("CNB 构建接口返回 HTTP %d", resp.StatusCode)
	}
	sn, buildURL = parseCNBBuild(body)
	if sn == "" && buildURL == "" {
		// 2xx 但没解析出字段：不判失败（接口形态可能演进），只把原响应留在 output。
		return "", "", output, nil
	}
	return sn, buildURL, output, nil
}

// parseCNBBuild 从响应里取 sn 与 buildLogUrl。字段可能在顶层或 data 下，
// 命名有 camelCase / snake_case 两种写法，都兼容。
func parseCNBBuild(raw []byte) (sn, buildURL string) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return "", ""
	}
	layers := []map[string]json.RawMessage{top}
	if data, ok := top["data"]; ok {
		var dm map[string]json.RawMessage
		if err := json.Unmarshal(data, &dm); err == nil && dm != nil {
			layers = append(layers, dm)
		}
	}
	for _, layer := range layers {
		if sn == "" {
			sn = firstJSONValue(layer, "sn", "SN", "buildSn", "build_sn")
		}
		if buildURL == "" {
			buildURL = firstJSONValue(layer, "buildLogUrl", "buildLogURL", "build_log_url")
		}
	}
	return sn, buildURL
}

// firstJSONValue 依次取第一个能转成字符串的键（兼容数字型 sn）。
func firstJSONValue(m map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
			continue
		}
		var n json.Number
		if err := json.Unmarshal(raw, &n); err == nil && n.String() != "" {
			return n.String()
		}
	}
	return ""
}

// runLocalUpstreamSync 在源码检出里跑同步脚本。
//
// 前置检查（.git + PATH 里的 git）先做，给出可操作提示——容器/镜像部署里
// 这两样都没有，用户看到 shell 的 "not a git repository" 会一头雾水。
func runLocalUpstreamSync(ctx context.Context, cfg UpstreamSyncConfig) (string, error) {
	workdir := cfg.Workdir
	if abs, err := filepath.Abs(workdir); err == nil {
		workdir = abs
	}
	st, err := os.Stat(workdir)
	if err != nil {
		return "", fmt.Errorf("upstream_sync.workdir %s 不可访问: %v", workdir, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("upstream_sync.workdir %s 不是目录", workdir)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".git")); err != nil {
		return "", fmt.Errorf("upstream_sync.workdir %s 不是源码检出（缺少 .git）：容器/镜像部署没有源码树，请改用 cnb 模式或挂载源码", workdir)
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("PATH 中找不到 git：容器/镜像部署没有源码树，请改用 cnb 模式或挂载源码")
	}

	buf := &tailBuffer{limit: upstreamSyncOutputLimit}
	cmd := exec.CommandContext(ctx, "sh", "-c", cfg.Command) // #nosec G204 -- 命令来自配置，不接受请求体
	cmd.Dir = workdir
	cmd.Stdout = buf
	cmd.Stderr = buf
	// 超时杀掉 sh 后，孙进程可能还握着输出管道：WaitDelay 兜底，避免 Run 永久阻塞。
	cmd.WaitDelay = 5 * time.Second
	runErr := cmd.Run()
	out := buf.String()
	if ctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("同步命令超时（%s）", cfg.timeoutDuration())
	}
	if runErr != nil {
		return out, fmt.Errorf("同步命令失败: %w", runErr)
	}
	return out, nil
}

// tailBuffer 只保留最近 limit 字节的写入缓冲（stdout/stderr 合并）。
type tailBuffer struct {
	mu        sync.Mutex
	limit     int
	buf       []byte
	truncated bool
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.limit {
		cut := len(b.buf) - b.limit
		b.buf = append(b.buf[:0], b.buf[cut:]...)
		b.truncated = true
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := string(b.buf)
	if b.truncated {
		out = fmt.Sprintf("[输出超出上限，已截断，仅保留最后 %d KiB]\n%s", b.limit>>10, out)
	}
	return out
}

// tailTruncate 对已读入的字符串做同样的尾部截断（CNB 响应体用）。
func tailTruncate(s string) string {
	if len(s) <= upstreamSyncOutputLimit {
		return s
	}
	cut := len(s) - upstreamSyncOutputLimit
	for cut < len(s) && !isUTF8Start(s[cut]) {
		cut++
	}
	return fmt.Sprintf("[输出超出上限，已截断，仅保留最后 %d KiB]\n%s", upstreamSyncOutputLimit>>10, s[cut:])
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }
