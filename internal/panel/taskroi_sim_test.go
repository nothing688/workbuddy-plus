package panel

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// simRewards 离线演算用的奖励快照。**不是**线上实盘数据：
//   - 标注「实证」的数值来自本仓库动作表注释里记录过的上游下发值；
//   - 其余标记为未知（模拟「上游没给 reward_credit」的情形），引擎的处置是
//     不花钱、只尝试补领——这正是 ROI 模式的兜底行为。
//
// 真实部署里 reward 由 ListTasks/ListTasksMP 的 reward_credit 提供，本表只用于
// 在没有账号的情况下推演「收益 / 成本」的相对关系。
var simRewards = map[string]int64{
	"first_buddy":                   300, // 实证
	"create_canvas":                 300, // 实证
	"expert_use_maidehao":           50,  // 实证
	"expert_use_douyinyin":          50,  // 实证
	"expert_use_shangjia_xuanchuan": 50,  // 实证
	"expert_use_bowangwang":         50,  // 实证
	"school_season":                 100, // 实证
	"Sequential_Tasks_1":            100, // 实证
	"Sequential_Tasks_2":            200, // 实证
	"Sequential_Tasks_3":            300, // 实证
	"Sequential_Tasks_7":            500, // 实证口径（注释记为「疑 +500c+5e」）
}

// TestTaskROISimulation 打印「积分效益」演算表：给定单位对话成本，哪些任务
// 值得花钱、哪些应该只做零成本的遥测类任务。用 `go test -run Simulation -v` 看输出。
//
// 演算只调用纯函数 buildTaskPlan，不访问任何账号、不发网络请求。
func TestTaskROISimulation(t *testing.T) {
	codes := make([]string, 0, len(autoActions))
	tasks := map[string]*upstream.Task{}
	for _, act := range autoActions {
		code := act.TaskCode
		codes = append(codes, code)
		reward, known := simRewards[code]
		tasks[code] = &upstream.Task{
			TaskCode:      code,
			Credit:        reward,
			RewardKnown:   known,
			AcceptStatus:  "accepted",
			Status:        "in_progress",
			ProgressKnown: true,
			Target:        1,
			Current:       0,
		}
	}

	roiMode := TaskROI{Mode: TaskModeROI, UnitChatCost: defaultUnitChatCost}
	plainCost := func(code string, chats int) (int64, string) {
		if chats <= 0 {
			return 0, "free"
		}
		return int64(chats) * roiMode.UnitChatCost, "estimate"
	}

	// ── 表 1：零成本 vs 计费，以及每个计费任务的「盈亏平衡单次成本」
	t.Log("=== 表1 任务成本结构（单位对话成本 = " + fmt.Sprint(defaultUnitChatCost) + " 分）===")
	t.Log(fmt.Sprintf("%-32s %6s %7s %9s %9s %9s %s", "task_code", "chats", "reward", "est_cost", "net", "break_even", "零成本?"))
	billed := 0
	totalChats := 0
	freeReward := int64(0)
	for _, code := range codes {
		act := autoActionFor(code)
		if act == nil {
			continue
		}
		reward, known := simRewards[code]
		chats := act.Chats
		est := int64(chats) * roiMode.UnitChatCost
		net := reward - est
		be := "  -"
		if chats > 0 && known {
			be = fmt.Sprintf("%6.1f", float64(reward)/float64(chats))
		}
		t.Log(fmt.Sprintf("%-32s %6d %7d %9d %9d %9s %v",
			code, chats, reward, est, net, be, chats == 0))
		if chats > 0 {
			billed++
			totalChats += chats
		} else if known {
			freeReward += reward
		}
	}
	t.Log(fmt.Sprintf("合计：计费动作 %d 个 / 每轮真实对话 %d 次；零成本任务的确定奖励 %d 分", billed, totalChats, freeReward))

	// ── 表 2：三种模式在默认单位成本下的计划对比
	t.Log("=== 表2 三种模式的执行计划对比（同一任务快照）===")
	t.Log(fmt.Sprintf("%-8s %5s %5s %5s %7s %8s %8s %8s %s", "mode", "run", "claim", "skip", "chats", "cost", "reward", "net", "预算裁掉"))
	for _, mode := range []string{TaskModeAll, TaskModeFree, TaskModeROI} {
		roi := TaskROI{Mode: mode, UnitChatCost: defaultUnitChatCost}
		plan := buildTaskPlan(tasks, autoActions, roi, plainCost, 0)
		t.Log(fmt.Sprintf("%-8s %5d %5d %5d %7d %8d %8d %8d %v",
			plan.Mode, plan.RunCount, plan.ClaimCount, plan.SkipCount, plan.Chats,
			plan.ProjectedCost, plan.ProjectedReward, plan.ProjectedNet, plan.PartialBudgetCut))
	}

	// ── 表 3：单位对话成本敏感性（决策翻转点）
	t.Log("=== 表3 单位成本敏感性：ROI 模式下的净收益（分）===")
	unitCosts := []int64{1, 5, 10, 20, 30, 50, 100}
	header := fmt.Sprintf("%-34s", "task_code/chats")
	for _, u := range unitCosts {
		header += fmt.Sprintf("%8s", fmt.Sprintf("u=%d", u))
	}
	t.Log(header)
	for _, code := range codes {
		act := autoActionFor(code)
		if act == nil || act.Chats == 0 {
			continue
		}
		reward, known := simRewards[code]
		if !known {
			continue
		}
		row := fmt.Sprintf("%-34s", fmt.Sprintf("%s/%d", code, act.Chats))
		for _, u := range unitCosts {
			net := reward - int64(act.Chats)*u
			row += fmt.Sprintf("%8d", net)
		}
		t.Log(row)
	}

	// ── 断演算结论自洽：ROI 模式的净收益不得被 free 模式反超，且不得亏
	roiPlan := buildTaskPlan(tasks, autoActions, TaskROI{Mode: TaskModeROI, UnitChatCost: defaultUnitChatCost}, plainCost, 0)
	freePlan := buildTaskPlan(tasks, autoActions, TaskROI{Mode: TaskModeFree, UnitChatCost: defaultUnitChatCost}, plainCost, 0)
	allPlan := buildTaskPlan(tasks, autoActions, TaskROI{Mode: TaskModeAll, UnitChatCost: defaultUnitChatCost}, plainCost, 0)
	if roiPlan.ProjectedNet < freePlan.ProjectedNet {
		t.Fatalf("ROI 模式净收益 %d 低于 free 模式 %d，规划器有缺陷", roiPlan.ProjectedNet, freePlan.ProjectedNet)
	}
	if roiPlan.ProjectedNet < 0 {
		t.Fatalf("ROI 模式净收益为负 %d：应该跳过全部亏本任务", roiPlan.ProjectedNet)
	}
	if allPlan.Chats <= roiPlan.Chats {
		t.Fatalf("all 模式应比 roi 模式发更多真实对话：all=%d roi=%d", allPlan.Chats, roiPlan.Chats)
	}
	sort.Slice(codes, func(i, j int) bool { return codes[i] < codes[j] })
	if strings.Join(codes, "") == "" {
		t.Fatal("动作表为空")
	}
}
