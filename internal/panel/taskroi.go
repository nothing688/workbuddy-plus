// taskroi.go 成长任务的「积分效益」引擎：把「一键全跑」改成「按净收益跑」。
//
// 为什么需要它（2026-10-27 生产事故）：账号池把 30 项成长任务全量跑了一遍，
// 单号一天净减少 520–666 积分——任务奖励（+50 ~ +300 分/项）小于点亮任务所花的
// 真实对话成本（每项 1–10 次 glm-5.2 / fast-model 真实 chat）。也就是说：
// 「把所有任务都做完」在积分上是**净亏**的生意，而面板此前只有全跑与全不跑两档。
//
// 本文件的判据全部来自上游事实，不靠猜测：
//   - 收益：任务列表里的 reward_credit（upstream.Task.Credit，RewardKnown 才作数）；
//   - 成本：本机实测账本（data/task-credit-ledger.json）——每个计费动作执行前后各读
//     一次账号余额（UserResource 是免费只读接口），差值即该动作真实花掉的积分，
//     按任务 code 累计样本取均值；无样本时用配置的 task_unit_chat_cost 兜底。
//
// 三种模式（config schedule.task_mode）：
//   - all  全量执行（旧行为，保留兼容；净亏风险自负）
//   - free 只做零成本任务（纯遥测上报 / 已达标只领奖），一次真实对话都不发
//   - roi  零成本任务全做；计费任务只在「奖励 − 成本 ≥ task_min_net_credit」且
//     未超出 task_daily_credit_budget 时才做（缺省模式）
package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/safefile"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// 三种任务执行模式的取值（config schedule.task_mode）。
const (
	// TaskModeAll 全量执行：不看收益，把 autoActions 里每一项都跑一遍（旧行为）。
	TaskModeAll = "all"
	// TaskModeFree 只做零成本任务：计费动作一律跳过。
	TaskModeFree = "free"
	// TaskModeROI 按净收益执行：零成本全做，计费项按实测 ROI 与预算闸门决定（缺省）。
	TaskModeROI = "roi"
)

// defaultUnitChatCost 单次真实对话的兜底成本（积分）。
//
// 取 30 的依据：2026-10-27 生产实测单号满量跑 30 项任务净减少 520–666 分，而满量
// 队列里真实对话约 19 次（Model_chat 1 + expert_5 5 + Expert_team_use_3 3 + skill_1 1
// + Expert_lighthouse 1 + WorkBuddy_Valorant 1 + expert_use_* 4 + black_cat 3），
// 520/19 ≈ 27、666/19 ≈ 35。有实测样本后账本均值会覆盖这个估值。
const defaultUnitChatCost = 30

// TaskROI 成长任务的积分效益设置（config schedule.task_* 热透传到面板）。
type TaskROI struct {
	Mode string // all / free / roi；空或非法 → roi
	// MinNetCredit 只对计费动作生效：净收益（奖励 − 成本）达到该值才执行。
	MinNetCredit int64
	// DailyCreditBudget 单账号每日计费积分预算（>0 才生效）：本日已花 + 本次预估成本
	// 超过预算就跳过剩余计费动作。0 = 不限。
	DailyCreditBudget int64
	// UnitChatCost 单次真实对话的兜底成本（积分），<=0 → defaultUnitChatCost。
	UnitChatCost int64
}

// Normalize 归一化：模式非法回退 roi，成本非正回退兜底值，负门槛/预算归零。
func (r TaskROI) Normalize() TaskROI {
	r.Mode = strings.ToLower(strings.TrimSpace(r.Mode))
	switch r.Mode {
	case TaskModeAll, TaskModeFree, TaskModeROI:
	default:
		r.Mode = TaskModeROI
	}
	if r.UnitChatCost <= 0 {
		r.UnitChatCost = defaultUnitChatCost
	}
	if r.MinNetCredit < 0 {
		r.MinNetCredit = 0
	}
	if r.DailyCreditBudget < 0 {
		r.DailyCreditBudget = 0
	}
	return r
}

// ---------------------------------------------------------------------------
// 实测成本账本
// ---------------------------------------------------------------------------

// taskCostStat 单个任务 code 的成本样本统计。
type taskCostStat struct {
	Samples int   `json:"samples"` // 有效样本数（余额差值 > 0 的次数）
	Spent   int64 `json:"spent"`   // 累计实测花费（积分）
	Chats   int   `json:"chats"`   // 累计对话次数（与 Spent 同批）
	// UpdatedAt 最近一次采样时间（unix 秒）。
	UpdatedAt int64 `json:"updated_at"`
}

// avgPerChat 单次对话的实测均摊成本；无样本返回 0。
func (s taskCostStat) avgPerChat() int64 {
	if s.Chats <= 0 || s.Spent <= 0 {
		return 0
	}
	return s.Spent / int64(s.Chats)
}

// dailyCreditSpend 单账号当日已花的计费积分（预算闸门用，跨日自动归零）。
type dailyCreditSpend struct {
	Date  string `json:"date"`  // YYYY-MM-DD（本地时区）
	Spent int64  `json:"spent"` // 当日累计计费积分
}

// creditLedgerFile 账本文件结构（data/task-credit-ledger.json）。
type creditLedgerFile struct {
	Version int                          `json:"version"`
	Saved   int64                        `json:"saved"`
	Codes   map[string]*taskCostStat     `json:"codes"`
	Daily   map[string]*dailyCreditSpend `json:"daily"`
}

func newCreditLedger() *creditLedgerFile {
	return &creditLedgerFile{
		Version: 1,
		Codes:   map[string]*taskCostStat{},
		Daily:   map[string]*dailyCreditSpend{},
	}
}

// creditLedgerState 账本的运行期持有：懒加载一次，写入即落盘。
type creditLedgerState struct {
	mu   sync.Mutex
	file *creditLedgerFile
}

// ledgerPath 账本落盘路径；DataDir 为空（未配置数据卷/测试）时返回 ""，表示只在内存里用。
func (p *Panel) ledgerPath() string {
	if strings.TrimSpace(p.cfg.DataDir) == "" {
		return ""
	}
	return filepath.Join(p.cfg.DataDir, "task-credit-ledger.json")
}

// creditLedger 取（必要时从磁盘加载）账本。
func (p *Panel) creditLedger() *creditLedgerFile {
	p.credit.mu.Lock()
	defer p.credit.mu.Unlock()
	return p.ledgerLocked()
}

// ledgerLocked 同 creditLedger，但要求调用方已持有 p.credit.mu（懒加载在锁内完成）。
func (p *Panel) ledgerLocked() *creditLedgerFile {
	if p.credit.file != nil {
		return p.credit.file
	}
	p.credit.file = newCreditLedger()
	path := p.ledgerPath()
	if path == "" {
		return p.credit.file
	}
	raw, err := safefile.Read(path, nil)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("WARN: task ledger: 读取失败，按空账本继续: %v", err)
		}
		return p.credit.file
	}
	var f creditLedgerFile
	if err := json.Unmarshal(raw, &f); err != nil {
		log.Printf("WARN: task ledger: 解析失败，按空账本继续: %v", err)
		return p.credit.file
	}
	if f.Codes == nil {
		f.Codes = map[string]*taskCostStat{}
	}
	if f.Daily == nil {
		f.Daily = map[string]*dailyCreditSpend{}
	}
	f.Version = 1
	p.credit.file = &f
	return p.credit.file
}

// saveCreditLedgerLocked 落盘（调用方已持锁）。失败只 WARN：账本是自校准数据，
// 丢一轮样本不影响正确性，绝不因为写盘失败中断任务执行。
func (p *Panel) saveCreditLedgerLocked(f *creditLedgerFile) {
	path := p.ledgerPath()
	if path == "" {
		return
	}
	f.Saved = time.Now().Unix()
	raw, err := json.Marshal(f)
	if err != nil {
		log.Printf("WARN: task ledger: 序列化失败: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("WARN: task ledger: 建目录失败: %v", err)
		return
	}
	if err := safefile.WriteKeep(path, raw, 0o600); err != nil {
		log.Printf("WARN: task ledger: 写盘失败: %v", err)
	}
}

// recordTaskCost 记一次计费动作的实测成本（spent 积分 / chats 次对话），
// 同时累加该账号当日的计费花费（供预算闸门用）。spent <= 0 视为无效样本丢弃。
func (p *Panel) recordTaskCost(uid, code string, chats int, spent int64) {
	if spent <= 0 || chats <= 0 {
		return
	}
	if strings.TrimSpace(code) == "" {
		return
	}
	p.credit.mu.Lock()
	defer p.credit.mu.Unlock()
	f := p.ledgerLocked()
	st := f.Codes[code]
	if st == nil {
		st = &taskCostStat{}
		f.Codes[code] = st
	}
	st.Samples++
	st.Spent += spent
	st.Chats += chats
	st.UpdatedAt = time.Now().Unix()
	if uid != "" {
		today := time.Now().Format("2006-01-02")
		d := f.Daily[uid]
		if d == nil || d.Date != today {
			d = &dailyCreditSpend{Date: today}
			f.Daily[uid] = d
		}
		d.Spent += spent
	}
	p.saveCreditLedgerLocked(f)
}

// spentToday 该账号当日已花的计费积分（跨日自动归零）。
func (p *Panel) spentToday(uid string) int64 {
	if uid == "" {
		return 0
	}
	p.credit.mu.Lock()
	defer p.credit.mu.Unlock()
	f := p.ledgerLocked()
	d := f.Daily[uid]
	if d == nil || d.Date != time.Now().Format("2006-01-02") {
		return 0
	}
	return d.Spent
}

// taskCostOf 估算某任务 code 跑 chats 次真实对话的积分成本，返回成本与来源
// （measured = 账本实测均值，estimate = 配置兜底估值）。
func (p *Panel) taskCostOf(roi TaskROI, code string, chats int) (int64, string) {
	if chats <= 0 {
		return 0, "free"
	}
	p.credit.mu.Lock()
	f := p.ledgerLocked()
	var avg int64
	if st := f.Codes[code]; st != nil {
		avg = st.avgPerChat()
	}
	p.credit.mu.Unlock()
	if avg > 0 {
		return avg * int64(chats), "measured"
	}
	return roi.UnitChatCost * int64(chats), "estimate"
}

// taskCostTable 各任务 code 的实测单次对话成本（预览接口展示用）。
func (p *Panel) taskCostTable() map[string]int64 {
	p.credit.mu.Lock()
	f := p.ledgerLocked()
	out := make(map[string]int64, len(f.Codes))
	for code, st := range f.Codes {
		out[code] = st.avgPerChat()
	}
	p.credit.mu.Unlock()
	return out
}

// ---------------------------------------------------------------------------
// 规划器（纯函数，便于单测）
// ---------------------------------------------------------------------------

// 决策取值。
const (
	decisionRun   = "run"   // 执行动作（免费动作，或计费但合算）
	decisionClaim = "claim" // 进度已达标：只领奖，不发任何计费动作
	decisionSkip  = "skip"  // 不执行
)

// taskPlanItem 单个任务的计划条目。
type taskPlanItem struct {
	TaskCode    string `json:"task_code"`
	Desc        string `json:"desc,omitempty"`
	Chats       int    `json:"chats"` // 该动作内的真实对话次数（0 = 零成本）
	Reward      int64  `json:"reward"`
	RewardKnown bool   `json:"reward_known"`
	EstCost     int64  `json:"est_cost"`
	CostSource  string `json:"cost_source,omitempty"` // free / measured / estimate
	Net         int64  `json:"net"`
	Decision    string `json:"decision"`
	Reason      string `json:"reason"`
}

// taskPlan 一次队列执行的完整计划与预估。
type taskPlan struct {
	Mode             string         `json:"mode"`
	UnitChatCost     int64          `json:"unit_chat_cost"`
	MinNetCredit     int64          `json:"min_net_credit"`
	DailyBudget      int64          `json:"daily_credit_budget"`
	SpentToday       int64          `json:"spent_today"`
	BudgetLeft       int64          `json:"budget_left"` // -1 = 不限
	RunCount         int            `json:"run_count"`
	ClaimCount       int            `json:"claim_count"`
	SkipCount        int            `json:"skip_count"`
	ProjectedCost    int64          `json:"projected_cost"`
	ProjectedReward  int64          `json:"projected_reward"`
	ProjectedNet     int64          `json:"projected_net"`
	Chats            int            `json:"chats"`
	Items            []taskPlanItem `json:"items"`
	PartialBudgetCut bool           `json:"partial_budget_cut"` // 有计费项因预算被裁掉
}

// buildTaskPlan 依据任务快照、动作表、ROI 设置与账号当日花费生成执行计划。
//
// tasks 以 task_code 为键（默认口径与小程序口径合并后的快照）；costOf 给出
// 「该 code 跑 chats 次对话」的预估成本与来源；spentToday 是该账号当日已花的计费积分。
func buildTaskPlan(tasks map[string]*upstream.Task, acts []autoAction, roi TaskROI, costOf func(code string, chats int) (int64, string), spentToday int64) taskPlan {
	roi = roi.Normalize()
	plan := taskPlan{
		Mode:         roi.Mode,
		UnitChatCost: roi.UnitChatCost,
		MinNetCredit: roi.MinNetCredit,
		DailyBudget:  roi.DailyCreditBudget,
		SpentToday:   spentToday,
		BudgetLeft:   -1,
		Items:        make([]taskPlanItem, 0, len(acts)),
	}
	if roi.DailyCreditBudget > 0 {
		left := roi.DailyCreditBudget - spentToday
		if left < 0 {
			left = 0
		}
		plan.BudgetLeft = left
	}
	remaining := plan.BudgetLeft // -1 表示不限
	for _, act := range acts {
		item := taskPlanItem{TaskCode: act.TaskCode, Desc: act.Desc, Chats: act.Chats}
		t := tasks[act.TaskCode]
		switch {
		case t == nil:
			item.Decision, item.Reason = decisionSkip, "该账号未下发此任务"
		case t.Claimed:
			item.Decision, item.Reason = decisionSkip, "已领取过奖励"
		}
		if item.Decision == decisionSkip {
			plan.Items = append(plan.Items, item)
			plan.SkipCount++
			continue
		}
		item.Reward = t.Credit
		item.RewardKnown = t.RewardKnown || t.Credit > 0
		// 零成本动作（纯遥测上报）与「进度已达标只领奖」永远做：它们不花积分。
		free := act.Chats <= 0 || act.ClaimOnly || act.run == nil
		reached := taskProgressReached(t)
		if free || reached {
			item.EstCost, item.CostSource, item.Net = 0, "free", item.Reward
			if reached && !isFreeAction(act) {
				item.Reason = "进度已达标：只领奖，不再发真实对话"
			} else {
				item.Reason = "零成本动作"
			}
			item.Decision = decisionRun
			if reached {
				item.Decision = decisionClaim
				plan.ClaimCount++
			} else {
				plan.RunCount++
			}
			plan.ProjectedReward += item.Reward
			plan.ProjectedNet += item.Reward
			plan.Items = append(plan.Items, item)
			continue
		}
		// 计费动作：成本未知也要先算出来（用于展示），再按模式定夺。
		item.EstCost, item.CostSource = costOf(act.TaskCode, act.Chats)
		item.Net = item.Reward - item.EstCost
		switch roi.Mode {
		case TaskModeAll:
			item.Decision, item.Reason = decisionRun, "全量模式：不设收益闸门"
		case TaskModeFree:
			item.Decision = decisionSkip
			item.Reason = fmt.Sprintf("免费模式：跳过计费动作（预计花费 %d 分）", item.EstCost)
		default: // roi
			if !item.RewardKnown {
				item.Decision = decisionSkip
				item.Reason = "上游未下发奖励值，无法确认净收益"
				break
			}
			if item.Net < roi.MinNetCredit {
				item.Decision = decisionSkip
				item.Reason = fmt.Sprintf("净收益 %d 分低于门槛 %d（奖励 %d − 成本 %d）",
					item.Net, roi.MinNetCredit, item.Reward, item.EstCost)
				break
			}
			if remaining >= 0 {
				if item.EstCost > remaining {
					item.Decision = decisionSkip
					item.Reason = fmt.Sprintf("当日计费预算不足（剩余 %d 分，需 %d 分）", remaining, item.EstCost)
					plan.PartialBudgetCut = true
					break
				}
				remaining -= item.EstCost
			}
			item.Decision = decisionRun
			item.Reason = fmt.Sprintf("净收益 +%d 分（奖励 %d − 成本 %d，成本来源 %s）",
				item.Net, item.Reward, item.EstCost, item.CostSource)
		}
		if item.Decision == decisionRun {
			plan.RunCount++
			plan.ProjectedCost += item.EstCost
			plan.ProjectedReward += item.Reward
			plan.ProjectedNet += item.Net
			plan.Chats += act.Chats
		} else {
			plan.SkipCount++
		}
		plan.Items = append(plan.Items, item)
	}
	return plan
}

// isFreeAction 该动作是否零成本（不发真实模型对话）。
func isFreeAction(act autoAction) bool { return act.Chats <= 0 }

// planSummary 计划的一句话摘要（日志 / 面板事件用）。
func (pl taskPlan) summary() string {
	s := fmt.Sprintf("模式 %s：执行 %d 项、只领奖 %d 项、跳过 %d 项；预估花 %d 分、得 %d 分、净 %+d 分",
		pl.Mode, pl.RunCount, pl.ClaimCount, pl.SkipCount, pl.ProjectedCost, pl.ProjectedReward, pl.ProjectedNet)
	if pl.DailyBudget > 0 {
		s += fmt.Sprintf("（当日已花 %d / 预算 %d）", pl.SpentToday, pl.DailyBudget)
	}
	return s
}

// taskSnapshotMap 任务列表按 task_code 建索引；同 code 重复时以先出现的为准
// （默认口径优先，mp 口径补缺）。
func taskSnapshotMap(lists ...[]upstream.Task) map[string]*upstream.Task {
	out := map[string]*upstream.Task{}
	for _, list := range lists {
		for i := range list {
			code := strings.TrimSpace(list[i].TaskCode)
			if code == "" {
				continue
			}
			if _, ok := out[code]; ok {
				continue
			}
			t := list[i]
			out[code] = &t
		}
	}
	return out
}

// accountTaskPlan GET /panel/api/accounts/{uid}/tasks/plan
//
// 预览该账号在当前策略下的积分效益计划：跑哪些、只领奖哪些、跳过哪些，以及预估
// 成本/收益/净额。只读——只查任务列表，不发任何行为或领奖请求，可随时点。
func (p *Panel) accountTaskPlan(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	if a.IsGlobal() {
		writeErr(w, http.StatusNotImplemented, globalTaskWriteMessage)
		return
	}
	defaults, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "查询任务失败: "+err.Error())
		return
	}
	mp, mpErr := p.cfg.Upstream.ListTasksMP(a)
	roi := p.taskROI()
	snapshot := taskSnapshotMap(defaults, mp)
	plan := buildTaskPlan(snapshot, autoActions, roi, func(code string, chats int) (int64, string) {
		return p.taskCostOf(roi, code, chats)
	}, p.spentToday(a.UID))
	resp := map[string]any{
		"ok":         true,
		"uid":        a.UID,
		"plan":       plan,
		"cost_table": p.taskCostTable(), // 各任务实测单次对话成本（无样本则缺省）
		"summary":    plan.summary(),
	}
	if mpErr != nil {
		resp["mp_error"] = mpErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}
