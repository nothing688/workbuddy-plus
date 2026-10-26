package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// TestPanelModelsIncludesImageModels 面板「模型与档位」必须列出出图模型：
// 它们被目录按 tag（text-to-image）从对话列表剔掉，若不单列，面板上就看不到
// hunyuan-image-alpha / gpt-image-2.5-sunburst —— 用户不知道有哪些出图模型可调。
func TestPanelModelsIncludesImageModels(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			body := `{"code":0,"data":{"models":[` +
				`{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":200000,"maxOutputTokens":32000},` +
				`{"id":"hunyuan-image-alpha","name":"Hunyuan Image Alpha","tags":["text-to-image"]},` +
				`{"id":"seedance-2.5","name":"Seedance-2.5","tags":["text-to-video","image-to-video"]}],` +
				`"agents":[{"name":"cli","models":["glm-5.2"]}]}}`
			if strings.HasSuffix(r.URL.Path, "/v3/config") {
				body = `{"code":0,"data":{"models":[` +
					`{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":200000,"maxOutputTokens":32000},` +
					`{"id":"hunyuan-image-alpha","name":"Hunyuan Image Alpha","tags":["text-to-image"]},` +
					`{"id":"seedance-2.5","name":"Seedance-2.5","tags":["text-to-video"]}]}}`
			}
			return &http.Response{StatusCode: 200,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   io.NopCloser(strings.NewReader(body))}, nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	p.SetCredits("c1", 1000, 0)

	// 目录解析会顺手把出图模型留存在 client 上（面板 API 直接读它，不再打上游）。
	if _, err := up.FetchModels(p.AuthByUID("c1")); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}

	pn := New(Config{Version: "test", APIKey: "k", Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/models", nil)
	req.Header.Set("Authorization", "Bearer k")
	pn.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var chat, image, video bool
	for _, m := range resp.Models {
		switch m["id"] {
		case "cn:glm-5.2":
			chat = true
			if m["image_generation"] == true {
				t.Errorf("对话模型不应带 image_generation：%v", m)
			}
		case "cn:seedance-2.5":
			video = true // 视频模型不该出现（端点已撤回）——出现即失败
		case "cn:hunyuan-image-alpha":
			image = m["image_generation"] == true
			if m["name"] != "Hunyuan Image Alpha" {
				t.Errorf("出图模型应带名字：%v", m)
			}
		}
	}
	if !chat || !image {
		t.Fatalf("面板模型列表缺项：chat=%v image=%v（%d 条）", chat, image, len(resp.Models))
	}
	if video {
		t.Fatal("视频模型不该出现在面板模型列表（没有网关端点，列出来只会引诱客户端去调）")
	}
}

// fakeRoundTripper 测试用 http.RoundTripper（本包内没有现成的，server 包那个不跨包）。
type fakeRoundTripper func(*http.Request) (*http.Response, error)

func (f fakeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestPanelModelsAttachesBusyLevels 面板「模型与档位」附带上游繁忙度（/v2/chat/busy，只读）：
// 按域问对应账号，只附到该域的行上；上游没返回的模型不填字段（不编造）。
func TestPanelModelsAttachesBusyLevels(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			body := `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM-5.2"}]}}`
			switch {
			case strings.HasSuffix(r.URL.Path, "/v2/chat/busy"):
				body = `{"code":0,"msg":"OK","data":[{"model":"glm-5.2","busy_level":"congested"}]}`
			case strings.HasSuffix(r.URL.Path, "/v3/config"):
				body = `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM-5.2"},` +
					`{"id":"glm-5.3-flash","name":"GLM-5.3 Flash"}]}}`
			}
			return &http.Response{StatusCode: 200,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   io.NopCloser(strings.NewReader(body))}, nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	p.SetCredits("c1", 1000, 0)
	up.FetchModels(p.AuthByUID("c1"))

	pn := New(Config{Version: "test", APIKey: "k", Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/models", nil)
	req.Header.Set("Authorization", "Bearer k")
	pn.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, m := range resp.Models {
		if v, ok := m["busy"]; ok {
			seen[m["id"].(string)] = v.(string)
		}
	}
	if seen["cn:glm-5.2"] != "congested" {
		t.Fatalf("cn:glm-5.2 繁忙度未附上: %v（全部 entry=%v）", seen, resp.Models)
	}
	if _, ok := seen["cn:glm-5.3-flash"]; ok {
		t.Fatalf("上游未返回的模型不该凭空有繁忙度: %v", seen)
	}
}

// TestPanelModelsFallsBackToDisabledAccount 全池被禁时仍能拉目录（只读兜底）：
// 健康号优先，禁用号兜底；响应必须标明来源，避免把「禁用号拉的目录」当成可用证据。
func TestPanelModelsFallsBackToDisabledAccount(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			body := `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM-5.2"}],` +
				`"agents":[{"name":"cli","models":["glm-5.2"]}]}}`
			if strings.HasSuffix(r.URL.Path, "/v2/chat/busy") {
				body = `{"code":0,"data":[]}`
			}
			return &http.Response{StatusCode: 200,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   io.NopCloser(strings.NewReader(body))}, nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
	p := pool.New("")
	t.Cleanup(p.Close)
	p.Add(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	p.SetMeta("c1", "", "备用号")
	p.Disable("c1", "session dead")

	pn := New(Config{Version: "test", APIKey: "k", Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/models", nil)
	req.Header.Set("Authorization", "Bearer k")
	pn.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Models         []map[string]any `json:"models"`
		Warnings       []string         `json:"warnings"`
		CatalogSources map[string]struct {
			UID      string `json:"uid"`
			Disabled bool   `json:"disabled"`
			Fallback bool   `json:"fallback"`
		} `json:"catalog_sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range resp.Models {
		if m["id"] == "cn:glm-5.2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("禁用号兜底未拿到目录：%+v", resp.Models)
	}
	src := resp.CatalogSources["cn"]
	if src.UID != "c1" || !src.Disabled || !src.Fallback {
		t.Fatalf("catalog_sources.cn=%+v，want uid=c1 disabled/fallback=true", src)
	}
	joined := strings.Join(resp.Warnings, "；")
	if !strings.Contains(joined, "已禁用账号") {
		t.Fatalf("warnings 未标明禁用号兜底：%v", resp.Warnings)
	}
}

// TestPanelModelsCachesCatalog 模型页目录短缓存：连续两次加载只打一次上游；
// refresh=1 绕过缓存（用户点「重新获取」时必须真的重拉）。
func TestPanelModelsCachesCatalog(t *testing.T) {
	var hits atomic.Int32
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/v3/config") {
				hits.Add(1)
			}
			body := `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM-5.2"}],` +
				`"agents":[{"name":"cli","models":["glm-5.2"]}]}}`
			if strings.HasSuffix(r.URL.Path, "/v2/chat/busy") {
				body = `{"code":0,"data":[]}`
			}
			return &http.Response{StatusCode: 200,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   io.NopCloser(strings.NewReader(body))}, nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
	p := pool.New("")
	t.Cleanup(p.Close)
	p.Add(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	pn := New(Config{Version: "test", APIKey: "k", Pool: p, Upstream: up})

	get := func(q string) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/panel/api/models"+q, nil)
		req.Header.Set("Authorization", "Bearer k")
		pn.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("get %q code=%d body=%s", q, rec.Code, rec.Body.String())
		}
	}

	get("")
	first := hits.Load()
	if first == 0 {
		t.Fatal("首次加载没有请求上游目录")
	}
	get("")
	if got := hits.Load(); got != first {
		t.Fatalf("缓存未命中：第二次加载又打了 %d 次（累计 %d）", got-first, got)
	}
	get("?refresh=1")
	if got := hits.Load(); got <= first {
		t.Fatalf("refresh=1 未绕过缓存（累计 %d）", got)
	}
}
