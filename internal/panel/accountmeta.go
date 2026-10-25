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
	// probeChatFallbackModel 拿不到账号模型目录时的兜底模型（各域通用短名）。
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
// revived, group}。
//
// 真的发一条 chat 请求（messages=[user "ping"]、max_tokens 16）来判定账号此刻
// 能不能用。不记成功/失败、不占在途；瞬时失败（429 / 5xx / 网络）不改池状态。
//
// 状态联动只有两条，都是「上游已经给出终态判词」才落地：
//   - 探测成功且账号 disabled → Pool.Revive 解冻并回归原分组（移入 DisabledGroup
//     时记的书签见 pool 分组语义），响应 revived=true + group；这是「确认可用 → 自动恢复」。
//   - 探测命中终态故障（11140 账号级风控 / 12153 session dead）→ Pool.Disable，
//     响应 disabled=true + disabled_reason，避免「测试说不可用、池子照样选它」。
//     11-128 渠道校验、限流、网关 401（无 12153 信封）都不算终态，不动池。
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
	bare := probeBareModel(req.Model)
	if bare == "" {
		bare = p.probeDefaultModel(a)
	}
	resp := map[string]any{
		"ok":         false,
		"model":      realm + ":" + bare,
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
		writeJSON(w, http.StatusOK, resp)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), probeChatTimeout)
	defer cancel()
	start := time.Now()
	rc, status, respBody, callErr := p.cfg.Upstream.ChatStreamContext(ctx, a, body, "", upstream.ChatMeta{}, nil)
	if callErr != nil {
		if rc != nil {
			rc.Close()
		}
		resp["latency_ms"] = elapsedMs(start)
		resp["error"] = callErr.Error()
		if disabled, reason := p.probeDisableIfTerminal(uid, status, string(respBody), callErr); disabled {
			resp["disabled"] = true
			resp["disabled_reason"] = reason
		}
		log.Printf("panel: probe_chat uid=%s model=%s 失败: %v", uid, bare, callErr)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if status >= 400 {
		if rc != nil {
			rc.Close()
		}
		resp["latency_ms"] = elapsedMs(start)
		resp["error"] = fmt.Sprintf("upstream http %d: %s", status, truncateStr(string(respBody), 200))
		if disabled, reason := p.probeDisableIfTerminal(uid, status, string(respBody), nil); disabled {
			resp["disabled"] = true
			resp["disabled_reason"] = reason
		}
		log.Printf("panel: probe_chat uid=%s model=%s http=%d", uid, bare, status)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if rc == nil {
		resp["latency_ms"] = elapsedMs(start)
		resp["error"] = "upstream returned no body"
		writeJSON(w, http.StatusOK, resp)
		return
	}

	raw, readErr := io.ReadAll(io.LimitReader(rc, 1<<20))
	rc.Close()
	latency := elapsedMs(start)
	resp["latency_ms"] = latency
	if readErr != nil {
		resp["error"] = "read reply: " + readErr.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	reply, perr := probeReplyText(raw)
	if perr != nil {
		resp["error"] = perr.Error()
		log.Printf("panel: probe_chat uid=%s model=%s 解析失败: %v", uid, bare, perr)
		writeJSON(w, http.StatusOK, resp)
		return
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

// probeDefaultModel 未指定模型时取该账号可用模型列表的第一个；拿不到目录就兜底
// probeChatFallbackModel。目录查询失败不影响探测本身（模型名是短名，各域通用）。
func (p *Panel) probeDefaultModel(a *auth.Auth) string {
	if p.cfg.Upstream != nil {
		var infos []upstream.ModelInfo
		if a.IsGlobal() {
			infos = p.cfg.Upstream.FetchGlobalModelInfos(a)
		} else if got, err := p.cfg.Upstream.FetchModels(a); err == nil {
			infos = got
		}
		for _, mi := range infos {
			if id := strings.TrimSpace(mi.ID); id != "" {
				return id
			}
		}
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
