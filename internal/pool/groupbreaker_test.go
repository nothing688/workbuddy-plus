package pool

import (
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// newBreakerPool 三个号：team-a 的 ga1/ga2，team-b 的 gb1。
func newBreakerPool(t *testing.T) *Pool {
	t.Helper()
	p := New("")
	for _, a := range []*auth.Auth{
		{UID: "ga1", AccessToken: "at-ga1", ExpiresAt: 9999999999},
		{UID: "ga2", AccessToken: "at-ga2", ExpiresAt: 9999999999},
		{UID: "gb1", AccessToken: "at-gb1", ExpiresAt: 9999999999},
	} {
		p.Add(a)
	}
	if !p.SetMeta("ga1", "team-a", "") || !p.SetMeta("ga2", "team-a", "") || !p.SetMeta("gb1", "team-b", "") {
		t.Fatal("SetMeta failed")
	}
	return p
}

// TestGroupBreakerFailsOverWhenGroupAllCooling 组内全组对该模型冷却 → 改道组外，
// 并登记熔断行（计数 / 最早解封 / 原因）。
func TestGroupBreakerFailsOverWhenGroupAllCooling(t *testing.T) {
	p := newBreakerPool(t)
	reset := time.Now().Add(30 * time.Minute)
	p.CooldownSoftForModel("ga1", 600*time.Second, reset, "glm-5.2", "6004 model rate limit")
	p.CooldownSoftForModel("ga2", 600*time.Second, reset, "glm-5.2", "6004 model rate limit")

	acct, from := p.PickExcludingForRealmGroupWithFailover(nil, "glm-5.2", "", "team-a")
	if acct == nil {
		t.Fatal("组内全冷却时应改道组外，实际 nil")
	}
	if acct.UID != "gb1" {
		t.Fatalf("改道选中 %q，want gb1（组外唯一健康号）", acct.UID)
	}
	if from != "team-a" {
		t.Fatalf("from=%q，want team-a（熔断来源组）", from)
	}

	rows := p.GroupBreakers()
	if len(rows) != 1 {
		t.Fatalf("GroupBreakers=%+v，want 1 行", rows)
	}
	got := rows[0]
	if got.Group != "team-a" || got.Model != "glm-5.2" || got.Count != 2 {
		t.Fatalf("breaker row=%+v，want group=team-a model=glm-5.2 count=2", got)
	}
	if got.Until.IsZero() || got.Reason == "" {
		t.Fatalf("breaker row 应带最早解封与原因：%+v", got)
	}
}

// TestGroupBreakerKeepsGroupWhenOneAccountHealthy 组内只要还有一个能服务该模型的号，
// 就不熔断（即便另一个号在冷却）。
func TestGroupBreakerKeepsGroupWhenOneAccountHealthy(t *testing.T) {
	p := newBreakerPool(t)
	reset := time.Now().Add(30 * time.Minute)
	p.CooldownSoftForModel("ga1", 600*time.Second, reset, "glm-5.2", "6004 model rate limit")

	acct, from := p.PickExcludingForRealmGroupWithFailover(nil, "glm-5.2", "", "team-a")
	if acct == nil || acct.UID != "ga2" {
		t.Fatalf("应选中组内健康号 ga2，实际 %+v from=%q", acct, from)
	}
	if from != "" {
		t.Fatalf("组内仍有健康号，不应触发熔断，from=%q", from)
	}
	if rows := p.GroupBreakers(); len(rows) != 0 {
		t.Fatalf("不应有熔断行：%+v", rows)
	}
}

// TestGroupBreakerDisabledSwitch 关闭开关后退回纯组内选号：不改道、不登记台账；
// 重新打开后恢复改道能力。
func TestGroupBreakerDisabledSwitch(t *testing.T) {
	p := newBreakerPool(t)
	reset := time.Now().Add(30 * time.Minute)
	p.CooldownSoftForModel("ga1", 600*time.Second, reset, "glm-5.2", "6004")
	p.CooldownSoftForModel("ga2", 600*time.Second, reset, "glm-5.2", "6004")

	acct, from := p.PickExcludingForRealmGroupWithFailover(nil, "glm-5.2", "", "team-a")
	if acct == nil || from != "team-a" {
		t.Fatalf("前置：默认应改道，acct=%+v from=%q", acct, from)
	}
	if len(p.GroupBreakers()) != 1 {
		t.Fatal("前置：应有 1 条熔断台账")
	}

	p.SetGroupBreaker(false)
	if acct, from := p.PickExcludingForRealmGroupWithFailover(nil, "glm-5.2", "", "team-a"); acct != nil || from != "" {
		t.Fatalf("关闭后不应改道：acct=%+v from=%q", acct, from)
	}
	if rows := p.GroupBreakers(); len(rows) != 0 {
		t.Fatalf("关闭后熔断台账应清空：%+v", rows)
	}
	if p.GroupBrokenFor("team-a", "glm-5.2", "") {
		t.Fatal("关闭后 GroupBrokenFor 应为 false")
	}

	p.SetGroupBreaker(true)
	if acct, from := p.PickExcludingForRealmGroupWithFailover(nil, "glm-5.2", "", "team-a"); acct == nil || acct.UID != "gb1" || from != "team-a" {
		t.Fatalf("重新打开应恢复改道：acct=%+v from=%q", acct, from)
	}
}

// TestGroupBreakerModelScoped 熔断按模型独立：组内只是对 glm-5.2 全冷，
// 换 hy3 请求仍应落在本组。
func TestGroupBreakerModelScoped(t *testing.T) {
	p := newBreakerPool(t)
	reset := time.Now().Add(30 * time.Minute)
	p.CooldownSoftForModel("ga1", 600*time.Second, reset, "glm-5.2", "6004")
	p.CooldownSoftForModel("ga2", 600*time.Second, reset, "glm-5.2", "6004")

	acct, from := p.PickExcludingForRealmGroupWithFailover(nil, "hy3", "", "team-a")
	if acct == nil {
		t.Fatal("另一个模型不应受该组熔断影响")
	}
	if acct.UID != "ga1" && acct.UID != "ga2" {
		t.Fatalf("hy3 应留在 team-a，实际 %q", acct.UID)
	}
	if from != "" {
		t.Fatalf("未指定模型冷却时不应改道，from=%q", from)
	}
}

// TestGroupBreakerEmptyGroupNoFailover 组不存在 / 空组不熔断，保持 no_account_in_group
// 语义（拼错组名不该被组外账号兜底掩盖）。
func TestGroupBreakerEmptyGroupNoFailover(t *testing.T) {
	p := newBreakerPool(t)
	acct, from := p.PickExcludingForRealmGroupWithFailover(nil, "glm-5.2", "", "no-such-group")
	if acct != nil || from != "" {
		t.Fatalf("空组应返回 nil 且不熔断，实际 acct=%+v from=%q", acct, from)
	}
}

// TestGroupBreakerNoAlternativeKeepsGroupResult 组外没有可用号时，仍按组内原逻辑
// 选号（模型级全冷时原本就返回 nil），只是把「已熔断」的事实通过 from 透出。
func TestGroupBreakerNoAlternativeKeepsGroupResult(t *testing.T) {
	p := New("")
	for _, a := range []*auth.Auth{
		{UID: "ga1", AccessToken: "at-ga1", ExpiresAt: 9999999999},
		{UID: "ga2", AccessToken: "at-ga2", ExpiresAt: 9999999999},
	} {
		p.Add(a)
		p.SetMeta(a.UID, "team-a", "")
	}
	reset := time.Now().Add(30 * time.Minute)
	p.CooldownSoftForModel("ga1", 600*time.Second, reset, "glm-5.2", "6004")
	p.CooldownSoftForModel("ga2", 600*time.Second, time.Now().Add(time.Hour), "glm-5.2", "6004")

	acct, from := p.PickExcludingForRealmGroupWithFailover(nil, "glm-5.2", "", "team-a")
	if acct != nil {
		t.Fatalf("组外无可用号且组内模型级全冷，应保持 nil，实际 %q", acct.UID)
	}
	if from != "team-a" {
		t.Fatalf("from=%q，want team-a（已判定熔断但无备选）", from)
	}
}

// TestGroupBreakerClosesAfterRecovery 组内账号恢复可服务后，熔断自动闭合：
// 选号回到本组、GroupBreakers 清空。
func TestGroupBreakerClosesAfterRecovery(t *testing.T) {
	p := newBreakerPool(t)
	reset := time.Now().Add(30 * time.Minute)
	p.CooldownSoftForModel("ga1", 600*time.Second, reset, "glm-5.2", "6004")
	p.CooldownSoftForModel("ga2", 600*time.Second, reset, "glm-5.2", "6004")

	if acct, from := p.PickExcludingForRealmGroupWithFailover(nil, "glm-5.2", "", "team-a"); acct == nil || from != "team-a" {
		t.Fatalf("前置：应先熔断改道，acct=%+v from=%q", acct, from)
	}
	// 模拟上游提前恢复（冷却条目消失）。
	p.mu.Lock()
	delete(p.byUID["ga1"].modelCooldowns, "glm-5.2")
	p.mu.Unlock()

	acct, from := p.PickExcludingForRealmGroupWithFailover(nil, "glm-5.2", "", "team-a")
	if acct == nil || (acct.UID != "ga1" && acct.UID != "ga2") {
		t.Fatalf("恢复后应回到 team-a 选号，实际 %+v", acct)
	}
	if from != "" {
		t.Fatalf("恢复后不应再熔断，from=%q", from)
	}
	if rows := p.GroupBreakers(); len(rows) != 0 {
		t.Fatalf("恢复后熔断行应清空：%+v", rows)
	}
}

// TestGroupBreakerDisabledGroupNotSpilled 全禁用的组不触发改道：禁用是运维显式意图，
// 不能因为组不可用就把流量倒给别的组。
func TestGroupBreakerDisabledGroupNotSpilled(t *testing.T) {
	p := newBreakerPool(t)
	p.Disable("ga1", "maintenance")
	p.Disable("ga2", "maintenance")
	acct, from := p.PickExcludingForRealmGroupWithFailover(nil, "glm-5.2", "", "team-a")
	if acct != nil || from != "" {
		t.Fatalf("全禁用组不应改道，实际 acct=%+v from=%q", acct, from)
	}
}
