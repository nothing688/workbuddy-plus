// selfupdate.go 面板的「GitHub 热更新」：查 Release、下二进制、校验、替换、重启。
//
// 生产镜像里没有 git 也没有 Go 工具链，所以「拉源码编译」不成立；能自更新的只有
// 发布产物本身：GitHub Release 里按平台打包的 wb2api-panel-<ver>-<os>-<arch>.zip。
// 流程刻意做成「下载 → 校验 → 原子落盘 → exec 换进程」，任一步失败都不会动正在跑的
// 那个二进制。
//
// 默认关闭（panel.self_update.enabled=false）：这是一个能换掉自己代码的能力，
// 不该默认对外暴露；开启后也仍然只认配置里那一个仓库的 Release。
package panel

import (
	"archive/zip"
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/selfupdate"
)

// SelfUpdateConfig 面板热更新配置（config.json 的 panel.self_update）。
type SelfUpdateConfig struct {
	Enabled bool   `json:"enabled"`
	Repo    string `json:"repo"`     // owner/name，默认 nothing688/workbuddy-plus
	Token   string `json:"token"`    // 可选：私有仓库 / 提高 API 限额
	APIBase string `json:"api_base"` // 默认 https://api.github.com（测试可指向假服务）
}

const (
	// selfUpdateMaxBytes 单个发布包上限。二进制约 15MB，留足余量防被超大盘拖垮。
	selfUpdateMaxBytes = 256 << 20
	// selfUpdateNotesMax Release 说明回显上限（面板只展示摘要）。
	selfUpdateNotesMax = 1500
)

// normalizeSelfUpdate 填默认值，避免配置里只写 enabled 就不可用。
func normalizeSelfUpdate(c SelfUpdateConfig) SelfUpdateConfig {
	c.Repo = strings.Trim(strings.TrimSpace(c.Repo), "/")
	if c.Repo == "" {
		c.Repo = "nothing688/workbuddy-plus"
	}
	c.APIBase = strings.TrimRight(strings.TrimSpace(c.APIBase), "/")
	if c.APIBase == "" {
		c.APIBase = "https://api.github.com"
	}
	return c
}

// releaseAsset 单个发布资产。digest 形如 "sha256:<hex>"（GitHub 自 2025 起提供）。
type releaseAsset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// URL 是网页下载地址（browser_download_url）。私有仓库未登录时它回 404，
	// 真正能带 token 下载的是 API 地址（url + Accept: application/octet-stream）。
	URL    string `json:"browser_download_url"`
	APIURL string `json:"url"`
	Digest string `json:"digest"`
}

// releaseInfo GitHub Release 的必要字段（其余忽略）。
type releaseInfo struct {
	TagName     string         `json:"tag_name"`
	PublishedAt time.Time      `json:"published_at"`
	Body        string         `json:"body"`
	Assets      []releaseAsset `json:"assets"`
}

// selfUpdateCheck GET /panel/api/selfupdate/check
// 只读：查最新 Release、挑当前平台的资产、和运行中版本比大小。不下载。
func (p *Panel) selfUpdateCheck(w http.ResponseWriter, r *http.Request) {
	cfg := p.selfUpdateCfg()
	cur := p.cfg.Version
	resp := map[string]any{
		"enabled":          cfg.Enabled,
		"repo":             cfg.Repo,
		"current":          cur,
		"update_available": false,
	}
	if !cfg.Enabled {
		resp["reason"] = "未启用：在 config.json 设 panel.self_update.enabled=true 并重启"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	rel, err := p.fetchLatestRelease(cfg)
	if err != nil {
		resp["reason"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	asset, why := pickReleaseAsset(rel, runtime.GOOS, runtime.GOARCH)
	latest := strings.TrimPrefix(rel.TagName, "v")
	resp["latest_tag"] = rel.TagName
	resp["latest_version"] = latest
	resp["published_at"] = rel.PublishedAt
	if asset == nil {
		resp["reason"] = why
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp["asset"] = map[string]any{
		"name": asset.Name, "size": asset.Size,
		"digest": asset.Digest, "has_digest": asset.Digest != "",
	}
	// 版本号比较用日期式逐段比，不能用字符串（不补零，字典序会反）。
	if selfupdate.CompareVersion(latest, cur) > 0 {
		resp["update_available"] = true
		resp["notes"] = truncateStr(rel.Body, selfUpdateNotesMax)
	} else {
		resp["reason"] = "已是最新（运行中 " + cur + "，最新 " + latest + "）"
	}
	writeJSON(w, http.StatusOK, resp)
}

// selfUpdateApply POST /panel/api/selfupdate/apply
// 下载 → 校验 digest → 从 zip 取二进制 → 原子落盘 → 回包后 exec 换进程。
func (p *Panel) selfUpdateApply(w http.ResponseWriter, r *http.Request) {
	cfg := p.selfUpdateCfg()
	if !cfg.Enabled {
		writeErr(w, http.StatusForbidden, "未启用热更新：在 config.json 设 panel.self_update.enabled=true 并重启")
		return
	}
	if p.cfg.DataDir == "" {
		writeErr(w, http.StatusInternalServerError, "未配置数据目录，无法落地新版本")
		return
	}
	// 无鉴权的面板等于把「换掉自己」开放给任何能访问 7863 的人；这条闸比 enabled 更硬，
	// 默认启用之后它就是唯一的兜底。
	if p.apiKey() == "" {
		writeErr(w, http.StatusForbidden, "面板未设置 api_key：拒绝在无鉴权环境下替换二进制")
		return
	}
	rel, err := p.fetchLatestRelease(cfg)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "查询 Release 失败："+err.Error())
		return
	}
	latest := strings.TrimPrefix(rel.TagName, "v")
	asset, why := pickReleaseAsset(rel, runtime.GOOS, runtime.GOARCH)
	if asset == nil {
		writeErr(w, http.StatusBadRequest, why)
		return
	}
	if selfupdate.CompareVersion(latest, p.cfg.Version) <= 0 {
		writeErr(w, http.StatusBadRequest, "已是最新版本（运行中 "+p.cfg.Version+"，最新 "+latest+"）")
		return
	}
	dlURL := asset.APIURL
	if dlURL == "" {
		dlURL = asset.URL
	}
	if u, err := url.Parse(dlURL); err != nil || u.Scheme != "https" {
		writeErr(w, http.StatusBadGateway, "资产地址不是 https，拒绝下载")
		return
	}

	// 下载到临时文件并同时算 sha256：digest 校验必须在解压前完成。
	tmp, err := os.CreateTemp("", "wb2api-update-*.zip")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "创建临时文件失败："+err.Error())
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	h := sha256.New()
	n, err := p.downloadTo(io.MultiWriter(tmp, h), dlURL, cfg.Token)
	tmp.Close()
	if err != nil {
		writeErr(w, http.StatusBadGateway, "下载失败："+err.Error())
		return
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if asset.Digest != "" {
		want := strings.TrimPrefix(strings.ToLower(asset.Digest), "sha256:")
		if want != sum {
			writeErr(w, http.StatusBadGateway, "校验失败：sha256 不匹配（期望 "+want+"，实际 "+sum+"）")
			return
		}
	}

	bin, closeFn, err := openZipBinary(tmpName)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "发布包内容异常："+err.Error())
		return
	}
	defer closeFn()
	path, err := selfupdate.Install(p.cfg.DataDir, latest, rel.TagName, asset.Name, bin)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "安装失败："+err.Error())
		return
	}
	log.Printf("面板: 热更新已安装 %s（%s，%d 字节，sha256=%s…）→ %s", latest, asset.Name, n, sum[:12], path)

	// 先把结果回给浏览器，再换进程：exec 成功不返回，晚一点执行才能把响应发出去。
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "version": latest, "tag": rel.TagName, "path": path,
		"sha256": sum, "verified": asset.Digest != "", "size": n,
		"restart": "exec",
	})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	execSelf := p.cfg.ExecSelf
	if execSelf == nil {
		execSelf = selfupdate.Exec
	}
	go func() {
		time.Sleep(400 * time.Millisecond) // 让响应落地
		if err := execSelf(path, os.Args[1:]); err != nil {
			log.Printf("面板: 热更新已下载但无法原地重启（%v）；下次启动会自动优先新版本", err)
		}
	}()
}

// selfUpdateCfg 取生效配置（归一化后的副本）。
func (p *Panel) selfUpdateCfg() SelfUpdateConfig {
	return normalizeSelfUpdate(p.cfg.SelfUpdate)
}

// fetchLatestRelease 查 GitHub 最新正式 Release（不是预发布）。
func (p *Panel) fetchLatestRelease(cfg SelfUpdateConfig) (*releaseInfo, error) {
	u := cfg.APIBase + "/repos/" + cfg.Repo + "/releases/latest"
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	resp, err := p.selfUpdateHTTP(20 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// 私有仓库在未鉴权时对 /releases/latest 也回 404（不是「没有 Release」），
		// 两种原因分开说，否则会让人一直去查标签。
		if cfg.Token == "" {
			return nil, fmt.Errorf("GitHub 404：仓库 %s 不存在、为私有，或没有 Release。私有仓库需要在 panel.self_update.token 配只读 token", cfg.Repo)
		}
		return nil, fmt.Errorf("仓库 %s 没有 Release（先打 v* 标签发布）", cfg.Repo)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("GitHub API %d：token 无效或权限不足（需要 repo 读权限）", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API %d", resp.StatusCode)
	}
	var rel releaseInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return nil, err
	}
	if rel.TagName == "" {
		return nil, errors.New("Release 缺少 tag_name")
	}
	return &rel, nil
}

// pickReleaseAsset 按当前平台挑资产：wb2api-panel-<version>-<goos>-<goarch>.zip。
// 版本段可能和 tag 不完全一致（历史包名），所以按「-goos-goarch.zip 结尾」匹配，
// 命中多个时取版本号最大的。
func pickReleaseAsset(rel *releaseInfo, goos, goarch string) (*releaseAsset, string) {
	suffix := "-" + goos + "-" + goarch + ".zip"
	type candidate struct {
		idx  int
		ver  string
		name string
	}
	var cands []candidate
	for i, a := range rel.Assets {
		if !strings.HasPrefix(a.Name, "wb2api-panel-") || !strings.HasSuffix(a.Name, suffix) {
			continue
		}
		ver := strings.TrimSuffix(strings.TrimPrefix(a.Name, "wb2api-panel-"), suffix)
		cands = append(cands, candidate{idx: i, ver: ver, name: a.Name})
	}
	if len(cands) == 0 {
		var names []string
		for _, a := range rel.Assets {
			names = append(names, a.Name)
		}
		sort.Strings(names)
		return nil, "Release 里没有 " + goos + "/" + goarch + " 的发布包（资产：" + strings.Join(names, ", ") + "）"
	}
	sort.Slice(cands, func(i, j int) bool {
		return selfupdate.CompareVersion(cands[i].ver, cands[j].ver) > 0
	})
	a := rel.Assets[cands[0].idx]
	return &a, ""
}

// selfUpdateHTTP 热更新用的 HTTP 客户端（测试注入 TLS 客户端用；nil 走默认）。
func (p *Panel) selfUpdateHTTP(timeout time.Duration) *http.Client {
	if p.cfg.HTTPClient != nil {
		return p.cfg.HTTPClient
	}
	return &http.Client{Timeout: timeout}
}

// downloadTo 下载 URL 到 w，带大小上限与超时。
func (p *Panel) downloadTo(w io.Writer, rawURL, token string) (int64, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	// API 资产端点要这个 Accept 才会回二进制；网页下载地址忽略它也无害。
	req.Header.Set("Accept", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := p.selfUpdateHTTP(5 * time.Minute).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > selfUpdateMaxBytes {
		return 0, fmt.Errorf("发布包过大：%d 字节", resp.ContentLength)
	}
	return io.Copy(w, io.LimitReader(resp.Body, selfUpdateMaxBytes))
}

// openZipBinary 从发布包 zip 里取可执行文件。包内布局固定：wb2api 与
// config.example.json / README.md 平铺；Windows 包里是 wb2api.exe。
func openZipBinary(path string) (io.Reader, func() error, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, func() error { return nil }, err
	}
	want := "wb2api"
	if runtime.GOOS == "windows" {
		want = "wb2api.exe"
	}
	for _, f := range zr.File {
		name := strings.TrimPrefix(strings.ReplaceAll(f.Name, "\\", "/"), "./")
		if name != want {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			zr.Close()
			return nil, func() error { return nil }, err
		}
		return bufio.NewReader(rc), func() error { rc.Close(); return zr.Close() }, nil
	}
	zr.Close()
	return nil, func() error { return nil }, fmt.Errorf("发布包里没有 %s", want)
}
