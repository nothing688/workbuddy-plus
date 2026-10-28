// modelverify.go 单模型可用性验证：对指定模型发一条最小对话，回答
// 「目录里列的这个模型，此刻真的能调通吗」。
//
// 与账号可用性测试（probe_chat）的分工：
//   - probe_chat 关心「账号能不能用」，会联动池状态（终态失败禁用 / 成功解冻）；
//   - 本接口只读：不 NoteError、不禁用、不解冻、不复活账号，只回报这一次调用的结果。
//
// 成本：一次 max_tokens=16 的对话，可能产生少量积分扣费，前端点击前会确认。
package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// modelVerifyAttempts 一次验证最多换几个账号：模型在不同账号类型上权限不同
// （11158），单号失败不代表全池不可用；3 个足够覆盖常见混合池。
const modelVerifyAttempts = 3

// modelVerify POST /panel/api/models/verify  body {"model":"cn:glm-5.2"}
//
// → {ok, model, realm, latency_ms, reply, error, account, attempts}
//
// 模型名必须带 cn:/global: 前缀（与面板显示/调用值一致），避免同名模型串域。
func (p *Panel) modelVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	raw := strings.TrimSpace(req.Model)
	if raw == "" {
		writeErr(w, http.StatusBadRequest, "model is required")
		return
	}
	// 必须显式带域前缀：SplitRealmPrefix 对裸名默认回落 cn，靠它无法区分
	// 「用户漏了前缀」和「用户真的要 cn」；同名模型跨域时验证结果会串。
	if !strings.HasPrefix(raw, "cn:") && !strings.HasPrefix(raw, "global:") {
		writeErr(w, http.StatusBadRequest, "model must be prefixed with cn: or global:")
		return
	}
	realm, bare := upstream.SplitRealmPrefix(raw)
	if bare == "" {
		writeErr(w, http.StatusBadRequest, "model is required")
		return
	}
	if p.cfg.Upstream == nil || p.cfg.Pool == nil {
		writeErr(w, http.StatusServiceUnavailable, "upstream client not configured")
		return
	}
	if realm == "global" && !p.cfg.Upstream.GlobalEnabled {
		writeErr(w, http.StatusBadRequest, "global 路由未启用")
		return
	}
	uids := p.cfg.Pool.CatalogUIDsForRealm(realm)
	if len(uids) == 0 {
		writeErr(w, http.StatusServiceUnavailable, "没有账号：请先添加 "+realm+" 域账号")
		return
	}

	resp := map[string]any{
		"ok":         false,
		"model":      realm + ":" + bare,
		"realm":      realm,
		"latency_ms": int64(0),
		"reply":      "",
		"error":      "",
	}
	var attempts []map[string]any

	ctx, cancel := context.WithTimeout(r.Context(), probeChatTimeout)
	defer cancel()
	for i, uid := range uids {
		if i >= modelVerifyAttempts {
			break
		}
		a := p.cfg.Pool.AuthByUID(uid)
		if a == nil {
			continue
		}
		// 临期 token 先刷新（只维护凭证）；失败也继续，由请求本身给结论。
		p.prepareCatalogAuth(a)
		body, err := json.Marshal(map[string]any{
			"model":      bare,
			"messages":   []map[string]any{{"role": "user", "content": "ping"}},
			"max_tokens": 64, // 同 probe_chat：思考型模型会先把小预算吃在 reasoning 上
			"stream":     false,
		})
		if err != nil {
			resp["error"] = "build body: " + err.Error()
			break
		}
		start := time.Now()
		rc, status, respBody, callErr := p.cfg.Upstream.ChatStreamContext(ctx, a, body, "", upstream.ChatMeta{}, nil)
		latency := elapsedMs(start)
		resp["latency_ms"] = latency

		var reply string
		if callErr == nil && status < 400 && rc != nil {
			rawResp, readErr := io.ReadAll(io.LimitReader(rc, 1<<20))
			rc.Close()
			if readErr == nil {
				text, reasoning, perr := probeReply(rawResp)
				if perr != nil {
					callErr = fmt.Errorf("parse reply: %w", perr)
				} else if text != "" {
					reply = text
				} else if reasoning != "" {
					reply = "（仅思考内容，账号可用）" // 思考型模型在 64 token 预算下的常见形态
				} else {
					callErr = fmt.Errorf("upstream reply has no content")
				}
			} else {
				callErr = fmt.Errorf("read reply: %w", readErr)
			}
		} else if rc != nil {
			rc.Close()
		}

		if callErr == nil && status < 400 && reply != "" {
			resp["ok"] = true
			resp["reply"] = truncateReply(reply)
			resp["account"] = p.modelVerifyAccount(uid, a)
			log.Printf("panel: model_verify model=%s uid=%s ok latency=%dms", realm+":"+bare, uid, latency)
			break
		}

		// 失败：记录这次尝试，换下一个账号。模型级权限问题尤其可能换号后成功。
		msg := ""
		switch {
		case callErr != nil:
			msg = callErr.Error()
		case status >= 400:
			msg = fmt.Sprintf("upstream http %d: %s", status, truncateStr(string(respBody), 200))
		default:
			msg = "上游返回空回复"
		}
		// 首错优先：它通常来自主账号（健康号在前），比后面兜底号的信息更有诊断价值；
		// 全部尝试仍完整列在 attempts 里，前端可展开查看。
		if resp["error"] == "" {
			resp["error"] = msg
		}
		attempts = append(attempts, map[string]any{
			"uid":    uid,
			"status": status,
			"error":  msg,
		})
		log.Printf("panel: model_verify model=%s uid=%s 失败: %s", realm+":"+bare, uid, msg)
	}
	if len(attempts) > 0 {
		resp["attempts"] = attempts
	}
	writeJSON(w, http.StatusOK, resp)
}

// modelVerifyAccount 验证命中/尝试账号的展示信息（不暴露 token）。
func (p *Panel) modelVerifyAccount(uid string, a *auth.Auth) map[string]any {
	out := map[string]any{"uid": uid, "nickname": a.Nickname}
	if st, ok := p.cfg.Pool.Status(uid); ok {
		out["note"] = st.Note
		out["disabled"] = st.Disabled
		out["fallback"] = st.Disabled || st.Cooling || st.Paused
	}
	return out
}
