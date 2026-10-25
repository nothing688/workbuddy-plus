package pool

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// TestActiveGroupTrimAndDirty 覆盖默认调用池的 TrimSpace、空串清除与「值不变不置脏」。
func TestActiveGroupTrimAndDirty(t *testing.T) {
	p := New("")
	if got := p.ActiveGroup(); got != "" {
		t.Fatalf("fresh pool ActiveGroup=%q, want empty (全池)", got)
	}

	p.SetActiveGroup("  team-a  ")
	if got := p.ActiveGroup(); got != "team-a" {
		t.Fatalf("ActiveGroup=%q, want trimmed team-a", got)
	}
	if !p.dirty.Load() {
		t.Fatal("SetActiveGroup(new value) should mark dirty")
	}

	// 只有空白差异 → TrimSpace 后同值，不置 dirty（避免无意义落盘）。
	p.Flush()
	p.SetActiveGroup(" team-a ")
	if p.dirty.Load() {
		t.Error("unchanged SetActiveGroup should not mark dirty")
	}
	if got := p.ActiveGroup(); got != "team-a" {
		t.Fatalf("ActiveGroup=%q after same-value set, want team-a", got)
	}

	// 空串 = 清除 → 回到全池。
	p.SetActiveGroup("   ")
	if got := p.ActiveGroup(); got != "" {
		t.Fatalf("ActiveGroup=%q after clear, want empty", got)
	}
	if !p.dirty.Load() {
		t.Error("clearing a non-empty active group is a change → dirty expected")
	}
}

// TestActiveGroupPersistRoundTrip 落盘 → 重载后保留；再次清除后 state.json 不再写该字段。
func TestActiveGroupPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")

	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetActiveGroup("  team-a  ")
	p.Flush()
	p.Close()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read state.json: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"active_group": "team-a"`)) {
		t.Fatalf("state.json missing trimmed active_group: %s", raw)
	}

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if got := p2.ActiveGroup(); got != "team-a" {
		t.Fatalf("reloaded ActiveGroup=%q, want team-a", got)
	}

	// 清除后落盘：omitempty 让字段消失，重启回到空串（全池）。
	p2.SetActiveGroup("")
	p2.Flush()
	raw, err = os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read state.json after clear: %v", err)
	}
	if bytes.Contains(raw, []byte("active_group")) {
		t.Fatalf("cleared active_group must be omitted: %s", raw)
	}
	p2.Close()

	p3 := New(fp)
	defer p3.Close()
	if got := p3.ActiveGroup(); got != "" {
		t.Fatalf("reloaded after clear ActiveGroup=%q, want empty", got)
	}
}
