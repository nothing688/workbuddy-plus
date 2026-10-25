package pool

import "testing"

// TestCreateGroup 手工创建空分组：出现在 GroupCounts 里（0/0），空名拒绝。
func TestCreateGroup(t *testing.T) {
	p := New("")
	t.Cleanup(p.Close)

	if !p.CreateGroup("  teamX  ") {
		t.Fatal("CreateGroup(teamX) = false")
	}
	if !p.CreateGroup("teamX") {
		t.Fatal("重复创建应当幂等成功")
	}
	if p.CreateGroup("   ") {
		t.Fatal("空名应当被拒绝")
	}
	found := false
	for _, g := range p.GroupCounts() {
		if g.Name == "teamX" {
			found = true
			if g.Total != 0 || g.Healthy != 0 {
				t.Fatalf("空分组应 0/0，实际 %d/%d", g.Total, g.Healthy)
			}
		}
	}
	if !found {
		t.Fatalf("GroupCounts 不含 teamX: %+v", p.GroupCounts())
	}
}
