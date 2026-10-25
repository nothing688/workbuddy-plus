package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// 导入字段别名表。解析时账号对象的键已统一折成小写，因此这里也全用小写：
// snake_case 与 camelCase 折叠后一致（accessToken / accesstoken → accesstoken），
// 大小写不敏感的要求由此天然满足。每个字段按表内顺序取第一个非空值。
var (
	importAliasUID = []string{"uid", "id", "user_id", "userid", "account_id", "accountid"}
	// access_token 的 `token` 别名与嵌套子对象同名；子对象是 map，取字符串时自然为空，
	// 不会遮蔽真正的顶层 token 字符串字段。
	importAliasAccess  = []string{"access_token", "accesstoken", "token", "accesstokenvalue"}
	importAliasRefresh = []string{"refresh_token", "refreshtoken", "refresh"}
	importAliasExpires = []string{"expires_at", "expiresat", "expiry", "expire_at", "expireat", "expires"}
	importAliasNick    = []string{"nickname", "name", "username", "display_name", "displayname"}
	importAliasEmail   = []string{"email", "mail"}
	importAliasDomain  = []string{"domain", "site", "host"}
	importAliasRealm   = []string{"realm"}
	importAliasDevice  = []string{"device_token", "devicetoken"}
	// 导出文件里的运维元数据（分组 / 名称备注）：导入时按条回填，实现往返不丢。
	importAliasGroup = []string{"group", "groupname", "group_name"}
	importAliasNote  = []string{"note", "remark", "comment"}
)

// importContainerKeys 包裹对象的候选键（按顺序找第一个数组值）。
var importContainerKeys = []string{"accounts", "data", "items", "list", "result"}

// importCockpit 接收多种来源的账号 JSON 文件，批量导入账号到池中。
//
//	POST /panel/api/import/cockpit
//	Content-Type: multipart/form-data
//	Body: file=<json>
//
// 路由名沿用 cockpit（前端已固定），但内容不再只认 cockpit tools 的导出格式：
// 顶层数组 / 单个账号对象 / {accounts|data|items|list|result: [...]} 包裹均可，
// 字段按 importAlias* 别名字表取值。返回 {ok, total, imported, skipped, errors, group}。
func (p *Panel) importCockpit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	// group 可选：本次导入成功的账号统一归到该组（空 = 不动已有分组）。
	group := strings.TrimSpace(r.FormValue("group"))
	file, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing file field: "+err.Error())
		return
	}
	defer file.Close()

	raw, err := io.ReadAll(file)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read file: "+err.Error())
		return
	}

	accounts, err := parseImportAccounts(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(accounts) == 0 {
		writeErr(w, http.StatusBadRequest, "empty accounts array")
		return
	}

	var total, imported, skipped int
	var errs []string

	for i, acc := range accounts {
		uid := importString(acc, importAliasUID)
		at := importString(acc, importAliasAccess)
		rt := importString(acc, importAliasRefresh)
		// uid 与 access_token 必填；refresh_token 允许为空（静态 token 类账号），
		// 空 refresh 照常落盘，只是后续刷新会失败。
		if uid == "" {
			skipped++
			errs = append(errs, fmt.Sprintf("#%d: missing uid", i+1))
			continue
		}
		if at == "" {
			skipped++
			errs = append(errs, fmt.Sprintf("#%d: missing access_token (uid=%s)", i+1, uid))
			continue
		}
		if !validImportUID(uid) {
			skipped++
			errs = append(errs, fmt.Sprintf("#%d: invalid uid (id=%s)", i+1, uid))
			continue
		}

		domain := importString(acc, importAliasDomain)
		// realm 显式值优先于 domain 推断（cn / global / intl，intl 归一为 global）。
		explicitRealm := normalizeImportRealm(importString(acc, importAliasRealm))
		realm := auth.ResolveRealm(explicitRealm, domain)

		// expires 支持毫秒/秒时间戳（数字或数字字符串）与 RFC3339 时间字符串；
		// 非法或缺失按一年后兜底（与原逻辑一致）。
		expiresAt := importExpiresAt(acc)
		if expiresAt <= 0 {
			expiresAt = time.Now().Add(365 * 24 * time.Hour).Unix()
		}

		nickname := importString(acc, importAliasNick)
		if nickname == "" {
			nickname = importString(acc, importAliasEmail)
		}

		a := &auth.Auth{
			AccessToken:  at,
			RefreshToken: rt,
			ExpiresAt:    expiresAt,
			Domain:       domain,
			UID:          uid,
			Nickname:     nickname,
			DeviceToken:  importString(acc, importAliasDevice),
			FilePath:     filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", uid)),
		}

		if explicitRealm != "" {
			if _, err := auth.BackfillRealmFor(a, explicitRealm); err != nil {
				skipped++
				errs = append(errs, fmt.Sprintf("uid=%s: set realm failed: %v", uid, err))
				continue
			}
		} else {
			// 未显式给 realm：沿用原逻辑，按 domain 推断并落盘。
			_, _ = a.BackfillRealm()
		}

		if err := a.SaveAtomic(); err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("uid=%s: save auth failed: %v", uid, err))
			continue
		}

		isNewAccount := p.cfg.Pool.AuthByUID(uid) == nil
		// 先取原分组/原备注：SetMeta 是整条覆盖，直接传空会把用户手写的元数据清掉。
		prevNote, prevGroup := "", ""
		if st, ok := p.cfg.Pool.Status(uid); ok {
			prevNote, prevGroup = st.Note, st.Group
		}
		p.cfg.Pool.Add(a)
		p.cfg.Pool.Revive(uid)
		// 分组来源优先级：表单 group > 单条记录里的 group > 保留原分组。
		// 备注同构：单条记录里的 note 覆盖，缺省保留原备注。
		// 这样导出文件可以直接再导入（往返不丢分组/备注）。
		wantGroup, wantNote := group, prevNote
		if wantGroup == "" {
			if g := importString(acc, importAliasGroup); g != "" {
				wantGroup = g
			}
		}
		if n := importString(acc, importAliasNote); n != "" {
			wantNote = n
		}
		if wantGroup == "" {
			wantGroup = prevGroup
		}
		if wantGroup != prevGroup || wantNote != prevNote {
			p.cfg.Pool.SetMeta(uid, wantGroup, wantNote)
		}

		// 顺带签到/激活（幂等；失败仅记日志，不阻断导入）。
		if realm == "global" {
			if activated, err := p.cfg.Upstream.GlobalCompleteRegistration(a); err != nil {
				log.Printf("panel: import global 注册激活 uid=%s: %v", uid, err)
			} else if activated {
				log.Printf("panel: import global 注册激活 uid=%s 完成", uid)
			}
			if claimed, err := p.cfg.Upstream.ClaimTrial(a); err != nil {
				log.Printf("panel: import global trial uid=%s: %v", uid, err)
			} else if claimed {
				log.Printf("panel: import global trial uid=%s 已领", uid)
			}
		} else {
			if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
				log.Printf("panel: import checkin uid=%s: %v", uid, err)
			}
		}
		if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
			p.cfg.Pool.ReenableIfCredits(uid, rm, tt)
		}

		if isNewAccount {
			p.StartNewAccountTasks(uid)
		}
		imported++
	}

	total = len(accounts)
	log.Printf("panel: cockpit import finished total=%d imported=%d skipped=%d", total, imported, skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"total":    total,
		"imported": imported,
		"skipped":  skipped,
		"errors":   errs,
		"group":    group,
	})
}

// parseImportAccounts 把任意来源的 JSON 解成账号对象数组。每个对象已做键折叠
// （小写）与嵌套 token 子对象合并，后续取值无需再关心原始大小写。
func parseImportAccounts(raw []byte) ([]map[string]any, error) {
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")) // 容忍 Windows 导出带 BOM
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // 保住大整数 uid 的精度（float64 会丢低位）
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}

	switch v := root.(type) {
	case []any:
		return importObjectList(v), nil
	case map[string]any:
		// 包裹对象：按 importContainerKeys 顺序取第一个命中的数组。
		for _, key := range importContainerKeys {
			for k, val := range v {
				if !strings.EqualFold(strings.TrimSpace(k), key) {
					continue
				}
				if arr, ok := val.([]any); ok {
					return importObjectList(arr), nil
				}
			}
		}
		// 没有包裹数组 → 整个对象就是一个账号。
		return []map[string]any{normalizeImportAccount(v)}, nil
	default:
		return nil, fmt.Errorf("unsupported json shape: expected account object or array")
	}
}

// importObjectList 把 []any 逐项转成账号对象；非对象元素保留为空对象，
// 由调用方按「缺 uid」跳过并记错误（总数仍然对得上）。
func importObjectList(list []any) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if obj, ok := item.(map[string]any); ok {
			out = append(out, normalizeImportAccount(obj))
			continue
		}
		out = append(out, map[string]any{})
	}
	return out
}

// normalizeImportAccount 浅合并嵌套子对象并折叠键名。
//
// 合并顺序：先铺开 token / tokens / credentials 子对象的键，再铺顶层键——
// **顶层优先**（同一字段两处都给了值时，以账号对象顶层的值为准）。
// importContainers 常见的「账号/凭证」容器键：值是一个对象，内部字段才真正携带
// uid / token / 到期时间。真实导出格式多为 account+auth 两层（CodeBuddy 桌面端、
// 插件、各类工具），也有把凭证塞在 token/credentials 里的。
// 合并顺序＝从左到右，后者覆盖前者；容器自身的键不参与别名匹配。
var importContainers = []string{
	"account", "user", "profile",
	"auth", "credential", "credentials",
	"token", "tokens",
}

func normalizeImportAccount(obj map[string]any) map[string]any {
	out := make(map[string]any, len(obj)+16)
	// 1) 先按容器顺序摊平（内层字段互相覆盖：auth 的 token 覆盖 account 里的同名字段）
	for _, container := range importContainers {
		for k, v := range obj {
			if strings.ToLower(strings.TrimSpace(k)) != container {
				continue
			}
			sub, ok := v.(map[string]any)
			if !ok {
				continue
			}
			for sk, sv := range sub {
				out[strings.ToLower(strings.TrimSpace(sk))] = sv
			}
		}
	}
	// 2) 顶层字段最后写入 → 顶层永远优先于容器内同名值。
	// 注意：只有「值是对象」的容器键才跳过；顶层 `"token": "at-xxx"` 这种字符串
	// 必须保留（它是 access_token 的合法别名，不能因为键名撞上容器而被吞掉）。
	for k, v := range obj {
		key := strings.ToLower(strings.TrimSpace(k))
		if _, isObj := v.(map[string]any); isObj && isImportContainer(key) {
			continue
		}
		out[key] = v
	}
	return out
}

func isImportContainer(key string) bool {
	for _, c := range importContainers {
		if key == c {
			return true
		}
	}
	return false
}

// importString 按别名顺序取第一个非空字符串值（数字按十进制文本处理）。
func importString(acc map[string]any, aliases []string) string {
	for _, key := range aliases {
		if v, ok := acc[key]; ok {
			if s := importScalarString(v); s != "" {
				return s
			}
		}
	}
	return ""
}

func importScalarString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case json.Number:
		return t.String()
	case float64:
		if math.Trunc(t) == t && math.Abs(t) < 1e15 {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

// importExpiresAt 取 expires 并统一转成 Unix 秒（0 = 未提供/无法解析）。
func importExpiresAt(acc map[string]any) int64 {
	for _, key := range importAliasExpires {
		v, ok := acc[key]
		if !ok {
			continue
		}
		if ts := importExpiryValue(v); ts > 0 {
			return ts
		}
	}
	return 0
}

func importExpiryValue(v any) int64 {
	switch t := v.(type) {
	case json.Number:
		if n, err := t.Float64(); err == nil {
			return importEpochNumber(n)
		}
	case float64:
		return importEpochNumber(t)
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		// RFC3339（带不带纳秒都收）。
		if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return ts.Unix()
		}
		if ts, err := time.Parse(time.RFC3339, s); err == nil {
			return ts.Unix()
		}
		// 数字字符串：毫秒 / 秒同一口径。
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return importEpochNumber(n)
		}
	}
	return 0
}

// importEpochNumber 把时间戳归一为秒：>1e12 视为毫秒（cockpit 导出），否则按秒。
func importEpochNumber(n float64) int64 {
	if n <= 0 {
		return 0
	}
	if n > 1e12 {
		return int64(n / 1000)
	}
	return int64(n)
}

// normalizeImportRealm 归一显式 realm：cn / global / intl(intl → global)。
// 无法识别的值返回空串，交由 domain 推断兜底。
func normalizeImportRealm(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "cn":
		return "cn"
	case "global", "intl", "international":
		return "global"
	}
	return ""
}

// validImportUID 校验导入 uid 是否可用于拼文件名（同 login.go validUID 口径）。
func validImportUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for _, c := range uid {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
