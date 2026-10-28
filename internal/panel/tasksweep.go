// tasksweep.go 补领清扫：只 accept 待接受的、只 claim 已达标未领的，不跑任何行为链路。
//
// 动机（issue #174）：达标未领的任务此前没有补领兜底——新号自动跑时它们还没达标
// （awaiting_progress），成长队列一天只跑一次，白名单（autoActions）外的码更是永远
// 进不了队列。本清扫与 autoActions 无关，判据只有 progress，故周期跑即可兜住全部漏领。
package panel

import (
	"fmt"
	"log"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/logfmt"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// 领奖连续失败后的退避：同一个 (账号, 任务) 连续失败 sweepClaimFailLimit 次后，
// 跳过 sweepClaimBackoff，到点再试。为什么需要：清扫每 15 分钟一轮，一条永远领不
// 下来（上游 400「task not completed」、任务被回滚成未达标）的任务会 96 次/天重试，
// 日志刷屏且持续打上游——不是积分问题（领奖不扣分），但属于纯粹的无谓流量。
const (
	sweepClaimFailLimit = 3
	sweepClaimBackoff   = 6 * time.Hour
)

// sweepFailState 单任务退避状态（内存表：进程重启后退避清零，下一轮重新判断。
// 领奖幂等，重试本身无害，所以不需要落盘）。
type sweepFailState struct {
	fails int
	until time.Time
}

// RunTaskSweepNow 一轮清扫（scheduler 周期调用）。返回 (清扫账号数, 摘要)。
// 已禁用 / 国际区 / 企业版账号一个上游请求都不发（门控与任务中心扫描一致）；
// 正在跑任务动作的账号本轮让位（与单任务 / 一键完成 / 成长队列共用 per-account 锁）。
func (p *Panel) RunTaskSweepNow() (int, string) {
	if p.cfg.Pool == nil || p.cfg.Upstream == nil {
		return 0, "账号池未接线"
	}
	// 防重入：定时器每 15 分钟一轮，管理端 / 面板按钮可随时手动触发。两轮重叠会
	// 让同一账号的列表与领奖请求翻倍（per-account 锁只挡住动作，不挡重复清扫）。
	if !p.sweeping.CompareAndSwap(false, true) {
		return 0, "上一轮清扫尚未结束，跳过"
	}
	defer p.sweeping.Store(false)
	var swept, busy, accepted, claimed, failed int
	for _, st := range p.cfg.Pool.List() {
		if st.Disabled || st.Realm == "global" {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil || !a.SupportsPersonalGrowth() {
			continue // a==nil 无凭证；企业版 / 静态 API Key 无个人成长体系
		}
		if !p.tryLockAccount(st.UID) {
			busy++
			continue
		}
		acc, cl, err := p.sweepAccount(a)
		p.unlockAccount(st.UID)
		swept++
		accepted += acc
		claimed += cl
		if err != nil {
			failed++
			log.Printf("WARN: 补领清扫 %s: %v", logfmt.Label(a.UID, a.Nickname), err)
		}
	}
	return swept, fmt.Sprintf("清扫 %d 个账号（跳过 %d）· 接受 %d · 领奖 %d · 出错 %d",
		swept, busy, accepted, claimed, failed)
}

// sweepAccount 单账号清扫，返回 (接受数, 领奖数, 列表失败错误)。
//
// 领奖判据只用 progress（Task.Claimable）而不是 accept_status：后者会在回读时短暂转
// completed 随后被整体回滚（见 autotask.go），据此领奖会误领。领奖本身幂等（重复领
// 上游返回 already_claimed → 0 分 0 能），所以逐条失败只记 WARN 不中断，宁可多试一轮。
func (p *Panel) sweepAccount(a *auth.Auth) (accepted, claimed int, err error) {
	label := logfmt.Label(a.UID, a.Nickname)
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		return 0, 0, err
	}
	mp, mpErr := p.cfg.Upstream.ListTasksMP(a)
	if mpErr != nil {
		log.Printf("WARN: 补领清扫 %s mp 列表: %v", label, mpErr)
	}

	// 端标记不同，两种口径分开提交（缺 mp 头 accept 返回 task not found）。
	if codes := acceptPendingCodes(tasks); len(codes) > 0 {
		if err := p.cfg.Upstream.AcceptTasks(a, codes); err != nil {
			log.Printf("WARN: 补领清扫 %s 接受: %v", label, err)
		} else {
			accepted += len(codes)
		}
	}
	if codes := acceptPendingCodes(mp); len(codes) > 0 {
		if err := p.cfg.Upstream.AcceptTasksMP(a, codes); err != nil {
			log.Printf("WARN: 补领清扫 %s 接受(mp): %v", label, err)
		} else {
			accepted += len(codes)
		}
	}

	// mp 列表是默认口径的超集，按 task_code 去重（否则同一任务领两次）。
	seen := make(map[string]bool, len(tasks)+len(mp))
	onlyMP := make(map[string]bool, len(mp))
	for _, t := range mp {
		onlyMP[t.TaskCode] = true
	}
	for _, t := range tasks {
		onlyMP[t.TaskCode] = false
	}
	for _, t := range append(append([]upstream.Task{}, tasks...), mp...) {
		if t.TaskCode == "" || seen[t.TaskCode] {
			continue
		}
		seen[t.TaskCode] = true
		if !t.Claimable || t.Locked {
			continue // 锁定 = 上游未解锁（Sequential 族每日解锁一环），领也领不到
		}
		if p.sweepClaimBackingOff(a.UID, t.TaskCode) {
			continue // 连续失败已退避：等窗口过了再试，不刷日志
		}
		// mp 专属码走 mp 领奖（自带 Web 域 400 降级）；只在 mp 列表出现的码同办，
		// 否则 Web 域对 mp 任务 400 会变成每轮一条 WARN。
		var cerr error
		if isMPTaskCode(t.TaskCode) || onlyMP[t.TaskCode] {
			_, _, cerr = p.cfg.Upstream.ClaimRewardMP(a, t.TaskCode)
		} else {
			_, _, cerr = p.cfg.Upstream.ClaimReward(a, t.TaskCode)
		}
		if cerr != nil {
			p.noteSweepClaimFail(a.UID, t.TaskCode, label, cerr)
			continue
		}
		p.clearSweepClaimFail(a.UID, t.TaskCode)
		claimed++
	}
	return accepted, claimed, nil
}

// acceptPendingCodes 待接受的任务码：未领、未达标、未锁定、状态还不是 accepted/completed。
func acceptPendingCodes(tasks []upstream.Task) []string {
	var codes []string
	for i := range tasks {
		t := &tasks[i]
		if !t.Claimed && !t.Locked && !taskProgressReached(t) &&
			t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
			codes = append(codes, t.TaskCode)
		}
	}
	return codes
}

// sweepClaimBackingOff 该任务是否还在退避窗口内。
func (p *Panel) sweepClaimBackingOff(uid, code string) bool {
	p.sweepMu.Lock()
	defer p.sweepMu.Unlock()
	st, ok := p.sweepFail[uid+"|"+code]
	return ok && time.Now().Before(st.until)
}

// noteSweepClaimFail 记一次领奖失败；连续失败到阈值后开始退避，只在进入退避时打一条
// WARN（其余轮次静默跳过），避免每 15 分钟刷同一行。
func (p *Panel) noteSweepClaimFail(uid, code, label string, err error) {
	p.sweepMu.Lock()
	defer p.sweepMu.Unlock()
	if p.sweepFail == nil {
		p.sweepFail = map[string]sweepFailState{}
	}
	key := uid + "|" + code
	st := p.sweepFail[key]
	st.fails++
	if st.fails < sweepClaimFailLimit {
		p.sweepFail[key] = st
		log.Printf("WARN: 补领 %s %s: %v", label, code, err)
		return
	}
	st.until = time.Now().Add(sweepClaimBackoff)
	p.sweepFail[key] = st
	log.Printf("WARN: 补领 %s %s: 连续 %d 次失败（%v），退避 %s 再试",
		label, code, st.fails, err, sweepClaimBackoff)
}

// clearSweepClaimFail 领奖成功后清掉退避记录。
func (p *Panel) clearSweepClaimFail(uid, code string) {
	p.sweepMu.Lock()
	defer p.sweepMu.Unlock()
	delete(p.sweepFail, uid+"|"+code)
}
