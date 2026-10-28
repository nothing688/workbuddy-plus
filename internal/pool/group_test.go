package pool

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// TestSetMeta 覆盖设置、清除与未知 UID 三种情形。
func TestSetMeta(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	// 设置：两端空白被裁掉。
	if !p.SetMeta("u1", "  team-a  ", "  主号，勿动  ") {
		t.Fatal("SetMeta(existing) = false, want true")
	}
	st, _ := p.Status("u1")
	if st.Group != "team-a" || st.Note != "主号，勿动" {
		t.Fatalf("status group/note=%q/%q, want team-a/主号，勿动", st.Group, st.Note)
	}

	// 重复写入同值：账号存在 → true，但不应置 dirty（无变更不落盘）。
	p.Flush() // 清 dirty
	if !p.SetMeta("u1", "team-a", "主号，勿动") {
		t.Fatal("SetMeta(unchanged) = false, want true")
	}
	if p.dirty.Load() {
		t.Error("unchanged SetMeta should not mark dirty")
	}

	// 空串 = 清除。
	if !p.SetMeta("u1", "", "") {
		t.Fatal("SetMeta(clear) = false, want true")
	}
	st, _ = p.Status("u1")
	if st.Group != "" || st.Note != "" {
		t.Fatalf("cleared group/note=%q/%q, want empty", st.Group, st.Note)
	}

	// 未知 UID。
	if p.SetMeta("nope", "g", "n") {
		t.Error("SetMeta(unknown uid) = true, want false")
	}
}

// TestMetaRoundTrip 落盘 → 重载后 group/note 保留（与现有 persist 测试同写法）。
func TestMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	if !p.SetMeta("u1", "team-a", "备号") {
		t.Fatal("SetMeta failed")
	}
	p.Flush()
	p.Close()

	p2 := New(fp)
	defer p2.Close()
	p2.Add(&auth.Auth{UID: "u1"})
	p2.Add(&auth.Auth{UID: "u2"})
	st, ok := p2.Status("u1")
	if !ok || st.Group != "team-a" || st.Note != "备号" {
		t.Fatalf("reloaded u1=%+v ok=%v, want group=team-a note=备号", st, ok)
	}
	// 未设置过的账号仍是未分组（零值，不串味）。
	st, ok = p2.Status("u2")
	if !ok || st.Group != "" || st.Note != "" {
		t.Fatalf("reloaded u2=%+v ok=%v, want empty meta", st, ok)
	}
}

// TestGroupCounts 验证聚合与 healthy 计数（healthy 用账号级口径：禁用/冷却都不算健康）。
// 注意禁用会把账号移入 DisabledGroup（见 group_test.go 的禁用书签测试），故 u2 计入
// 「已禁用」而非 team-a。
func TestGroupCounts(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"}) // team-a，健康
	p.Add(&auth.Auth{UID: "u2"}) // team-a，禁用（仍留在 team-a，只不健康）
	p.Add(&auth.Auth{UID: "u3"}) // team-b，冷却中
	p.Add(&auth.Auth{UID: "u4"}) // 未分组，健康
	p.Add(&auth.Auth{UID: "u5"}) // team-b，健康
	p.SetMeta("u1", "team-a", "")
	p.SetMeta("u2", "team-a", "")
	p.SetMeta("u3", "team-b", "")
	p.SetMeta("u5", "team-b", "")
	p.Disable("u2", "401")
	p.Cooldown("u3", CoolSoft, time.Hour, "429 rate limit")

	got := p.GroupCounts()
	want := []GroupInfo{
		{Name: "team-a", Total: 2, Healthy: 1}, // 禁用号不搬组：总数含它，健康数不含
		{Name: "team-b", Total: 2, Healthy: 1},
		{Name: "", Total: 1, Healthy: 1}, // 未分组排最后
	}
	if len(got) != len(want) {
		t.Fatalf("GroupCounts=%+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("GroupCounts[%d]=%+v, want %+v (all=%+v)", i, got[i], want[i], got)
		}
	}

	// 空池 → 空切片。
	if e := New("").GroupCounts(); len(e) != 0 {
		t.Fatalf("empty pool GroupCounts=%+v, want empty", e)
	}
}

// TestPickExcludingForRealmGroup 覆盖：只在指定组选、组内无可用返回 nil、
// group=="" 与旧函数等价。
func TestPickExcludingForRealmGroup(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "a1"})
	p.Add(&auth.Auth{UID: "a2"})
	p.Add(&auth.Auth{UID: "b1"})
	p.Add(&auth.Auth{UID: "free"}) // 未分组
	p.SetMeta("a1", "team-a", "")
	p.SetMeta("a2", "team-a", "")
	p.SetMeta("b1", "team-b", "")

	// 指定组：只在该组内选。
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		a := p.PickExcludingForRealmGroup(nil, "", "", "team-a")
		if a == nil {
			t.Fatal("PickExcludingForRealmGroup(team-a) returned nil")
		}
		if a.UID != "a1" && a.UID != "a2" {
			t.Fatalf("picked %s outside team-a", a.UID)
		}
		seen[a.UID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("team-a picks=%v, want both a1 and a2", seen)
	}

	// 请求级轮换（tried）在组内继续生效。
	if a := p.PickExcludingForRealmGroup(map[string]bool{"a1": true}, "", "", "team-a"); a == nil || a.UID != "a2" {
		t.Fatalf("tried a1 → %+v, want a2", a)
	}
	// 组内全被 tried → nil（不会跨组兜底）。
	if a := p.PickExcludingForRealmGroup(map[string]bool{"a1": true, "a2": true}, "", "", "team-a"); a != nil {
		t.Fatalf("all team-a tried → %+v, want nil", a)
	}

	// 组内无可用账号（唯一成员禁用）→ nil；组外的健康号不得被选中。
	p.Disable("a1", "401")
	p.Disable("a2", "401")
	if a := p.PickExcludingForRealmGroup(nil, "", "", "team-a"); a != nil {
		t.Fatalf("team-a all disabled → %+v, want nil", a)
	}

	// 不存在的组 → nil。
	if a := p.PickExcludingForRealmGroup(nil, "", "", "team-x"); a != nil {
		t.Fatalf("unknown group → %+v, want nil", a)
	}

	// 组过滤同样作用于全冷却兜底：team-c 唯一号冷却中，team-b 也有冷却号——
	// 不得从 team-b 的冷却号兜底。
	p.Add(&auth.Auth{UID: "c1"})
	p.SetMeta("c1", "team-c", "")
	p.Cooldown("c1", CoolSoft, time.Hour, "429")
	p.Cooldown("b1", CoolSoft, time.Hour, "429")
	if a := p.PickExcludingForRealmGroup(nil, "", "", "team-c"); a == nil || a.UID != "c1" {
		t.Fatalf("team-c cooling fallback=%+v, want c1 (in-group only)", a)
	}
	p.Disable("c1", "401")
	if a := p.PickExcludingForRealmGroup(nil, "", "", "team-c"); a != nil {
		t.Fatalf("team-c no usable account → %+v, want nil (must not fall back to team-b)", a)
	}
}

// TestPickExcludingForRealmGroupEmptyEqualsOld 验证 group=="" 与旧函数行为一致：
// 单账号时同一个号；tried 全排时都返回 nil；多账号时可选集合相同。
func TestPickExcludingForRealmGroupEmptyEqualsOld(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "a1"})
	p.SetMeta("a1", "team-a", "")

	if a := p.PickExcludingForRealmGroup(nil, "", "", ""); a == nil || a.UID != "a1" {
		t.Fatalf("empty group single acct=%+v, want a1", a)
	}
	if a := p.PickExcludingForRealm(nil, "", ""); a == nil || a.UID != "a1" {
		t.Fatalf("old func single acct=%+v, want a1", a)
	}
	if a := p.PickExcludingForRealmGroup(map[string]bool{"a1": true}, "", "", ""); a != nil {
		t.Fatalf("empty group all tried=%+v, want nil", a)
	}
	if a := p.PickExcludingForRealm(map[string]bool{"a1": true}, "", ""); a != nil {
		t.Fatalf("old func all tried=%+v, want nil", a)
	}

	// 多账号：两者都能选出分组/未分组的全部健康号（候选集合等价）。
	p.Add(&auth.Auth{UID: "free"})
	seenNew, seenOld := map[string]bool{}, map[string]bool{}
	for i := 0; i < 100; i++ {
		if a := p.PickExcludingForRealmGroup(nil, "", "", ""); a != nil {
			seenNew[a.UID] = true
		}
		if a := p.PickExcludingForRealm(nil, "", ""); a != nil {
			seenOld[a.UID] = true
		}
	}
	for _, uid := range []string{"a1", "free"} {
		if !seenNew[uid] || !seenOld[uid] {
			t.Fatalf("uid %s not seen: new=%v old=%v", uid, seenNew, seenOld)
		}
	}
}

// ---------------------------------------------------------------------------
// 禁用分组书签：禁用移入 DisabledGroup，恢复回归原组
// ---------------------------------------------------------------------------

// TestDisableParksGroupAndReviveRestores 覆盖主路径：teamA → 禁用 → 已禁用
// （书签记录原组）→ Revive → 回到 teamA 且标记清空。两个恢复入口都要生效。
func TestDisableKeepsGroupAndReviveKeepsIt(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMeta("u1", "teamA", "")

	// 2026-10-28 起：禁用只关业务资格，不改写分组归属（面板改为把禁用号置底）。
	p.Disable("u1", "401")
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("Status(u1) not found")
	}
	if st.Group != "teamA" || st.GroupParked || st.PrevGroup != "" {
		t.Fatalf("after Disable group=%q parked=%v prev=%q, want teamA/false/\"\"",
			st.Group, st.GroupParked, st.PrevGroup)
	}

	// Revive 只恢复资格，分组原样不动。
	if !p.Revive("u1") {
		t.Fatal("Revive(u1) = false, want true")
	}
	st, _ = p.Status("u1")
	if st.Disabled || st.Group != "teamA" {
		t.Fatalf("after Revive disabled=%v group=%q, want false/teamA", st.Disabled, st.Group)
	}

	// ReviveDisabled 同口径。
	p.Disable("u1", "401")
	p.ReviveDisabled("u1")
	st, _ = p.Status("u1")
	if st.Disabled || st.Group != "teamA" {
		t.Fatalf("after ReviveDisabled disabled=%v group=%q, want false/teamA", st.Disabled, st.Group)
	}
}

// TestDisableKeepsUngroupedAccount 未分组账号被禁用后仍是未分组——面板把它排到
// 列表末尾，而不是塞进一个「已禁用」分组。
func TestDisableKeepsUngroupedAccount(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 不调 SetMeta：group 为空串（未分组）。

	p.Disable("u1", "401")
	st, _ := p.Status("u1")
	if st.Group != "" || st.GroupParked {
		t.Fatalf("after Disable group=%q parked=%v, want \"\"/false", st.Group, st.GroupParked)
	}

	p.ReviveDisabled("u1")
	if st, _ = p.Status("u1"); st.Group != "" || st.Disabled {
		t.Fatalf("after Revive group=%q disabled=%v, want \"\"/false", st.Group, st.Disabled)
	}
}

// TestDisableTwiceKeepsGroup 已禁用账号再禁用（只更新 reason）不得改写分组，
// 也不能凭空写书签——现在根本没有「搬组」这回事。
func TestDisableTwiceKeepsGroup(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMeta("u1", "teamA", "")

	p.Disable("u1", "401")
	p.Disable("u1", "manual ban")
	st, _ := p.Status("u1")
	if st.Group != "teamA" || st.GroupParked || st.PrevGroup != "" {
		t.Fatalf("repeat disable group=%q parked=%v prev=%q, want teamA/false/\"\"",
			st.Group, st.GroupParked, st.PrevGroup)
	}
	if st.DisabledReason != "manual ban" {
		t.Fatalf("repeat disable reason=%q, want manual ban", st.DisabledReason)
	}

	p.Revive("u1")
	if st, _ = p.Status("u1"); st.Group != "teamA" {
		t.Fatalf("after Revive group=%q, want teamA", st.Group)
	}
}

// TestDisableThenSetMetaWins 人工移动优先：禁用后手动改到 teamB，恢复时留在 teamB。
// 显式设置成 DisabledGroup 同样按普通分组处理（清书签），恢复也不跳回。
func TestDisableThenSetMetaWins(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetMeta("u1", "teamA", "")
	p.SetMeta("u2", "teamA", "")
	p.Disable("u1", "401")
	p.Disable("u2", "401")

	p.SetMeta("u1", "teamB", "")
	st, _ := p.Status("u1")
	if st.Group != "teamB" || st.GroupParked || st.PrevGroup != "" {
		t.Fatalf("SetMeta(group) → group=%q parked=%v prev=%q, want teamB/false/\"\"", st.Group, st.GroupParked, st.PrevGroup)
	}
	p.Revive("u1")
	if st, _ = p.Status("u1"); st.Group != "teamB" {
		t.Fatalf("after Revive group=%q, want teamB (人工移动优先)", st.Group)
	}

	// 显式设成 DisabledGroup：按普通分组处理，清标记后恢复不跳回 teamA。
	p.SetMeta("u2", DisabledGroup, "")
	st, _ = p.Status("u2")
	if st.GroupParked || st.PrevGroup != "" {
		t.Fatalf("SetMeta(DisabledGroup) parked=%v prev=%q, want false/\"\"", st.GroupParked, st.PrevGroup)
	}
	p.Revive("u2")
	if st, _ = p.Status("u2"); st.Group != DisabledGroup {
		t.Fatalf("after Revive group=%q, want %q (显式禁用分组保持)", st.Group, DisabledGroup)
	}
}

// TestLegacyDisabledGroupMigratesOnLoad 历史 state（禁用号被搬进「已禁用」并留书签）
// 载入时一次性搬回原组，避免分组下拉一直挂着一个没人再写入的「已禁用」组。
func TestLegacyDisabledGroupMigratesOnLoad(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	raw := `{"accounts":{
		"u1":{"credits":10,"disabled":true,"reason":"legacy","group":"` + DisabledGroup + `","prev_group":"teamA","group_parked":true},
		"u2":{"credits":10,"disabled":true,"reason":"legacy","group":"` + DisabledGroup + `","prev_group":"","group_parked":true}
	}}`
	if err := os.WriteFile(fp, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	p := New(fp)
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})

	if st, ok := p.Status("u1"); !ok || st.Group != "teamA" || st.GroupParked {
		t.Fatalf("u1=%+v ok=%v, want group=teamA 且书签清空", st, ok)
	}
	if st, ok := p.Status("u2"); !ok || st.Group != "" || st.GroupParked {
		t.Fatalf("u2=%+v ok=%v, want group=\"\" 且书签清空", st, ok)
	}
	// 迁移不应改动禁用状态本身。
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatal("迁移只搬组，不该把号解禁")
	}
}

// TestSnapshotKeepsOriginalGroup Redis 快照恢复路径（adoptSnapshot 复用
// applyAccountsLocked）保留分组归属：禁用不再搬组，快照里 group 就是 teamA。
func TestSnapshotKeepsOriginalGroup(t *testing.T) {
	// 快照镜像只在有 state.json 路径时随落盘发生（saveLocked 先判 stateFp）。
	p := New(filepath.Join(t.TempDir(), "state.json"))
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMeta("u1", "teamA", "")
	p.Disable("u1", "401")
	ms := &memStore{}
	p.SetStore(ms)
	p.Flush()

	ms.mu.Lock()
	hasSnap := len(ms.saved) > 0
	// memStore 的读、写槽分开：把落盘镜像接到 LoadState 上模拟 Redis 取回。
	ms.loadData = append([]byte(nil), ms.saved...)
	ms.loadOK = hasSnap
	ms.mu.Unlock()
	if !hasSnap {
		t.Fatal("Flush 未写快照")
	}

	// 本地 state.json 不存在 → 采用 Redis 快照（adoptSnapshot 路径）。
	p2 := New(filepath.Join(t.TempDir(), "absent.json"))
	p2.SetStore(ms)
	p2.RestoreFromSnapshot()
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || st.Group != "teamA" || st.GroupParked || st.PrevGroup != "" {
		t.Fatalf("snapshot u1=%+v ok=%v, want teamA/未停车/无书签", st, ok)
	}
	p2.Revive("u1")
	if st, _ := p2.Status("u1"); st.Group != "teamA" {
		t.Fatalf("snapshot Revive group=%q, want teamA", st.Group)
	}
}
