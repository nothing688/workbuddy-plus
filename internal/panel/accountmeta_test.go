package panel

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// doAccountMetaRequest 带面板密钥发一次请求（全部用例都走 withAuth 后面的真实路由）。
func doAccountMetaRequest(t *testing.T, p *Panel, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// probeJSONResponse 构造固定 JSON 回包（假上游用）。
func probeJSONResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// probeSSEResponse 构造 SSE 回包（upstream 强制 stream:true，生产回包就是这个形态）。
func probeSSEResponse(payload string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(payload)),
	}
}

// newAccountMetaPool 建一个带单个 cn 账号的池（probe 用例共用）。
func newAccountMetaPool(t *testing.T) *pool.Pool {
	t.Helper()
	pl := pool.New("")
	t.Cleanup(pl.Close)
	pl.Add(&auth.Auth{
		UID:          "u1",
		AccessToken:  "at",
		RefreshToken: "rt",
		ExpiresAt:    9999999999,
		Domain:       "www.codebuddy.cn",
	})
	return pl
}

// ---------------------------------------------------------------------------
// meta / groups
// ---------------------------------------------------------------------------

// TestAccountSetMeta 覆盖写成功与未知 UID 404（分组/备注两端空白被裁掉）。
func TestAccountSetMeta(t *testing.T) {
	pl := newAccountMetaPool(t)
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pl})

	rec := doAccountMetaRequest(t, pn, "POST", "/panel/api/accounts/u1/meta",
		`{"group":"  teamA  ","note":"  主号，勿动  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != true {
		t.Fatalf("resp=%v, want ok:true", resp)
	}
	st, ok := pl.Status("u1")
	if !ok || st.Group != "teamA" || st.Note != "主号，勿动" {
		t.Fatalf("status group/note=%q/%q ok=%v", st.Group, st.Note, ok)
	}

	// 只传 group：note 必须保持原值（SetMeta 整条覆盖，别把用户备注清掉）。
	rec = doAccountMetaRequest(t, pn, "POST", "/panel/api/accounts/u1/meta", `{"group":"teamB"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("partial code=%d body=%s", rec.Code, rec.Body.String())
	}
	if st, _ := pl.Status("u1"); st.Group != "teamB" || st.Note != "主号，勿动" {
		t.Fatalf("partial update group/note=%q/%q, want teamB/主号，勿动", st.Group, st.Note)
	}

	// 未知 UID：404 + 固定文案（前端按此分支提示）。
	rec = doAccountMetaRequest(t, pn, "POST", "/panel/api/accounts/nope/meta",
		`{"group":"teamA","note":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown uid code=%d want 404; body=%s", rec.Code, rec.Body.String())
	}
	var errResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatal(err)
	}
	if errResp["error"] != "account not found" {
		t.Fatalf("error=%v, want account not found", errResp["error"])
	}

	// 坏 JSON 必须 400：静默当成空值会清掉用户已有分组/备注。
	rec = doAccountMetaRequest(t, pn, "POST", "/panel/api/accounts/u1/meta", `{"group":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json code=%d want 400; body=%s", rec.Code, rec.Body.String())
	}
	if st, _ := pl.Status("u1"); st.Group != "teamB" || st.Note != "主号，勿动" {
		t.Fatalf("bad json 后分组/备注被改：%q/%q", st.Group, st.Note)
	}
}

// TestGroupsAggregation 分组列表直接透出 pool.GroupCounts（含未分组桶与 health 口径）。
//
// 禁用会把账号移入 pool.DisabledGroup（书签记原组），故 teamA 的 Total 只剩未禁用的
// 1 个，被禁用的 u2 单独计入「已禁用」桶（Healthy=0）。
func TestGroupsAggregation(t *testing.T) {
	pl := pool.New("")
	t.Cleanup(pl.Close)
	for _, uid := range []string{"u1", "u2", "u3", "u4"} {
		pl.Add(&auth.Auth{UID: uid})
	}
	pl.SetMeta("u1", "teamA", "")
	pl.SetMeta("u2", "teamA", "")
	pl.SetMeta("u3", "teamB", "")
	pl.Disable("u2", "401") // u2 移入「已禁用」，teamA 只剩 1 个健康号

	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pl})
	rec := doAccountMetaRequest(t, pn, "GET", "/panel/api/groups", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Groups []pool.GroupInfo `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	want := []pool.GroupInfo{
		{Name: "teamA", Total: 1, Healthy: 1},
		{Name: "teamB", Total: 1, Healthy: 1},
		{Name: pool.DisabledGroup, Total: 1, Healthy: 0},
		{Name: "", Total: 1, Healthy: 1},
	}
	if len(resp.Groups) != len(want) {
		t.Fatalf("groups=%+v, want %+v", resp.Groups, want)
	}
	for i := range want {
		if resp.Groups[i] != want[i] {
			t.Fatalf("groups[%d]=%+v, want %+v (all=%+v)", i, resp.Groups[i], want[i], resp.Groups)
		}
	}

	// 禁用书签：原组记在 PrevGroup，Revive 后回归 teamA（分组聚合随之恢复）。
	st, ok := pl.Status("u2")
	if !ok || st.Group != pool.DisabledGroup || !st.GroupParked || st.PrevGroup != "teamA" {
		t.Fatalf("禁用后 group=%q parked=%v prev=%q, want %q/true/teamA",
			st.Group, st.GroupParked, st.PrevGroup, pool.DisabledGroup)
	}
	if !pl.Revive("u2") {
		t.Fatal("Revive(u2)=false")
	}
	st, _ = pl.Status("u2")
	if st.Group != "teamA" || st.GroupParked || st.PrevGroup != "" || st.Disabled {
		t.Fatalf("Revive 后 group=%q parked=%v prev=%q disabled=%v, want teamA/false/\"\"/false",
			st.Group, st.GroupParked, st.PrevGroup, st.Disabled)
	}
	// 回归后 teamA 又是 2 个（u2 已解冻）。
	rec = doAccountMetaRequest(t, pn, "GET", "/panel/api/groups", "")
	resp.Groups = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, g := range resp.Groups {
		if g.Name == "teamA" {
			found = true
			if g.Total != 2 || g.Healthy != 2 {
				t.Fatalf("Revive 后 teamA=%+v, want total/healthy=2/2", g)
			}
		}
		if g.Name == pool.DisabledGroup {
			t.Fatalf("Revive 后不应再有「已禁用」桶：%+v", resp.Groups)
		}
	}
	if !found {
		t.Fatalf("Revive 后缺少 teamA：%+v", resp.Groups)
	}
}

// ---------------------------------------------------------------------------
// 导入带分组
// ---------------------------------------------------------------------------

// TestImportCockpitAppliesGroupKeepsNote 导入时的 group 只覆盖分组，不能清掉已有备注。
func TestImportCockpitAppliesGroupKeepsNote(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			// 签到/余额等旁路调用统一给一个无害信封：导入流程对它们的失败只记日志。
			return probeJSONResponse(200, `{"code":0,"data":{}}`), nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
	pl := pool.New("")
	t.Cleanup(pl.Close)
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "old-at", RefreshToken: "old-rt", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	if !pl.SetMeta("u1", "", "手写备注，别清") {
		t.Fatal("SetMeta faield")
	}

	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pl, Upstream: up, AuthDir: t.TempDir()})

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("group", "  teamA  "); err != nil {
		t.Fatal(err)
	}
	fw, err := mw.CreateFormFile("file", "accounts.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(`[{"uid":"u1","access_token":"new-at","refresh_token":"new-rt","domain":"www.codebuddy.cn"}]`)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/panel/api/import/cockpit", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Imported int    `json:"imported"`
		Group    string `json:"group"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Imported != 1 || resp.Group != "teamA" {
		t.Fatalf("resp=%+v, want imported=1 group=teamA", resp)
	}
	st, ok := pl.Status("u1")
	if !ok || st.Group != "teamA" {
		t.Fatalf("status group=%q ok=%v, want teamA", st.Group, ok)
	}
	if st.Note != "手写备注，别清" {
		t.Fatalf("note=%q, 导入不得清空已有备注", st.Note)
	}
}

// ---------------------------------------------------------------------------
// probe_chat
// ---------------------------------------------------------------------------

// TestAccountProbeChatNotFound 未知 UID 走 404（不是 ok:false 的 200）。
func TestAccountProbeChatNotFound(t *testing.T) {
	pl := newAccountMetaPool(t)
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pl, Upstream: &upstream.Client{}})

	rec := doAccountMetaRequest(t, pn, "POST", "/panel/api/accounts/nope/probe_chat", `{"model":"cn:hy3"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["error"] != "account not found" {
		t.Fatalf("error=%v, want account not found", resp["error"])
	}
}

// TestAccountProbeChatSuccess 真发一条最小对话：断言出站 body、回包字段与「不动池状态」。
func TestAccountProbeChatSuccess(t *testing.T) {
	var (
		mu      sync.Mutex
		gotBody map[string]any
	)
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/chat/completions") {
				raw, _ := io.ReadAll(r.Body)
				var obj map[string]any
				_ = json.Unmarshal(raw, &obj)
				mu.Lock()
				gotBody = obj
				mu.Unlock()
				return probeSSEResponse(
					"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"hy3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"pong\"}}]}\n\n" +
						"data: [DONE]\n\n"), nil
			}
			return probeJSONResponse(500, `{"code":500,"msg":"no models"}`), nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
	pl := newAccountMetaPool(t)
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pl, Upstream: up})

	rec := doAccountMetaRequest(t, pn, "POST", "/panel/api/accounts/u1/probe_chat", `{"model":"cn:hy3"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK        bool   `json:"ok"`
		Model     string `json:"model"`
		LatencyMS int64  `json:"latency_ms"`
		Reply     string `json:"reply"`
		Error     string `json:"error"`
		Revived   bool   `json:"revived"`
		Group     string `json:"group"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Model != "cn:hy3" || resp.Reply != "pong" || resp.Error != "" {
		t.Fatalf("resp=%+v, want ok/model=cn:hy3/reply=pong", resp)
	}
	// 本来就可用的号：探测成功不得「复活」，分组原样回显（此号未分组 = 空串）。
	if resp.Revived {
		t.Fatalf("resp=%+v, 可用账号不该 revived", resp)
	}
	if resp.Group != "" {
		t.Fatalf("group=%q, want 空（账号未分组）", resp.Group)
	}
	if resp.LatencyMS < 0 {
		t.Fatalf("latency_ms=%d 必须非负", resp.LatencyMS)
	}

	mu.Lock()
	body := gotBody
	mu.Unlock()
	if body == nil {
		t.Fatal("上游未收到 chat 请求")
	}
	if body["model"] != "hy3" {
		t.Fatalf("出站 model=%v, 必须是裸名 hy3（前端传的是 cn:hy3）", body["model"])
	}
	if mt, ok := body["max_tokens"].(float64); !ok || mt != 16 {
		t.Fatalf("出站 max_tokens=%v, want 16", body["max_tokens"])
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("出站 messages=%v, want 1 条", body["messages"])
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "ping" {
		t.Fatalf("出站消息=%v, want user/ping", first)
	}

	// 只读探测：成功也不改池状态（不记成功、不占在途）。
	if st, _ := pl.Status("u1"); st.SuccessCount != 0 || st.ErrTotal != 0 || st.Cooling || st.Disabled {
		t.Fatalf("探测改动了池状态：%+v", st)
	}
}

// TestAccountProbeChatRevivesDisabled 探测确认可用 → 禁用号自动解冻并回归原分组；
// 响应带 revived=true + 恢复后的分组。
func TestAccountProbeChatRevivesDisabled(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/chat/completions") {
				return probeSSEResponse(
					"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"pong\"}}]}\n\n" +
						"data: [DONE]\n\n"), nil
			}
			return probeJSONResponse(500, `{"code":500,"msg":"no models"}`), nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
	pl := newAccountMetaPool(t)
	if !pl.SetMeta("u1", "teamA", "") {
		t.Fatal("SetMeta failed")
	}
	pl.Disable("u1", "401")
	if st, _ := pl.Status("u1"); !st.Disabled || st.Group != pool.DisabledGroup || st.PrevGroup != "teamA" {
		t.Fatalf("前置失败: disabled=%v group=%q prev=%q", st.Disabled, st.Group, st.PrevGroup)
	}
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pl, Upstream: up})

	rec := doAccountMetaRequest(t, pn, "POST", "/panel/api/accounts/u1/probe_chat", `{"model":"cn:hy3"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK      bool   `json:"ok"`
		Revived bool   `json:"revived"`
		Group   string `json:"group"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || !resp.Revived || resp.Group != "teamA" {
		t.Fatalf("resp=%+v, want ok/revived/group=teamA", resp)
	}
	st, ok := pl.Status("u1")
	if !ok || st.Disabled {
		t.Fatalf("探测成功后仍禁用：%+v", st)
	}
	if st.Group != "teamA" || st.GroupParked || st.PrevGroup != "" {
		t.Fatalf("恢复后 group=%q parked=%v prev=%q, want teamA/false/\"\"",
			st.Group, st.GroupParked, st.PrevGroup)
	}
}

// TestAccountProbeChatFailureKeepsDisabled 探测失败 → 保持禁用与原分组书签，
// 响应 ok:false 且 revived:false（失败不得顺手解冻）。
func TestAccountProbeChatFailureKeepsDisabled(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			return probeJSONResponse(500, `{"code":500,"msg":"boom"}`), nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
	pl := newAccountMetaPool(t)
	pl.SetMeta("u1", "teamA", "")
	pl.Disable("u1", "401")
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pl, Upstream: up})

	rec := doAccountMetaRequest(t, pn, "POST", "/panel/api/accounts/u1/probe_chat", `{"model":"cn:hy3"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK      bool   `json:"ok"`
		Revived bool   `json:"revived"`
		Group   string `json:"group"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Revived {
		t.Fatalf("resp=%+v, want ok:false revived:false", resp)
	}
	if resp.Group != pool.DisabledGroup {
		t.Fatalf("group=%q, want %q（失败不改分组）", resp.Group, pool.DisabledGroup)
	}
	st, _ := pl.Status("u1")
	if !st.Disabled || st.Group != pool.DisabledGroup || !st.GroupParked || st.PrevGroup != "teamA" {
		t.Fatalf("探测失败后状态被改：disabled=%v group=%q parked=%v prev=%q",
			st.Disabled, st.Group, st.GroupParked, st.PrevGroup)
	}
}

// TestAccountProbeChatFailureKeepsPoolClean 上游失败 → 200 + ok:false，且账号不冷却/不计错。
func TestAccountProbeChatFailureKeepsPoolClean(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			return probeJSONResponse(500, `{"code":500,"msg":"boom"}`), nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
	pl := newAccountMetaPool(t)
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pl, Upstream: up})

	rec := doAccountMetaRequest(t, pn, "POST", "/panel/api/accounts/u1/probe_chat", `{"model":"cn:hy3"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200（探测失败也走 200 + ok:false）; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK {
		t.Fatal("ok=true, want false")
	}
	if !strings.Contains(resp.Error, "500") {
		t.Fatalf("error=%q, 应包含上游状态码", resp.Error)
	}
	st, _ := pl.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.SuccessCount != 0 {
		t.Fatalf("探测失败不得改动池状态：%+v", st)
	}
}

// TestAccountProbeChatDefaultModelFallback 未指定模型且目录查询失败 → 兜底 hy3，
// 回显仍带该账号 realm 前缀。
func TestAccountProbeChatDefaultModelFallback(t *testing.T) {
	var (
		mu      sync.Mutex
		gotBody map[string]any
	)
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/chat/completions") {
				raw, _ := io.ReadAll(r.Body)
				var obj map[string]any
				_ = json.Unmarshal(raw, &obj)
				mu.Lock()
				gotBody = obj
				mu.Unlock()
				return probeSSEResponse(
					"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"pong\"}}]}\n\n" +
						"data: [DONE]\n\n"), nil
			}
			// 模型目录两路都失败 → probe 兜底常量。
			return probeJSONResponse(500, `{"code":500,"msg":"no column"}`), nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
	pl := newAccountMetaPool(t)
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pl, Upstream: up})

	// 空 body：model 缺省，且不能因此报 400。
	rec := doAccountMetaRequest(t, pn, "POST", "/panel/api/accounts/u1/probe_chat", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Model string `json:"model"`
		Reply string `json:"reply"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Model != "cn:"+probeChatFallbackModel || resp.Reply != "pong" {
		t.Fatalf("resp=%+v, want ok model=cn:%s", resp, probeChatFallbackModel)
	}
	mu.Lock()
	body := gotBody
	mu.Unlock()
	if body == nil || body["model"] != probeChatFallbackModel {
		t.Fatalf("出站 model=%v, want %s", body, probeChatFallbackModel)
	}
}

// probeChatForTest 跑一次 probe_chat 并解出前端关心的字段。
func probeChatForTest(t *testing.T, status int, body string) (pool.Status, map[string]any) {
	t.Helper()
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			return probeJSONResponse(status, body), nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
	pl := newAccountMetaPool(t)
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pl, Upstream: up})
	rec := doAccountMetaRequest(t, pn, "POST", "/panel/api/accounts/u1/probe_chat", `{"model":"cn:hy3"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	st, ok := pl.Status("u1")
	if !ok {
		t.Fatal("account disappeared")
	}
	return st, resp
}

// TestAccountProbeChatDisablesOn11140 探测命中 11140 账号级风控 → 直接禁用。
// 否则「测试说不可用、池子照样选它」，用户看到的就是这条不一致。
func TestAccountProbeChatDisablesOn11140(t *testing.T) {
	st, resp := probeChatForTest(t, 403, `{"code":11140,"msg":"request illegal","zh":"账号存在异常行为，已暂停"}`)
	if resp["ok"] == true {
		t.Fatalf("ok=true, want false: %+v", resp)
	}
	if resp["disabled"] != true {
		t.Fatalf("11140 应自动禁用: %+v", resp)
	}
	reason, _ := resp["disabled_reason"].(string)
	if !strings.Contains(reason, "11140") || !strings.Contains(reason, "账号存在异常行为") {
		t.Fatalf("禁用原因应带 11140 + 上游 zh 文案，得到 %q", reason)
	}
	if !st.Disabled || !strings.Contains(st.DisabledReason, "11140") {
		t.Fatalf("池状态未禁用: disabled=%v reason=%q", st.Disabled, st.DisabledReason)
	}
}

// TestAccountProbeChatChannelCheckKeepsPool 11-128 渠道校验是请求级可重试信号，
// 处罚账号等于误杀：探测不许改池状态。
func TestAccountProbeChatChannelCheckKeepsPool(t *testing.T) {
	st, resp := probeChatForTest(t, 400, `{"code":"11-128","msg":"request illegal: first message is not system prompt"}`)
	if resp["disabled"] == true {
		t.Fatalf("11-128 不该禁用: %+v", resp)
	}
	if st.Disabled || st.Cooling {
		t.Fatalf("11-128 改动了池状态: %+v", st)
	}
}

// TestAccountProbeChatDisablesOnSessionDead 12153 是登录态终态，探测命中即禁用。
func TestAccountProbeChatDisablesOnSessionDead(t *testing.T) {
	st, resp := probeChatForTest(t, 401, `{"code":12153,"msg":"offline session"}`)
	if resp["disabled"] != true {
		t.Fatalf("12153 应自动禁用: %+v", resp)
	}
	if !st.Disabled {
		t.Fatalf("池状态未禁用: %+v", st)
	}
}

// TestAccountProbeChatRateLimitKeepsPool 429 限流是瞬时故障：探测只报结论，不动池。
func TestAccountProbeChatRateLimitKeepsPool(t *testing.T) {
	st, resp := probeChatForTest(t, 429, `{"code":429,"msg":"rate limit"}`)
	if resp["disabled"] == true {
		t.Fatalf("429 不该禁用: %+v", resp)
	}
	if st.Disabled {
		t.Fatalf("429 不该禁用账号: %+v", st)
	}
}
