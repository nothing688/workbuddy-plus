// zh.go 上游错误信封里的中文文案。
//
// 上游业务错误（11140 等）的响应体形如
// {"code":11140,"msg":"request illegal","zh":"账号存在异常行为，已暂停"}：
// msg 是给机器/日志的英文，zh 才是官方给用户看的那句。面板的状态列和禁用原因都
// 优先用 zh，运维不用再自己翻译 11140。
package upstream

import (
	"encoding/json"
	"strings"
)

// ZhText 取错误信封里的 zh 字段；没有该字段或不是 JSON 时返回空串。
// 解析失败不当错误——它只用于展示文案。
func ZhText(body string) string {
	if body == "" {
		return ""
	}
	var v struct {
		Zh string `json:"zh"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return ""
	}
	return strings.TrimSpace(v.Zh)
}
