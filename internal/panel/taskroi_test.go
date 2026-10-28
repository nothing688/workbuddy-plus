package panel

import (
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// 计费动作的测试替身：run 非 nil（否则会被当成零成本动作），Chats 标明对话次数。
func billedAction(code string, chats int) autoAction {
	return autoAction{TaskCode: code, Desc: "test " + code, Chats: chats, run: func(*Panel, *auth.Auth) (string, error) { return "", nil }}
}

// 零成本动作替身（纯遥测上报）。
func freeAction(code string) autoAction {
	return autoAction{TaskCode: code, Desc: "free " + code, run: func(*Panel, *auth.Auth) (string, error) { return "", nil }}
}

func taskOf(code string, credit int64) *upstream.Task {
	return &upstream.Task{TaskCode: code, Credit: credit, RewardKnown: credit > 0}
}

func TestTaskROINormalize(t *testing.T) {
	// 零值 = 缺省档：roi 模式 + 兜底单次对话成本，老 config 没有这些键时行为就是它。
	z := TaskROI{}.Normalize()
	if z.Mode != TaskModeROI {
		t.Fatalf("零值 Mode = %q，期望 %q", z.Mode, TaskModeROI)
	}
	if z.UnitChatCost != defaultUnitChatCost {
		t.Fatalf("零值 UnitChatCost = %d，期望 %d", z.UnitChatCost, defaultUnitChatCost)
	}
	if got := (TaskROI{Mode: "ALL"}).Normalize().Mode; got != TaskModeAll {
		t.Fatalf("大写模式未归一化：%q", got)
	}
	if got := (TaskROI{Mode: "bogus"}).Normalize().Mode; got != TaskModeROI {
		t.Fatalf("非法模式未回退 roi：%q", got)
	}
	if got := (TaskROI{MinNetCredit: -5}).Normalize().MinNetCredit; got != 0 {
		t.Fatalf("负门槛未归零：%d", got)
	}
}

func TestBuildTaskPlanFreeModeSkipsBilled(t *testing.T) {
	tasks := map[string]*upstream.Task{
		"chat_5":   taskOf("chat_5", 100),
		"expert_5": taskOf("expert_5", 500),
	}
	acts := []autoAction{freeAction("chat_5"), billedAction("expert_5", 5)}
	plan := buildTaskPlan(tasks, acts, TaskROI{Mode: TaskModeFree, UnitChatCost: 30},
		func(c string, chats int) (int64, string) { return 30 * int64(chats), "estimate" }, 0)

	if plan.RunCount != 1 || plan.SkipCount != 1 {
		t.Fatalf("free 模式 run=%d skip=%d，期望 1/1（%s）", plan.RunCount, plan.SkipCount, plan.summary())
	}
	if plan.ProjectedCost != 0 {
		t.Fatalf("free 模式不该有预估花费，得到 %d", plan.ProjectedCost)
	}
	if item := plan.Items[1]; item.Decision != decisionSkip {
		t.Fatalf("计费动作应跳过，得到 %q（%s）", item.Decision, item.Reason)
	}
}

func TestBuildTaskPlanROIGatesOnNet(t *testing.T) {
	// 单次对话成本 30：奖励 50 净 +20 该做；奖励 20 净 -10 不该做。
	tasks := map[string]*upstream.Task{
		"expert_use_maidehao": taskOf("expert_use_maidehao", 50),
		"Sequential_Tasks_1":  taskOf("Sequential_Tasks_1", 20),
	}
	acts := []autoAction{
		billedAction("expert_use_maidehao", 1),
		billedAction("Sequential_Tasks_1", 1),
	}
	plan := buildTaskPlan(tasks, acts, TaskROI{UnitChatCost: 30},
		func(c string, chats int) (int64, string) { return 30 * int64(chats), "estimate" }, 0)

	if plan.Items[0].Decision != decisionRun {
		t.Fatalf("净 +20 应执行，得到 %q（%s）", plan.Items[0].Decision, plan.Items[0].Reason)
	}
	if plan.Items[0].Net != 20 {
		t.Fatalf("净收益 = %d，期望 20", plan.Items[0].Net)
	}
	if plan.Items[1].Decision != decisionSkip {
		t.Fatalf("净 -10 应跳过，得到 %q（%s）", plan.Items[1].Decision, plan.Items[1].Reason)
	}
	if plan.ProjectedNet != 20 || plan.ProjectedCost != 30 || plan.ProjectedReward != 50 {
		t.Fatalf("预估口径不对：net=%d cost=%d reward=%d",
			plan.ProjectedNet, plan.ProjectedCost, plan.ProjectedReward)
	}
}

func TestBuildTaskPlanUnknownRewardSkips(t *testing.T) {
	// 上游不下发奖励值时无法确认净收益：roi 模式宁可不做（all 模式仍照做）。
	tasks := map[string]*upstream.Task{"mystery": {TaskCode: "mystery"}}
	acts := []autoAction{billedAction("mystery", 1)}
	costOf := func(c string, chats int) (int64, string) { return 30 * int64(chats), "estimate" }

	plan := buildTaskPlan(tasks, acts, TaskROI{}, costOf, 0)
	if plan.Items[0].Decision != decisionSkip {
		t.Fatalf("奖励未知应跳过，得到 %q", plan.Items[0].Decision)
	}
	planAll := buildTaskPlan(tasks, acts, TaskROI{Mode: TaskModeAll}, costOf, 0)
	if planAll.Items[0].Decision != decisionRun {
		t.Fatalf("all 模式应照做，得到 %q", planAll.Items[0].Decision)
	}
}

func TestBuildTaskPlanDailyBudget(t *testing.T) {
	// 预算 40、每项成本 30：第一项放行，第二项因预算不足跳过并标记被裁。
	tasks := map[string]*upstream.Task{
		"a": taskOf("a", 100),
		"b": taskOf("b", 100),
	}
	acts := []autoAction{billedAction("a", 1), billedAction("b", 1)}
	plan := buildTaskPlan(tasks, acts, TaskROI{UnitChatCost: 30, DailyCreditBudget: 40},
		func(c string, chats int) (int64, string) { return 30 * int64(chats), "estimate" }, 0)

	if plan.Items[0].Decision != decisionRun {
		t.Fatalf("第一项应执行，得到 %q（%s）", plan.Items[0].Decision, plan.Items[0].Reason)
	}
	if plan.Items[1].Decision != decisionSkip {
		t.Fatalf("第二项应因预算跳过，得到 %q（%s）", plan.Items[1].Decision, plan.Items[1].Reason)
	}
	if !plan.PartialBudgetCut {
		t.Fatal("应标记部分计费项被预算裁掉")
	}

	// 当日已花 40 且预算 40：所有计费项一律跳过，零成本项照做。
	tasks["free"] = taskOf("free", 10)
	acts = append(acts, freeAction("free"))
	spent := buildTaskPlan(tasks, acts, TaskROI{UnitChatCost: 30, DailyCreditBudget: 40},
		func(c string, chats int) (int64, string) { return 30 * int64(chats), "estimate" }, 40)
	if spent.BudgetLeft != 0 {
		t.Fatalf("预算剩余应为 0，得到 %d", spent.BudgetLeft)
	}
	if spent.Items[2].Decision != decisionRun {
		t.Fatalf("零成本项不受预算影响，得到 %q", spent.Items[2].Decision)
	}
}

func TestBuildTaskPlanClaimWhenReached(t *testing.T) {
	// 进度已达标：只领奖（成本 0、不发对话），即使它在动作表里是计费动作。
	tasks := map[string]*upstream.Task{"expert_5": {
		TaskCode: "expert_5", Credit: 500, RewardKnown: true,
		Target: 5, Current: 5, ProgressKnown: true,
	}}
	acts := []autoAction{billedAction("expert_5", 5)}
	plan := buildTaskPlan(tasks, acts, TaskROI{UnitChatCost: 30},
		func(c string, chats int) (int64, string) { return 30 * int64(chats), "estimate" }, 0)

	if plan.ClaimCount != 1 || plan.RunCount != 0 {
		t.Fatalf("claim=%d run=%d，期望 1/0", plan.ClaimCount, plan.RunCount)
	}
	if plan.ProjectedCost != 0 || plan.ProjectedNet != 500 {
		t.Fatalf("只领奖不该计成本：cost=%d net=%d", plan.ProjectedCost, plan.ProjectedNet)
	}
	if plan.Chats != 0 {
		t.Fatalf("不该发真实对话，Chats=%d", plan.Chats)
	}
}

func TestCreditLedgerRecordsAndPersists(t *testing.T) {
	dir := t.TempDir()
	p := &Panel{cfg: Config{DataDir: dir}}
	p.recordTaskCost("u1", "expert_5", 5, 150) // 30 分/次
	p.recordTaskCost("u1", "expert_5", 5, 100) // 两次样本：250/10 = 25 分/次

	roi := TaskROI{UnitChatCost: 99}
	cost, src := p.taskCostOf(roi, "expert_5", 5)
	if src != "measured" || cost != 125 {
		t.Fatalf("实测成本应为 measured/125，得到 %s/%d", src, cost)
	}
	// 没样本的 code 走配置兜底估值。
	if cost, src := p.taskCostOf(roi, "unknown", 2); src != "estimate" || cost != 198 {
		t.Fatalf("无样本应回退估值，得到 %s/%d", src, cost)
	}
	if spent := p.spentToday("u1"); spent != 250 {
		t.Fatalf("当日累计花费 = %d，期望 250", spent)
	}
	// 无效样本（花费 <=0 或对话数 <=0）直接丢弃，不污染均值。
	p.recordTaskCost("u1", "expert_5", 0, 100)
	p.recordTaskCost("u1", "expert_5", 5, 0)
	if spent := p.spentToday("u1"); spent != 250 {
		t.Fatalf("无效样本不该计入，得到 %d", spent)
	}

	// 重新加载（模拟进程重启）：账本从磁盘读回，均值不变。
	p2 := &Panel{cfg: Config{DataDir: dir}}
	if cost, src := p2.taskCostOf(roi, "expert_5", 5); src != "measured" || cost != 125 {
		t.Fatalf("重启后账本丢失：%s/%d", src, cost)
	}
	if table := p2.taskCostTable(); table["expert_5"] != 25 {
		t.Fatalf("账本表单次成本 = %d，期望 25", table["expert_5"])
	}
}

func TestTaskSnapshotMapDefaultWins(t *testing.T) {
	// 默认口径与小程序口径同名时以默认口径（先出现者）为准，空 code 丢弃。
	def := []upstream.Task{{TaskCode: "a", Credit: 100}, {TaskCode: ""}}
	mp := []upstream.Task{{TaskCode: "a", Credit: 999}, {TaskCode: "b", Credit: 50}}
	snap := taskSnapshotMap(def, mp)
	if len(snap) != 2 {
		t.Fatalf("快照条目 = %d，期望 2", len(snap))
	}
	if snap["a"].Credit != 100 {
		t.Fatalf("同名任务应取默认口径，得到 %d", snap["a"].Credit)
	}
	if snap["b"].Credit != 50 {
		t.Fatalf("mp 口径独有条目应保留，得到 %d", snap["b"].Credit)
	}
}
