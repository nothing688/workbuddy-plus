// poolactive.go 面板「默认调用池」的读写：不带分组参数的请求走哪个组。
//
// 真正的语义与持久化都在 pool（SetActiveGroup / ActiveGroup，state.json 的
// active_group）；面板只做一层薄透出：列表顺带给出分组计数，切换时原样写回。
// 刻意**不校验组是否存在**：组名可以先于账号配置（切到空组只会让请求报
// no_account_in_group），面板不该替运维拦这一步。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
)

// poolActiveGet GET /panel/api/pool/active
//
// 回包 {"group":"teamA","groups":[{"name":"","total":3,"healthy":2}, ...]}。
// groups 直接复用 pool.GroupCounts（空名 = 未分组，排在最后），与账号池的
// 分组筛选、按组选号使用同一份聚合口径。
func (p *Panel) poolActiveGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"group":  p.cfg.Pool.ActiveGroup(),
		"groups": p.cfg.Pool.GroupCounts(),
	})
}

// poolActiveSet POST /panel/api/pool/active
//
// body {"group":"teamA"}；空串 = 清除（回到全池）。响应回读实际生效值
// （SetActiveGroup 会 TrimSpace），前端据此更新徽章与下拉，不自己猜。
func (p *Panel) poolActiveSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Group string `json:"group"`
	}
	// 坏 JSON 必须报错：静默当成空串会把运维的调用池意外清成「全池」。
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	p.cfg.Pool.SetActiveGroup(req.Group)
	group := p.cfg.Pool.ActiveGroup()
	log.Printf("panel: active pool=%q", group)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "group": group})
}
