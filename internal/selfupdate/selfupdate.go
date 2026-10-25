// Package selfupdate 单文件二进制的「热更新」：下载新版本、落盘、校验、替换并重启。
//
// 为什么需要：本仓发布物就是一个自包含二进制（前端 embed 在里面），但生产跑在容器里，
// 镜像由 CI 构建、watchtower 拉取。想只更新一个版本而不等镜像重建，就得让进程自己
// 换掉自己。这里的约束来自镜像：/app 属 root、进程以低权限用户运行，**不能原地覆盖
// /app/wb2api**，所以新版本一律落在可写的数据卷里（与 state.json 同目录），再由
// 启动逻辑优先运行它。
//
// 三层保障，缺一不可：
//  1. 安装是原子的（临时文件 + fsync + rename），不会留下半个二进制；
//  2. 版本号写进 current.json，启动时只有「比当前进程新」才优先运行，避免容器重建后
//     用回旧的热更新版本，也避免 exec 死循环；
//  3. 旧版本按数量清理，数据卷不会被历史二进制撑满。
package selfupdate

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// stateFile 记录「当前优先运行的热更新版本」。放数据卷里，容器重建也还在。
const stateFile = "current.json"

// State 热更新版本记录。
type State struct {
	Version     string    `json:"version"`      // 版本号，与 appVersion 同口径（如 2026.10.25.1830）
	Tag         string    `json:"tag"`          // 来源 Release 的 tag（如 v2026.10.25.1830）
	Asset       string    `json:"asset"`        // 来源资产名，便于排查
	InstalledAt time.Time `json:"installed_at"` // 安装时刻
}

// Dir 热更新目录：与 state.json 同级，保证在数据卷里（容器重建不丢）。
func Dir(dataDir string) string { return filepath.Join(dataDir, "updates") }

// Current 读取当前热更新版本；没有记录时返回 nil。
func Current(dataDir string) (*State, error) {
	raw, err := os.ReadFile(filepath.Join(Dir(dataDir), stateFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("current.json 损坏: %w", err)
	}
	if strings.TrimSpace(st.Version) == "" {
		return nil, nil
	}
	return &st, nil
}

// BinaryPath 某个版本二进制的落盘路径。
func BinaryPath(dataDir, version string) string {
	name := "wb2api-" + safeVersion(version)
	if IsWindows() {
		name += ".exe"
	}
	return filepath.Join(Dir(dataDir), name)
}

// PreferPath 判断启动时是否该改跑热更新版本：
// 只有装过、文件在、且版本**严格高于**当前进程版本才返回路径。
// 用严格大于而不是「不等于」：容器重建后镜像里的版本一旦追平/超过热更新版本，
// 就自动回到镜像版本，不会永远卡在旧的热更新上。
func PreferPath(dataDir, currentVersion string) (string, *State) {
	st, err := Current(dataDir)
	if err != nil || st == nil {
		return "", nil
	}
	if CompareVersion(st.Version, currentVersion) <= 0 {
		return "", nil
	}
	p := BinaryPath(dataDir, st.Version)
	if fi, err := os.Stat(p); err != nil || fi.IsDir() {
		return "", nil
	}
	return p, st
}

// Install 把读到的二进制原子落盘，并更新 current.json。返回落盘路径。
// r 必须是已经校验过的内容（调用方负责 digest / 签名）。
func Install(dataDir, version, tag, asset string, r io.Reader) (string, error) {
	if strings.TrimSpace(version) == "" {
		return "", fmt.Errorf("version required")
	}
	dir := Dir(dataDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := BinaryPath(dataDir, version)
	tmp, err := os.CreateTemp(dir, ".wb2api-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后这里删不到东西，失败时兜底清理
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return "", err
	}
	// Windows 上目标文件被占用时 rename 会失败，这时直接把错误抛给调用方
	// （面板会提示「已下载，重启后生效」——启动逻辑同样会优先它）。
	if err := os.Rename(tmpName, dst); err != nil {
		return "", err
	}
	st := State{Version: version, Tag: tag, Asset: asset, InstalledAt: time.Now()}
	raw, _ := json.MarshalIndent(st, "", "  ")
	stateTmp := filepath.Join(dir, stateFile+".tmp")
	if err := os.WriteFile(stateTmp, raw, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(stateTmp, filepath.Join(dir, stateFile)); err != nil {
		return "", err
	}
	PruneOld(dataDir, version, 2)
	return dst, nil
}

// PruneOld 只保留最新 keep 份二进制（含刚才安装的那份），其余删掉。
// 数据卷是持久化的，长期热更新不清理会攒一堆几十 MB 的历史二进制。
func PruneOld(dataDir, keepVersion string, keep int) {
	if keep < 1 {
		keep = 1
	}
	dir := Dir(dataDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type item struct {
		name, version string
	}
	var bins []item
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, "wb2api-") || strings.HasPrefix(n, ".") || strings.HasSuffix(n, ".tmp") {
			continue
		}
		v := strings.TrimSuffix(strings.TrimPrefix(n, "wb2api-"), ".exe")
		if v == keepVersion {
			continue // 正在用的这份不动
		}
		bins = append(bins, item{name: n, version: v})
	}
	sort.Slice(bins, func(i, j int) bool { return CompareVersion(bins[i].version, bins[j].version) > 0 })
	for i, b := range bins {
		if i >= keep-1 {
			os.Remove(filepath.Join(dir, b.name))
		}
	}
}

// CompareVersion 比较两个版本号（日期式 2026.10.25.1830，不补零）。
// 逐段按数字比，"dev" / 空串视为最小。返回 -1/0/1。
// 不能直接用字符串比：不补零时 "2026.9.26" > "2026.10.25"（字典序），恰好是反的。
func CompareVersion(a, b string) int {
	as, bs := versionParts(a), versionParts(b)
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func versionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" || v == "dev" {
		return nil
	}
	// 时间戳后缀（如 2026.10.25.1830-1-gabc）只取数字段，遇到非数字段停止。
	var out []int
	for _, seg := range strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '-' || r == '+' }) {
		n, err := strconv.Atoi(seg)
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}

// safeVersion 把版本号收敛成文件名安全的形式（只留字母数字与点划线）。
func safeVersion(v string) string {
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}
