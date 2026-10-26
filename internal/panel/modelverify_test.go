package panel

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// TestModelVerifySuccess 单模型验证：真实发一次对话，成功时回报账号与延迟。
func TestModelVerifySuccess(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			return probeSSEResponse(
				"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"pong\"}}]}\n\n" +
					"data: [DONE]\n\n"), nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
	pl := newAccountMetaPool(t)
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pl, Upstream: up})

	rec := doAccountMetaRequest(t, pn, "POST", "/panel/api/models/verify", `{"model":"cn:glm-5.2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK      bool   `json:"ok"`
		Model   string `json:"model"`
		Realm   string `json:"realm"`
		Reply   string `json:"reply"`
		Account struct {
			UID string `json:"uid"`
		} `json:"account"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Model != "cn:glm-5.2" || resp.Realm != "cn" || resp.Reply != "pong" || resp.Account.UID != "u1" {
		t.Fatalf("resp=%+v", resp)
	}
	// 验证只读：不得改动池状态。
	if st, _ := pl.Status("u1"); st.Disabled || st.Cooling || st.ErrTotal != 0 {
		t.Fatalf("model_verify 改了池状态：%+v", st)
	}
}

// TestModelVerifyTriesNextAccount 模型在第一个账号上无权限（11158）时换下一个号；
// 这在混合账号类型（个人/企业/API Key）的池里很常见。
func TestModelVerifyTriesNextAccount(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.Header.Get("Authorization"), "at-u1") {
				return probeJSONResponse(400, `{"code":"11158","msg":"not allowed for user type"}`), nil
			}
			return probeSSEResponse(
				"data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"pong\"}}]}\n\n" +
					"data: [DONE]\n\n"), nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
	pl := pool.New("")
	t.Cleanup(pl.Close)
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	pl.Add(&auth.Auth{UID: "u2", AccessToken: "at-u2", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pl, Upstream: up})

	rec := doAccountMetaRequest(t, pn, "POST", "/panel/api/models/verify", `{"model":"cn:glm-5.2"}`)
	var resp struct {
		OK       bool             `json:"ok"`
		Account  map[string]any   `json:"account"`
		Attempts []map[string]any `json:"attempts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Account["uid"] != "u2" {
		t.Fatalf("resp=%+v，want u2 成功", resp)
	}
	if len(resp.Attempts) != 1 || resp.Attempts[0]["uid"] != "u1" {
		t.Fatalf("attempts=%v，want 记录 u1 的 11158", resp.Attempts)
	}
}

// TestModelVerifyValidation 模型名必须带域前缀；没有账号时 503。
func TestModelVerifyValidation(t *testing.T) {
	pl := newAccountMetaPool(t)
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pl, Upstream: &upstream.Client{}})
	for _, body := range []string{`{"model":""}`, `{"model":"glm-5.2"}`, `{"model":"xx:glm-5.2"}`} {
		rec := doAccountMetaRequest(t, pn, "POST", "/panel/api/models/verify", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%s code=%d，want 400", body, rec.Code)
		}
	}

	empty := pool.New("")
	t.Cleanup(empty.Close)
	pn2 := New(Config{Version: "test", APIKey: "test-key", Pool: empty, Upstream: &upstream.Client{}})
	rec := doAccountMetaRequest(t, pn2, "POST", "/panel/api/models/verify", `{"model":"cn:glm-5.2"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no account code=%d，want 503", rec.Code)
	}
}
