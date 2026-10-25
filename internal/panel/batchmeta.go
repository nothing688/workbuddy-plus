// batchmeta.go 批量移动账号到分组的面板后端。
//
// 与单个账号的 /accounts/{uid}/meta 同一口径，只是把「逐个改」收成一次请求：
//   - 只改分组，备注一律沿用账号原值（SetMeta 是整条覆盖，不先取 Note 就会把
//     运维手写的备注清掉，导入处也是这么处理的）；
//   - UID 先去重再处理，重复项不会重复计数；
//   - 不存在的 UID 不报错，收进 missing 返回（部分成功仍是 200，前端只提示差异），
//     这样批量操作是幂等的：重复点一次不会因为某个号已被删就整批失败。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

// metaBatchMaxUIDs 单次批量移动的 UID 上限。面板全选几千个号没有意义，
// 且大批量会让 SetMeta 的逐条落盘拖长请求；超过直接拒绝，由前端分批。
const metaBatchMaxUIDs = 1000

// accountsMetaBatch POST /panel/api/accounts/meta_batch
//
// body {"uids":["uid1","uid2"],"group":"teamA"}；group 空串 = 移出分组（未分组）。
// 成功 → {ok:true, updated:N, missing:[...]}。
func (p *Panel) accountsMetaBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UIDs  []string `json:"uids"`
		Group string   `json:"group"`
	}
	// 坏 JSON 必须在动账号之前失败：解到一半就改分组会留下半批脏数据。
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	// 上限按原始条数判（去重前的载荷大小才是前端一次点选的真实规模）。
	if len(req.UIDs) > metaBatchMaxUIDs {
		writeErr(w, http.StatusBadRequest, "too many uids (max 1000)")
		return
	}
	uids := dedupeTrimmedUIDs(req.UIDs)
	if len(uids) == 0 {
		writeErr(w, http.StatusBadRequest, "uids required")
		return
	}

	group := strings.TrimSpace(req.Group)
	updated := 0
	missing := make([]string, 0) // 空也序列化成 []，前端不必判空
	for _, uid := range uids {
		st, ok := p.cfg.Pool.Status(uid)
		if !ok {
			missing = append(missing, uid)
			continue
		}
		// 保留原备注：只换分组。SetMeta 返回 false 说明账号在这两步之间被删了，
		// 按「不存在」处理，与前面 Status 未命中的口径一致。
		if !p.cfg.Pool.SetMeta(uid, group, st.Note) {
			missing = append(missing, uid)
			continue
		}
		updated++
	}

	log.Printf("panel: meta_batch group=%q updated=%d missing=%d", group, updated, len(missing))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"updated": updated,
		"missing": missing,
	})
}

// dedupeTrimmedUIDs 逐项 TrimSpace 后去重，保留首次出现的顺序（日志与响应稳定）。
// 空串项直接丢弃：前端全选时偶尔会混进空行，不该被当成一个 UID 去查。
func dedupeTrimmedUIDs(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, s := range raw {
		uid := strings.TrimSpace(s)
		if uid == "" {
			continue
		}
		if _, dup := seen[uid]; dup {
			continue
		}
		seen[uid] = struct{}{}
		out = append(out, uid)
	}
	return out
}
