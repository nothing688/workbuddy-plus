package scheduler

import (
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/cst"
)

// 面板「暂停任务」必须挡住夜猫子：它是整条排程里唯一纯真实对话的任务，
// 暂停时一个上游请求都不该发（否则「只留签到回血」的意图会被夜聊抵消）。
func TestBlackcatSkippedWhenPaused(t *testing.T) {
	restoreClock := nowForNightWindow
	nowForNightWindow = func() time.Time { return time.Date(2026, 10, 7, 23, 30, 0, 0, cst.Loc) }
	t.Cleanup(func() { nowForNightWindow = restoreClock })

	s, r := enterpriseTestScheduler(t, false)
	s.SetPausedFunc(func(string) bool { return true })
	s.RunBlackcatNow()
	if r.has("/v2/activity/growth/tasks") {
		t.Fatalf("暂停后仍发了上游请求：%v", r.snapshot())
	}

	// 对照：未暂停时必须真的问一次「还差几次」，证明上面的空请求是闸门而不是构造问题。
	control, cr := enterpriseTestScheduler(t, false)
	control.RunBlackcatNow()
	if !cr.has("/v2/activity/growth/tasks") {
		t.Fatalf("对照组的个人号应发起夜猫子查询，实际请求=%v", cr.snapshot())
	}
}
