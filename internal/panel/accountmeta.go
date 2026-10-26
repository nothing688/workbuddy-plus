// accountmeta.go 账号「分组 / 备注 / 可用性测试」的面板后端。
//
// 三件事各自独立：
//   - 分组 + 备注是运维自定义元数据（pool.SetMeta 落盘），面板只管读写；
//   - 分组列表直接透出 pool.GroupCounts 的聚合结果，不在面板侧重算；
//   - 可用性测试是**只读探测**：真发一条最小 chat 请求拿回一句话，除「探测成功
//     且账号当前 disabled」会调 Pool.Revive 解冻并回归原分组外，全程不碰池状态
//     （不记失败、不冷却、不占在途）——探测失败只说明"这次不通"，不该让运维点
//     一下测试就把账号判死或让号退出选号。
package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

const (
	// probeChatTimeout 单次可用性测试的上游超时。面板是交互式操作，
	// 30s 足够跑完一条 16 token 的问答；再久只会让前端干等。
	probeChatTimeout = 30 * time.Second
	// probeChatMaxReply 回显正文的最大字符数（按 rune 截，不切碎中文）。
	probeChatMaxReply = 200
	// probeChatFallbackModel 可用性测试的首选模型 / 目录不可用时的兜底（各域通用短名）。
	probeChatFallbackModel = "hy3"
	// probeChatRefreshSkew 探测前的 token 新鲜度要求：与请求路径同口径，
	// 快过期时先刷一次，避免明明账号可用却因为 token 过期报失败。
	probeChatRefreshSkew = 2 * time.Minute
)

// accountSetMeta POST /panel/api/accounts/{uid}/meta
//
// body {"group":"teamA","note":"主号"}；两字段都 TrimSpace、空串 = 清除。
// 未出现的字段保持原值（SetMeta 是整条覆盖，用指针区分"没传"与"传空"，否则
// 只想改分组的调用会把备注一起清掉）。未知 UID → 404（与其它单号运维接口同口径）。
func (p *Panel) accountSetMeta(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	st, ok := p.cfg.Pool.Status(uid)
	if !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	var req struct {
		Group *string `json:"group"`
		Note  *string `json:"note"`
	}
	// 坏 JSON 必须报错而不是当成空值：空值会把用户已有分组/备注清掉。
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	group, note := st.Group, st.Note
	if req.Group != nil {
		group = *req.Group
	}
	if req.Note != nil {
		note = *req.Note
	}
	if !p.cfg.Pool.SetMeta(uid, group, note) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	log.Printf("panel: meta uid=%s group=%q", uid, strings.TrimSpace(group))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// groupsList GET /panel/api/groups
//
// 直接序列化 pool.GroupCounts（Name 空串 = 未分组，排在最后），面板侧不重算，
// 保证与按组选号的成员口径完全一致。
func (p *Panel) groupsList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"groups": p.cfg.Pool.GroupCounts()})
}

// groupsCreate POST /panel/api/groups
//
// body {"name":"teamA"} → {"ok":true,"name":"teamA","groups":[...]}。
// 允许创建暂无账号的空分组（持久化在 state.json 的 groups 字段），
// 之后可在导入、账号名称/分组弹框、移动分组里选到它。名字为空返回 400。
func (p *Panel) groupsCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name required")
		return
	}
	if !p.cfg.Pool.CreateGroup(name) {
		writeErr(w, http.StatusBadRequest, "invalid name")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "name": name, "groups": p.cfg.Pool.GroupCounts(),
	})
}

// probeDisableIfTerminal 把探测到的「终态」失败落到池状态。只有上游明确判死才禁用：
//   - 11140 request illegal（且不是 11-128 渠道校验）→ 账号级风控，只能申诉
//   - 12153 session dead → 登录态失效，需重新登录
//
// 限流 429、5xx、网络超时、WAF 403、没有 12153 信封的网关 401 一律不动池：
// 探测是单次观测，瞬时故障不该让号退出选号（网关 401 的误杀复盘见 pool.NoteSessionDead）。
func (p *Panel) probeDisableIfTerminal(uid string, status int, body string, callErr error) (bool, string) {
	var kind upstream.ErrKind
	msg := body
	var ue *upstream.Error
	if errors.As(callErr, &ue) {
		kind, msg = ue.Kind, ue.Msg
		if status == 0 {
			status = ue.Status
		}
	} else if status > 0 {
		kind = upstream.Classify(status, body)
	}
	switch kind {
	case upstream.ErrAccountFault:
		if !strings.Contains(strings.ToLower(msg), "request illegal") {
			return false, ""
		}
		if upstream.HasCodeMarker(msg, "11-128") {
			return false, "" // 渠道校验：请求级可重试，罚号等于误杀
		}
		reason := "11140 账号级风控（request illegal，可用性测试判定）"
		if zh := upstream.ZhText(msg); zh != "" {
			reason = "11140 " + zh + "（可用性测试判定，需在 WorkBuddy 内提交反馈申诉解封）"
		}
		p.cfg.Pool.Disable(uid, reason)
		log.Printf("panel: probe_chat uid=%s → 上游判死，已自动禁用", uid)
		return true, reason
	case upstream.ErrSessionDead:
		if !upstream.HasCodeMarker(msg, "12153") {
			return false, ""
		}
		reason := "上游 12153 session dead（可用性测试判定，需重新登录）"
		p.cfg.Pool.Disable(uid, reason)
		log.Printf("panel: probe_chat uid=%s → 登录态失效，已自动禁用", uid)
		return true, reason
	}
	return false, ""
}

// accountProbeChat POST /panel/api/accounts/{uid}/probe_chat
//
// body {"model":"cn:hy3"}（可选）→ {ok, model, latency_ms, reply, error,
// revived, group, attempts?}。
//
// 真的发一条 chat 请求（messages=[user "ping"]、max_tokens 16）来判定账号此刻
// 能不能用。不记成功/失败、不占在途；瞬时失败（429 / 5xx / 网络）不改池状态。
//
// 模型选择：调用方显式给 model 就只测它；没给则按 hy3 → 该账号目录前两个的顺序
// 逐个尝试，遇到模型级失败（11102/11158/14401/6004/14003）继续换下一个，避免把
// 「这个模型不可用」误判成「账号不可用」。attempts 只在发生过换模型时出现。
//
// 状态联动只有两条，都是「上游已经给出终态判词」才落地：
//   - 探测成功且账号 disabled → Pool.Revive 解冻并回归原分组（移入 DisabledGroup
//     时记的书签见 pool 分组语义），响应 revived=true + group；这是「确认可用 → 自动恢复」。
//   - 探测命中终态故障（11140 账号级风控 / 12153 session dead）→ Pool.Disable，
//     响应 disabled=true + disabled_reason，避免「测试说不可用、池子照样选它」。
//     11-128 渠道校验、限流、网关 401（无 12153 信封）都不算终态，不动池。
//
// 探测失败仍是 200 + ok:false，由前端统一渲染错误文案（与 checkin 的非致命错误同风格）。
func (p *Panel) accountProbeChat(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	st, ok := p.cfg.Pool.Status(uid)
	if !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account credentials not found")
		return
	}

	// model 是可选字段：空 body / 坏 JSON 都按「未指定」处理（前端可能直接 POST 空体）。
	var req struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	realm := a.Realm()
	explicit := probeBareModel(req.Model)
	// 未指定模型时先只测 hy3：它是两域通用的免费短名，绝大多数账号第一个就过。
	// 目录只在 hy3 被上游按「模型级」拒绝后才去拉——不再为了选模型先打两次目录请求，
	// 「全部测试」几十个号时能省掉几十次上游调用。
	candidates := []string{probeChatFallbackModel}
	if explicit != "" {
		candidates = []string{explicit}
	}
	resp := map[string]any{
		"ok":         false,
		"model":      realm + ":" + candidates[0],
		"latency_ms": int64(0),
		"reply":      "",
		"error":      "",
		"revived":    false,
		"group":      st.Group,
	}
	if p.cfg.Upstream == nil {
		resp["error"] = "upstream client not configured"
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// 快过期先刷新：探测要看的是账号能不能用，不该被一张临期 token 误判成坏号。
	// 刷新本身会改写 token（并落盘），但这是凭证维护，不是池状态（冷却/禁用/计数）。
	if a.NeedsRefresh(probeChatRefreshSkew) {
		if err := p.cfg.Upstream.RefreshTokenIfNeeded(a, probeChatRefreshSkew); err != nil {
			resp["error"] = "refresh token: " + err.Error()
			writeJSON(w, http.StatusOK, resp)
			return
		}
		if err := a.SaveAtomicRetry(); err != nil {
			log.Printf("panel: probe_chat uid=%s 刷新后落盘失败: %v", uid, err)
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), probeChatTimeout)
	defer cancel()
	var attempts []map[string]any
	catalogLoaded := explicit != ""
	for i := 0; i < len(candidates); i++ {
		bare := candidates[i]
		resp["model"] = realm + ":" + bare
		// 非流式语义的请求体（上游 prepareBody 会强制 stream:true，回包仍是 SSE，
		// 下面按两种形态都能解析）。max_tokens 压到 16：只要一句回复即可判定可用。
		body, err := json.Marshal(map[string]any{
			"model":      bare,
			"messages":   []map[string]any{{"role": "user", "content": "ping"}},
			"max_tokens": 16,
			"stream":     false,
		})
		if err != nil {
			resp["error"] = "build body: " + err.Error()
			break
		}
		start := time.Now()
		rc, status, respBody, callErr := p.cfg.Upstream.ChatStreamContext(ctx, a, body, "", upstream.ChatMeta{}, nil)
		resp["latency_ms"] = elapsedMs(start)

		// 模型级失败不是账号坏：11102/11158/14401（模型不存在/无权限/无路由）与
		// 6004/14003（模型额度节流）都只否掉「这个模型」。用户没点名模型时换下一个
		// 候选继续测，否则会把「某个模型不可用」误判成「整个账号不可用」。
		modelLevel := status >= 400 &&
			(upstream.IsModelBlocked(status, string(respBody)) || upstream.IsModelRateLimit(string(respBody)))
		if modelLevel && explicit == "" {
			if !catalogLoaded {
				catalogLoaded = true
				for _, id := range p.probeCatalogModels(a) {
					if len(candidates) >= probeChatMaxModels {
						break
					}
					dup := false
					for _, c := range candidates {
						if c == id {
							dup = true
							break
						}
					}
					if !dup {
						candidates = append(candidates, id)
					}
				}
			}
			if i+1 < len(candidates) {
				if rc != nil {
					rc.Close()
				}
				attempts = append(attempts, map[string]any{
					"model":  realm + ":" + bare,
					"status": status,
					"error":  truncateStr(string(respBody), 160),
				})
				log.Printf("panel: probe_chat uid=%s model=%s 模型级失败（http=%d），换下一个候选", uid, bare, status)
				continue
			}
		}

		if callErr != nil {
			if rc != nil {
				rc.Close()
			}
			resp["error"] = callErr.Error()
			if disabled, reason := p.probeDisableIfTerminal(uid, status, string(respBody), callErr); disabled {
				resp["disabled"] = true
				resp["disabled_reason"] = reason
			}
			log.Printf("panel: probe_chat uid=%s model=%s 失败: %v", uid, bare, callErr)
			break
		}
		if status >= 400 {
			if rc != nil {
				rc.Close()
			}
			resp["error"] = fmt.Sprintf("upstream http %d: %s", status, truncateStr(string(respBody), 200))
			if disabled, reason := p.probeDisableIfTerminal(uid, status, string(respBody), nil); disabled {
				resp["disabled"] = true
				resp["disabled_reason"] = reason
			}
			log.Printf("panel: probe_chat uid=%s model=%s http=%d", uid, bare, status)
			break
		}
		if rc == nil {
			resp["error"] = "upstream returned no body"
			break
		}

		raw, readErr := io.ReadAll(io.LimitReader(rc, 1<<20))
		rc.Close()
		latency := elapsedMs(start)
		resp["latency_ms"] = latency
		if readErr != nil {
			resp["error"] = "read reply: " + readErr.Error()
			break
		}
		reply, perr := probeReplyText(raw)
		if perr != nil {
			resp["error"] = perr.Error()
			log.Printf("panel: probe_chat uid=%s model=%s 解析失败: %v", uid, bare, perr)
			break
		}
		resp["ok"] = true
		resp["reply"] = truncateReply(reply)
		log.Printf("panel: probe_chat uid=%s model=%s ok latency=%dms", uid, bare, latency)

		// 探测确认可用 → 禁用号自动恢复：Revive 解冻并（有书签时）回归原分组。
		// 先重新读一次状态：探测耗时最长 30s，期间账号可能已被别处禁用/恢复。
		if cur, ok := p.cfg.Pool.Status(uid); ok && cur.Disabled && p.cfg.Pool.Revive(uid) {
			after, _ := p.cfg.Pool.Status(uid)
			resp["revived"] = true
			resp["group"] = after.Group
			log.Printf("panel: probe_chat 恢复 uid=%s group=%s", uid, after.Group)
		}
		break
	}
	if len(attempts) > 0 {
		resp["attempts"] = attempts
	}
	writeJSON(w, http.StatusOK, resp)
}

// probeBareModel 把请求里的 model 归一成上游认识的裸名（剥掉 cn:/global: 前缀）。
// 前缀只是网关路由协议，实际打到哪个域由账号自己的 realm 决定。
func probeBareModel(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	_, bare := upstream.SplitRealmPrefix(name)
	return strings.TrimSpace(bare)
}

// probeChatMaxModels 未指定模型时最多试几个候选：hy3 + 目录里的两个。
// 只有 hy3 被模型级拒绝时才继续，正常号第一个就成功，不会平白多耗额度。
const probeChatMaxModels = 3

// probeCatalogModels 取该账号上游目录里的对话模型 ID（按上游顺序，最多 4 个）。
// 目录失败 / 为空返回 nil；调用方自己决定是否回落 hy3。
func (p *Panel) probeCatalogModels(a *auth.Auth) []string {
	if p.cfg.Upstream == nil || a == nil {
		return nil
	}
	realm := a.Realm()
	// 复用模型页的短缓存：同一轮「全部测试」不会每个禁用号都拉一次目录。
	if ids := p.cachedCatalogIDs(realm); len(ids) > 0 {
		return ids
	}
	// 目录里含出图模型；这里只取对话模型由 probe 的失败重试兜住——模型页的缓存
	// 已经过滤过非对话条目，直接复用即可。
	var infos []upstream.ModelInfo
	if a.IsGlobal() {
		infos = p.cfg.Upstream.FetchGlobalModelInfos(a)
	} else if got, err := p.cfg.Upstream.FetchModels(a); err == nil {
		infos = got
	}
	if len(infos) > 0 {
		p.storeCatalog(realm, catalogResult{acct: a, infos: infos})
	}
	out := make([]string, 0, 4)
	for _, mi := range infos {
		id := strings.TrimSpace(mi.ID)
		if id == "" {
			continue
		}
		dup := false
		for _, x := range out {
			if x == id {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		out = append(out, id)
		if len(out) >= 4 {
			break
		}
	}
	return out
}

// probeCatalogFirstModel 封禁探测用的模型：账号目录的第一个对话模型，目录不可用回落 hy3。
// 与 probe_chat 的「hy3 优先」分开：封禁探测只关心账号能不能发对话，沿用旧口径即可，
// 不让测试模型的策略变化影响封禁判词。
func (p *Panel) probeCatalogFirstModel(a *auth.Auth) string {
	if ids := p.probeCatalogModels(a); len(ids) > 0 {
		return ids[0]
	}
	return probeChatFallbackModel
}

// probeReplyText 从上游响应里取回复正文：先按非流式 chat.completion 解，
// 不成再按 SSE 聚合（upstream 强制 stream:true，生产路径基本走后者）。
func probeReplyText(raw []byte) (string, error) {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) == nil {
		if text := chatCompletionText(obj); text != "" {
			return text, nil
		}
	}
	agg, err := upstream.Aggregate(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("parse upstream reply: %w", err)
	}
	text := chatCompletionText(agg)
	if text == "" {
		return "", fmt.Errorf("upstream reply has no content")
	}
	return text, nil
}

// chatCompletionText 取 choices[0].message.content（Aggregate 与非流式信封同构）。
func chatCompletionText(obj map[string]any) string {
	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	first, _ := choices[0].(map[string]any)
	msg, _ := first["message"].(map[string]any)
	text, _ := msg["content"].(string)
	return text
}

// truncateReply 按字符（rune）截断回显正文，避免把多字节字符切成乱码。
func truncateReply(s string) string {
	r := []rune(s)
	if len(r) <= probeChatMaxReply {
		return s
	}
	return string(r[:probeChatMaxReply])
}

// elapsedMs 毫秒耗时（前端按整数渲染）。
func elapsedMs(start time.Time) int64 { return time.Since(start).Milliseconds() }
