package panel

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// 连续两轮「动作跑完、进度没动」后，自动队列第三次直接跳过，不再执行动作。
func TestTaskGuardTripsAfterRepeatedNoProgress(t *testing.T) {
	p, a, _ := newAutoTaskTestPanel(t, 0, 5, "accepted")
	actions := 0
	act := &autoAction{TaskCode: "chat_5", run: func(*Panel, *auth.Auth) (string, error) {
		actions++
		return "action returned 200", nil
	}}

	for i := 1; i <= maxStuckRuns; i++ {
		out := p.executeAutoTaskMode(a, act, true)
		if out.Status != "awaiting_progress" {
			t.Fatalf("第 %d 轮 status = %q, want awaiting_progress", i, out.Status)
		}
	}
	if actions != maxStuckRuns {
		t.Fatalf("action calls = %d, want %d", actions, maxStuckRuns)
	}

	out := p.executeAutoTaskMode(a, act, true)
	if out.Status != "skipped" || !strings.Contains(out.Message, "已暂停自动重跑") {
		t.Fatalf("熔断后 outcome = %#v, want skipped + 暂停提示", out)
	}
	if actions != maxStuckRuns {
		t.Fatalf("熔断后仍执行了动作：actions = %d", actions)
	}

	// 手动执行不受熔断影响：用户点了就该跑。
	out = p.executeAutoTask(a, act)
	if out.Status != "awaiting_progress" {
		t.Fatalf("手动 status = %q, want awaiting_progress（手动不该被熔断）", out.Status)
	}
	if actions != maxStuckRuns+1 {
		t.Fatalf("手动 action calls = %d, want %d", actions, maxStuckRuns+1)
	}
}

// 进度快照一变（服务端补计分 / 手动跑成功），熔断自动解除，不需要人工点恢复。
func TestTaskGuardLiftsWhenProgressMoves(t *testing.T) {
	p, a, state := newAutoTaskTestPanel(t, 0, 5, "accepted")
	actions := 0
	act := &autoAction{TaskCode: "chat_5", run: func(*Panel, *auth.Auth) (string, error) {
		actions++
		return "ok", nil
	}}
	for i := 0; i < maxStuckRuns; i++ {
		p.executeAutoTaskMode(a, act, true)
	}
	if !p.taskGuard.blocked(a.UID, act.TaskCode, "0/5") {
		t.Fatal("应为熔断态")
	}

	state.mu.Lock()
	state.current = 1
	state.mu.Unlock()

	out := p.executeAutoTaskMode(a, act, true)
	if out.Status != "awaiting_progress" || actions != maxStuckRuns+1 {
		t.Fatalf("进度变化后应重新执行动作: status=%q actions=%d", out.Status, actions)
	}
	if p.taskGuard.blocked(a.UID, act.TaskCode, "1/5") {
		t.Fatal("进度变化后熔断应解除")
	}
}

// 达标领奖会清掉熔断记录：任务已经拿到奖励，不该再留「空转」标记。
func TestTaskGuardClearedOnClaim(t *testing.T) {
	p, a, state := newAutoTaskTestPanel(t, 0, 5, "accepted")
	act := &autoAction{TaskCode: "chat_5", run: func(*Panel, *auth.Auth) (string, error) {
		return "ok", nil
	}}
	for i := 0; i < maxStuckRuns; i++ {
		p.executeAutoTaskMode(a, act, true)
	}

	state.mu.Lock()
	state.current, state.target = 5, 5
	state.mu.Unlock()

	out := p.executeAutoTaskMode(a, act, true)
	if out.Status != "done" || !out.Claimed {
		t.Fatalf("outcome = %#v, want done", out)
	}
	if p.taskGuard.blocked(a.UID, act.TaskCode, "5/5") {
		t.Fatal("领奖后不该再熔断")
	}
}

// 熔断表落盘后可原样读回；clear 也会落盘。
func TestTaskGuardPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task_attempts.json")
	g := newTaskGuard(path)
	g.noteNoProgress("uid-1", "skill_1", "0/1")
	g.noteNoProgress("uid-1", "skill_1", "0/1")
	if !g.blocked("uid-1", "skill_1", "0/1") {
		t.Fatal("内存表未熔断")
	}

	reloaded := newTaskGuard(path)
	if !reloaded.blocked("uid-1", "skill_1", "0/1") {
		t.Fatal("重载后应保持熔断（记录没落盘）")
	}
	if reloaded.blocked("uid-1", "skill_1", "1/1") {
		t.Fatal("进度快照不同不该熔断")
	}

	reloaded.clear("uid-1", "skill_1")
	if again := newTaskGuard(path); again.blocked("uid-1", "skill_1", "0/1") {
		t.Fatal("clear 后重载仍熔断")
	}
}
