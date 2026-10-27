// 分组级熔断：组内对某模型已无可服务账号时，把请求改道到组外。
//
// 与账号级熔断（entry.breakerUntil）的分工：账号级是单个号连续失败后的自保；
// 本机制是分组维度的整组让位——组里每个号对该模型都在冷却（典型是 6004 模型级
// 限流命中全组），继续在本组换号只会把每个号都撞一遍再报 503。此时把请求交给
// 其他分组的可用号，既省上游调用，也让请求有真实出口。
//
// 与「全冷却兜底」（pickEarliestExpiryLocked）的关系：兜底是最后一道选号路径，
// 本机制在它之前——组外还有可服务账号就先改道；组外也没有时仍按组内原逻辑选号
// （模型级全冷本来就没有兜底，账号级冷却的兜底语义也逐字保留）。
//
// 熔断态只记账不落盘：判定依据是组内账号的冷却状态本身，重启后首个请求重新得出，
// 不会留下「重启后熔断还在」的偏差。
package pool

import (
	"log"
	"sort"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// GroupBreakerRow 一个 (分组, 域, 模型) 的熔断画像。
type GroupBreakerRow struct {
	// Group 被熔断的分组名（请求显式指定或面板 active group）。
	Group string `json:"group"`
	// Realm 账号域（cn | global）。同名模型在两域各自独立判定。
	Realm string `json:"realm"`
	// Model 裸模型名。
	Model string `json:"model"`
	// Count 组内参与选号、但此刻对该模型不可用的账号数。
	Count int `json:"count"`
	// Since 本轮熔断开启时刻（第一次判定为全冷的时刻）。
	Since time.Time `json:"since"`
	// Until 组内最早解封时刻（零值 = 上游未给重置时刻，靠冷却 TTL 自然恢复）。
	Until time.Time `json:"until"`
	// Reason 最早解封账号的冷却原因（上游原文）。
	Reason string `json:"reason,omitempty"`
}

// groupBreakerKey 熔断台账键：分组、域、模型三者独立。
// 用 \x1f 分隔，避免分组名/模型名里出现分隔符导致的键碰撞。
func groupBreakerKey(group, realm, model string) string {
	return group + "\x1f" + realm + "\x1f" + model
}

// groupFilter 分组候选谓词：group 为空返回 nil（全池，不过滤）。
func groupFilter(group string) func(*entry) bool {
	if group == "" {
		return nil
	}
	return func(e *entry) bool { return e.group == group }
}

// SetGroupBreaker 开关分组熔断改道（main 从 config 注入，面板保存时热切换）。
// 关闭即清空台账并把选号退回纯组内语义——不保留历史熔断行，避免面板在关闭后
// 仍显示「已熔断」。
func (p *Pool) SetGroupBreaker(enabled bool) {
	if p.groupBreakerOn.Swap(enabled) == enabled {
		return
	}
	p.mu.Lock()
	cleared := len(p.groupBreakers)
	p.groupBreakers = map[string]*GroupBreakerRow{}
	p.mu.Unlock()
	if enabled {
		log.Printf("[pool] group breaker ENABLED")
		return
	}
	log.Printf("[pool] group breaker DISABLED（清空 %d 条熔断台账）", cleared)
}

// PickExcludingForRealmGroupWithFailover 在 PickExcludingForRealmGroup 之上叠加
// 分组熔断：组内对该模型已无可服务账号时改道组外。
//
// 返回值 from 非空表示「本组已被判定熔断」：
//   - acct != nil && from != ""：已改道成功，acct 来自组外；
//   - acct == nil && from != ""：本组熔断且组外也没有可用号（调用方据此细化错误文案）。
//
// group=="" 时与 PickExcludingForRealm 完全等价（全池没有「组」可熔断）。
func (p *Pool) PickExcludingForRealmGroupWithFailover(tried map[string]bool, reqModel, realm, group string) (*auth.Auth, string) {
	if group == "" || !p.groupBreakerOn.Load() {
		return p.pick(tried, reqModel, realm, groupFilter(group)), ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	total, usable, until, reason := p.groupModelStateLocked(group, reqModel, realm, now)
	if total > 0 && usable == 0 {
		p.openGroupBreakerLocked(group, reqModel, realm, total, until, reason, now)
		// 组外正常选号（healthy 优先，其次组外最早解封兜底）。
		if alt := p.pickLocked(tried, reqModel, realm, func(e *entry) bool { return e.group != group }); alt != nil {
			return alt, group
		}
		// 组外也没有可用号（或都被本轮试完）：按本组原逻辑选号，保持旧语义。
		return p.pickLocked(tried, reqModel, realm, groupFilter(group)), group
	}
	p.closeGroupBreakerLocked(group, reqModel, realm)
	return p.pickLocked(tried, reqModel, realm, groupFilter(group)), ""
}

// GroupBrokenFor 报告分组此刻是否对该 (realm, model) 处于熔断态（组内有号但全不可服务）。
// 供 handler 判断「组外粘性号是否仍是本会话的合法落点」——熔断改道后会话粘在
// 承接组上，不能因为它在原分组之外就每轮重选。
func (p *Pool) GroupBrokenFor(group, model, realm string) bool {
	if group == "" || !p.groupBreakerOn.Load() {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	total, usable, _, _ := p.groupModelStateLocked(group, model, realm, time.Now())
	return total > 0 && usable == 0
}

// GroupBreakers 当前仍处于熔断态的分组清单（按 group / realm / model 排序）。
//
// 读时自愈：组内重新出现可服务账号、或组已不存在（账号被移走/改名）的条目会在
// 这里清掉——面板每几秒拉一次，熔断的「开→合」不依赖请求流量。
func (p *Pool) GroupBreakers() []GroupBreakerRow {
	if !p.groupBreakerOn.Load() {
		return []GroupBreakerRow{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	out := make([]GroupBreakerRow, 0, len(p.groupBreakers))
	for key, row := range p.groupBreakers {
		total, usable, until, reason := p.groupModelStateLocked(row.Group, row.Model, row.Realm, now)
		if total == 0 || usable > 0 {
			delete(p.groupBreakers, key)
			continue
		}
		row.Count = total
		row.Until = until
		row.Reason = reason
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		if out[i].Realm != out[j].Realm {
			return out[i].Realm < out[j].Realm
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// groupModelStateLocked 统计分组内对 (realm, model) 的账号总数与可服务数。
//
// 口径与选号一致，但**不看 tried / 在途**：本轮已试过、恰好在途占满都属于轮换
// 穷尽或瞬时排队，不是「整组冷却」，不该触发熔断改道。
//   - total：参与选号（非禁用、非暂停）的组内账号数；
//   - usable：healthyForModel 成立且未被积分保底 / 保留积分停牌拦住的账号数；
//   - until/reason：组内最早解封账号的截止时刻与原因（供面板「还要锁多久」）。
//
// 调用方须持有 p.mu（读或写锁均可，本方法只读账号状态）。
func (p *Pool) groupModelStateLocked(group, model, realm string, now time.Time) (total, usable int, until time.Time, reason string) {
	for _, e := range p.byUID {
		if e.group != group {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		// 禁用 / 暂停是运维的显式意图，不算「冷却不可用」：全禁用的组不该被
		// 自动改道到别的组（那会绕过运维的停用决定）。这类组仍按旧逻辑报
		// no_account_in_group。
		if e.disabled || e.paused {
			continue
		}
		total++
		if !e.healthyForModel(now, model) {
			t, why := e.unblockAt(now, model)
			if !t.IsZero() && (until.IsZero() || t.Before(until)) {
				until, reason = t, why
			}
			continue
		}
		if p.floorBlockedForRealmModel(e, model, realm, now) || p.reserveBlocked(e, p.modelFreeOf(e, model)) {
			continue // 健康但被保底/停牌拦住：同样不可接单
		}
		usable++
	}
	return total, usable, until, reason
}

// unblockAt 单个账号对某模型最近一次解封的时刻与原因：模型级冷却（6004/11102）
// 优先，其次账号级的 until / 熔断 / 降权截止。都没有返回零值。
func (e *entry) unblockAt(now time.Time, model string) (time.Time, string) {
	if mc, ok := e.modelCooldowns[model]; ok && !mc.AuditOnly && !mc.Until.IsZero() && now.Before(mc.Until) {
		return mc.Until, mc.Reason
	}
	if t := e.expiry(now); !t.IsZero() {
		return t, e.reason
	}
	return time.Time{}, ""
}

// openGroupBreakerLocked 登记/刷新熔断态；只有「从未熔断 → 熔断」那一次打日志，
// 避免高流量下每个请求刷一条。
func (p *Pool) openGroupBreakerLocked(group, model, realm string, count int, until time.Time, reason string, now time.Time) {
	if p.groupBreakers == nil {
		p.groupBreakers = map[string]*GroupBreakerRow{}
	}
	key := groupBreakerKey(group, realm, model)
	row := p.groupBreakers[key]
	if row == nil {
		row = &GroupBreakerRow{Group: group, Realm: realm, Model: model, Since: now}
		p.groupBreakers[key] = row
		log.Printf("[pool] group breaker OPEN group=%q realm=%q model=%q accounts=%d until=%s reason=%q",
			group, realm, model, count, formatBreakerTime(until), reason)
	}
	row.Count, row.Until, row.Reason = count, until, reason
}

// closeGroupBreakerLocked 组内恢复可服务账号时闭合熔断。
func (p *Pool) closeGroupBreakerLocked(group, model, realm string) {
	key := groupBreakerKey(group, realm, model)
	row, ok := p.groupBreakers[key]
	if !ok {
		return
	}
	delete(p.groupBreakers, key)
	log.Printf("[pool] group breaker CLOSED group=%q realm=%q model=%q open_for=%s",
		group, realm, model, time.Since(row.Since).Round(time.Second))
}

// formatBreakerTime 零值打印成 "-"（上游未给重置时刻时 Until 为空）。
func formatBreakerTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format(time.RFC3339)
}
