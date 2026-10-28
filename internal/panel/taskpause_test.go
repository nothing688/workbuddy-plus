package panel

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
)

// 暂停态要能落盘并读回：重启后不能悄悄自己恢复（那等于额度又被夜里跑掉）。
func TestTaskPausePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-pause.json")
	if err := newTaskPause(path).Set(true); err != nil {
		t.Fatal(err)
	}
	if !newTaskPause(path).Paused() {
		t.Fatal("重载后应为暂停态")
	}
	if err := newTaskPause(path).Set(false); err != nil {
		t.Fatal(err)
	}
	if newTaskPause(path).Paused() {
		t.Fatal("重载后应为未暂停")
	}
}

// 暂停期间成长队列不启动（这是会花积分的那条路）。
func TestGrowthQueueSkippedWhenPaused(t *testing.T) {
	p := New(Config{})
	if err := p.taskPause.Set(true); err != nil {
		t.Fatal(err)
	}
	n, msg := p.RunGrowthQueueNow()
	if n != 0 || !strings.Contains(msg, "已暂停") {
		t.Fatalf("RunGrowthQueueNow = (%d, %q)，应为暂停跳过", n, msg)
	}
}

// 暂停只挡自动触发：daily 被拒，manual 仍能走到下一步（此处因未启动作业管理器而报
// errTaskJobsNotStarted，恰好证明它没被暂停闸门拦下）。
func TestTaskJobPausedOnlyBlocksAutomaticTriggers(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u-pause", AccessToken: "tok", RefreshToken: "rt"})
	pn := New(Config{Pool: p, AutoTasksEnabled: func() bool { return true }})
	if err := pn.taskPause.Set(true); err != nil {
		t.Fatal(err)
	}

	if _, _, err := pn.StartAccountTaskJob("u-pause", "daily"); err != errTaskJobPaused {
		t.Fatalf("daily err = %v，应为 errTaskJobPaused", err)
	}
	if _, _, err := pn.StartAccountTaskJob("u-pause", "new_account"); err != errTaskJobPaused {
		t.Fatalf("new_account err = %v，应为 errTaskJobPaused", err)
	}
	if _, _, err := pn.StartAccountTaskJob("u-pause", "manual"); err == errTaskJobPaused {
		t.Fatal("manual 不该被暂停闸门挡住")
	}
}

// 领奖连续失败进入退避：第 4 轮不再对同一任务发领奖请求。
func TestSweepClaimFailureBacksOff(t *testing.T) {
	f := &sweepFake{tasks: []*sweepTask{
		{Code: "chat_5", Current: 5, Target: 5, Accepted: true, FailClaim: true},
	}}
	p := sweepPanel(t, f, &auth.Auth{UID: "u-sweep", Nickname: "n", AccessToken: "tok", RefreshToken: "rt"})

	for i := 0; i < sweepClaimFailLimit; i++ {
		if _, msg := p.RunTaskSweepNow(); msg == "" {
			t.Fatalf("第 %d 轮应有摘要", i+1)
		}
	}
	if got := len(f.claims); got != sweepClaimFailLimit {
		t.Fatalf("claim 次数 = %d, want %d", got, sweepClaimFailLimit)
	}

	p.RunTaskSweepNow()
	if got := len(f.claims); got != sweepClaimFailLimit {
		t.Fatalf("退避期间仍发了领奖请求：claim 次数 = %d", got)
	}
}

// 上一轮没结束时不并发起第二轮（定时器 + 手动按钮重叠的去重）。
func TestSweepRejectsConcurrentRun(t *testing.T) {
	f := &sweepFake{tasks: []*sweepTask{{Code: "chat_5", Current: 5, Target: 5, Accepted: true}}}
	p := sweepPanel(t, f, &auth.Auth{UID: "u-busy", Nickname: "n", AccessToken: "tok", RefreshToken: "rt"})

	p.sweeping.Store(true) // 模拟上一轮仍在跑
	defer p.sweeping.Store(false)

	n, msg := p.RunTaskSweepNow()
	if n != 0 || !strings.Contains(msg, "尚未结束") {
		t.Fatalf("RunTaskSweepNow = (%d, %q)，应跳过", n, msg)
	}
	if len(f.claims) != 0 {
		t.Fatalf("跳过的轮次不该发请求：claims = %d", len(f.claims))
	}
}

// 分组 / 单号名单：落盘往返 + 三档合并判据（全局 > 分组 > 单号，任一命中即暂停）。
func TestTaskPauseScopeAndGate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-pause.json")
	tp := newTaskPause(path)
	if err := tp.SetScope([]string{"u-1"}, []string{"组A"}, true); err != nil {
		t.Fatal(err)
	}
	reloaded := newTaskPause(path)
	if !reloaded.AccountPaused("u-1") || !reloaded.GroupPaused("组A") {
		t.Fatal("名单未落盘")
	}

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u-1", Nickname: "a", AccessToken: "t1"})
	p.Add(&auth.Auth{UID: "u-2", Nickname: "b", AccessToken: "t2"})
	p.SetMeta("u-2", "组A", "")
	pn := New(Config{Pool: p})
	pn.taskPause = reloaded

	if !pn.AccountTasksPaused("u-1") {
		t.Error("单号名单应命中")
	}
	if !pn.AccountTasksPaused("u-2") {
		t.Error("分组名单应命中所属账号")
	}
	if pn.AccountTasksPaused("u-3") {
		t.Error("未命中任何一档不该暂停")
	}

	// 恢复后名单移出，判据立刻失效。
	if err := reloaded.SetScope([]string{"u-1"}, []string{"组A"}, false); err != nil {
		t.Fatal(err)
	}
	if pn.AccountTasksPaused("u-1") || pn.AccountTasksPaused("u-2") {
		t.Error("恢复后仍在暂停")
	}

	// 全局暂停覆盖所有账号。
	if err := reloaded.Set(true); err != nil {
		t.Fatal(err)
	}
	if !pn.AccountTasksPaused("u-3") {
		t.Error("全局暂停应对未列名单的账号生效")
	}
}

// 批量接口：POST /panel/api/tasks/pause/batch 要落状态并在 overview 透出名单。
func TestTaskPauseBatchEndpoint(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u-1", Nickname: "a", AccessToken: "t1"})
	pn := New(Config{Version: "test", Pool: p})

	rec := httptest.NewRecorder()
	body := `{"accounts":["u-1"],"groups":["组A"],"paused":true}`
	req := httptest.NewRequest(http.MethodPost, "/panel/api/tasks/pause/batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch code=%d body=%s", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	pn.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("overview code=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"tasks_paused_accounts":["u-1"]`) ||
		!strings.Contains(rec.Body.String(), `"tasks_paused_groups":["组A"]`) {
		t.Fatalf("overview 未透出暂停名单: %s", rec.Body.String())
	}
}
