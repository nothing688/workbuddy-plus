package selfupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompareVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2026.9.26.711", "2026.10.25.1830", -1}, // 不补零，字典序会判反
		{"2026.10.25.1830", "2026.10.25.1830", 0},
		{"2026.10.25.1831", "2026.10.25.1830", 1},
		{"dev", "2026.1.1.0", -1},
		{"", "2026.1.1.0", -1},
		{"v2026.10.25.1830", "2026.10.25.1830", 0}, // tag 前缀不影响
		{"2026.10.25.1830", "2026.10.25.900", 1},
	}
	for _, c := range cases {
		if got := CompareVersion(c.a, c.b); got != c.want {
			t.Errorf("CompareVersion(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

// TestInstallAndPrefer 安装后 Current 能读回；只有版本更高才在启动时优先。
func TestInstallAndPrefer(t *testing.T) {
	dir := t.TempDir()
	path, err := Install(dir, "2026.10.25.1900", "v2026.10.25.1900", "wb2api-panel-2026.10.25.1900-linux-amd64.zip", strings.NewReader("BINARY"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "BINARY" {
		t.Fatalf("落盘内容不对: %q err=%v", raw, err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("新二进制必须可执行: mode=%v err=%v", fi.Mode(), err)
	}
	st, err := Current(dir)
	if err != nil || st == nil || st.Version != "2026.10.25.1900" || st.Tag != "v2026.10.25.1900" {
		t.Fatalf("current=%+v err=%v", st, err)
	}

	if p, _ := PreferPath(dir, "2026.10.24.1000"); p != path {
		t.Fatalf("旧版本应优先热更新: %q want %q", p, path)
	}
	if p, _ := PreferPath(dir, "2026.10.25.1900"); p != "" {
		t.Fatalf("同版本不该再 exec（会死循环）: %q", p)
	}
	if p, _ := PreferPath(dir, "2026.10.26.1000"); p != "" {
		t.Fatalf("镜像版本更高时应回落镜像: %q", p)
	}
}

// TestInstallPruneOld 历史二进制只留最新若干份（数据卷不会被撑爆）。
func TestInstallPruneOld(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"2026.10.20.1000", "2026.10.21.1000", "2026.10.22.1000"} {
		if _, err := Install(dir, v, "v"+v, "a.zip", strings.NewReader("B")); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(Dir(dir))
	var bins []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "wb2api-") {
			bins = append(bins, e.Name())
		}
	}
	if len(bins) != 2 {
		t.Fatalf("应只保留 2 份，实际 %v", bins)
	}
	if _, err := os.Stat(BinaryPath(dir, "2026.10.22.1000")); err != nil {
		t.Fatalf("最新这份不该被删: %v", err)
	}
}

// TestCurrentMissing 没有 current.json 时返回 nil 而不是报错。
func TestCurrentMissing(t *testing.T) {
	st, err := Current(t.TempDir())
	if err != nil || st != nil {
		t.Fatalf("st=%+v err=%v", st, err)
	}
	if p, _ := PreferPath(filepath.Join(t.TempDir(), "nope"), "2026.1.1.1"); p != "" {
		t.Fatalf("空目录不该返回路径: %q", p)
	}
}
