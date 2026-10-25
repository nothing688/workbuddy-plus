// export.go 账号导出：把池内全部账号序列化成与导入同构的 JSON
// （{"account":{...},"auth":{...},"group","note"} 数组），用于迁移/备份。
//
// 安全提示：导出文件包含 access/refresh/device token 明文，等同凭证本身；
// 接口走面板鉴权，前端下载前也会提示，不要随手转发或提交到仓库。
package panel

import (
	"net/http"
	"time"
)

// exportAccount 单个账号的导出形态；字段名与桌面端导出、本仓通用导入互相兼容。
type exportAccount struct {
	Account struct {
		UID          string `json:"uid"`
		Nickname     string `json:"nickname"`
		EnterpriseID string `json:"enterpriseId,omitempty"`
	} `json:"account"`
	Auth struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken,omitempty"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain,omitempty"`
		Realm        string `json:"realm"`
		DeviceToken  string `json:"deviceToken,omitempty"`
	} `json:"auth"`
	Group string `json:"group,omitempty"`
	Note  string `json:"note,omitempty"`
}

// exportAccounts GET /panel/api/export/accounts
//
//	→ {"exported_at":"RFC3339","accounts":[{account,auth,group,note}, ...]}
//
// 顺序与池内 List 一致（UID 升序），便于 diff 两次导出。
func (p *Panel) exportAccounts(w http.ResponseWriter, r *http.Request) {
	list := p.cfg.Pool.List()
	out := make([]exportAccount, 0, len(list))
	for _, st := range list {
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		var item exportAccount
		item.Account.UID = st.UID
		item.Account.Nickname = st.Nickname
		item.Account.EnterpriseID = a.EnterpriseID
		// token 字段经锁读取：RefreshToken 可能正被后台刷新线程改写。
		a.Lock()
		item.Auth.AccessToken = a.AccessToken
		item.Auth.RefreshToken = a.RefreshToken
		item.Auth.ExpiresAt = a.ExpiresAt
		item.Auth.Domain = a.Domain
		item.Auth.DeviceToken = a.DeviceToken
		a.Unlock()
		item.Auth.Realm = st.Realm
		item.Group = st.Group
		item.Note = st.Note
		out = append(out, item)
	}

	name := "workbuddy-accounts-" + time.Now().Format("20060102-150405") + ".json"
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"exported_at": time.Now().Format(time.RFC3339),
		"accounts":    out,
	})
}
