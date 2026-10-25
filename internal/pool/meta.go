// meta.go 账号「分组 + 备注」：运维自定义的元数据（非凭证、非健康状态）。
//
// 定位：分组只用于「按组选号」（PickExcludingForRealmGroup）与面板归类，与自动机制
// （冷却/熔断/降权）完全正交——设分组不动任何状态机字段，健康判定也不看分组。
// 存储：stateAccount.Group/Note，随 state.json 与 Redis 快照一起往返（同源结构）。
package pool

import (
	"sort"
	"strings"
	"time"
)

// SetMeta 设置账号的分组名与备注（两个值都 TrimSpace；空串 = 清除）。
//
// 语义：
//   - uid 不存在 → 返回 false（供调用方区分「账号不存在」与「已写入」）；
//   - 两个值都未变化 → 不置 dirty（避免无意义的落盘），但仍返回 true（账号存在）；
//   - 有变化 → 写 entry 并置 dirty，由后台 flusher / Flush 落盘。
//
// 落盘路径与其它 setter 一致（dirty + saveLocked），没有额外的持久化钩子。
func (p *Pool) SetMeta(uid, group, note string) bool {
	group = strings.TrimSpace(group)
	note = strings.TrimSpace(note)
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	if e.group == group && e.note == note {
		return true
	}
	e.group, e.note = group, note
	p.dirty.Store(true)
	return true
}

// SetActiveGroup 切换默认调用池（active group）：不带分组参数的请求将走该组。
//
// 语义（与 SetMeta 同款取舍）：
//   - 值 TrimSpace 后比较；与当前值相同 → 不置 dirty（无变更不落盘）；
//   - 空串 = 清除（回到全池选号，与引入该字段前逐字一致）；
//   - 不校验分组是否存在：组名可先于账号配置（切到一个空组只会让请求报
//     no_account_in_group，等运维把号移进来即可），也避免与 SetMeta 抢锁顺序。
//
// 持久化走既有 dirty + saveLocked 路径（state.json 的 active_group，Redis 快照同源）。
func (p *Pool) SetActiveGroup(name string) {
	name = strings.TrimSpace(name)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.activeGroup == name {
		return
	}
	p.activeGroup = name
	p.dirty.Store(true)
}

// ActiveGroup 返回当前默认调用池（空串 = 全池）。读锁，供 handler 每请求读取。
func (p *Pool) ActiveGroup() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.activeGroup
}

// GroupInfo 单个分组的聚合视图（面板分组列表用）。
type GroupInfo struct {
	Name string `json:"name"` // 分组名；空串 = 未分组
	// Total 该组账号总数（含禁用/冷却/暂停）。
	Total int `json:"total"`
	// Healthy 该组账号级健康数（口径 = entry.healthy(now)：未禁用、未暂停、
	// 无有效冷却/熔断/连败降权）。**不含**在途与保留积分维度，与 CountsDetailed
	// 的 healthy 口径一致，只是按组切分。
	Healthy int `json:"healthy"`
}

// GroupCounts 按分组聚合账号数（Name 为空串的条目代表「未分组」）。
//
// 排序：非空分组名按字典序升序，未分组（空名）**排在最后**——它是「没有归类」的
// 兜底桶，混在字母序里会被误当成一个真实分组。
// 没有任何账号时返回空切片（非 nil 无意义，调用方按 len 判空即可）。
func (p *Pool) GroupCounts() []GroupInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	idx := map[string]int{} // name → out 下标
	out := make([]GroupInfo, 0, 4)
	for _, e := range p.byUID {
		i, ok := idx[e.group]
		if !ok {
			i = len(out)
			idx[e.group] = i
			out = append(out, GroupInfo{Name: e.group})
		}
		out[i].Total++
		if e.healthy(now) {
			out[i].Healthy++
		}
	}
	sort.Slice(out, func(i, j int) bool {
		// 未分组最后；其余按字典序（sort.Slice 不稳定，故比较器必须给全序）。
		if (out[i].Name == "") != (out[j].Name == "") {
			return out[j].Name == ""
		}
		return out[i].Name < out[j].Name
	})
	return out
}
