package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
)

// poolActiveResponse GET/POST 的响应形状（只解出本用例关心的字段）。
type poolActiveResponse struct {
	OK     bool   `json:"ok"`
	Group  string `json:"group"`
	Groups []struct {
		Name    string `json:"name"`
		Total   int    `json:"total"`
		Healthy int    `json:"healthy"`
	} `json:"groups"`
}

func doPoolActiveRequest(t *testing.T, p *Panel, method, body string) (*httptest.ResponseRecorder, poolActiveResponse) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "/panel/api/pool/active", rdr)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	var out poolActiveResponse
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v (body=%s)", method, err, rec.Body.String())
		}
	}
	return rec, out
}

// TestPoolActiveInitialAndAuth 初始为空（全池），并且两个端点都经过 withAuth。
func TestPoolActiveInitialAndAuth(t *testing.T) {
	pl := pool.New("")
	t.Cleanup(pl.Close)
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pl})

	rec, out := doPoolActiveRequest(t, pn, http.MethodGet, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET code=%d body=%s", rec.Code, rec.Body.String())
	}
	if out.Group != "" {
		t.Fatalf("初始 group=%q, want 空串（全池）", out.Group)
	}
	if len(out.Groups) != 0 {
		t.Fatalf("空池 groups=%+v, want 空切片", out.Groups)
	}

	// 无 Bearer → 401（直接打 mux，不经过 helper 的鉴权头）。
	req := httptest.NewRequest(http.MethodGet, "/panel/api/pool/active", nil)
	w := httptest.NewRecorder()
	pn.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无密钥 GET code=%d, want 401", w.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/panel/api/pool/active", strings.NewReader(`{"group":"teamA"}`))
	w = httptest.NewRecorder()
	pn.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无密钥 POST code=%d, want 401", w.Code)
	}
}

// TestPoolActiveSwitchAndClear 切换后 GET 能回读；空串清除回全池。
func TestPoolActiveSwitchAndClear(t *testing.T) {
	pl := pool.New("")
	t.Cleanup(pl.Close)
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pl})

	rec, out := doPoolActiveRequest(t, pn, http.MethodPost, `{"group":"  teamA  "}`)
	if rec.Code != http.StatusOK || !out.OK {
		t.Fatalf("POST code=%d body=%s", rec.Code, rec.Body.String())
	}
	// 返回值是实际生效值（TrimSpace 后），前端据此更新徽章。
	if out.Group != "teamA" {
		t.Fatalf("POST group=%q, want teamA", out.Group)
	}

	_, got := doPoolActiveRequest(t, pn, http.MethodGet, "")
	if got.Group != "teamA" {
		t.Fatalf("GET 回读 group=%q, want teamA", got.Group)
	}

	rec, out = doPoolActiveRequest(t, pn, http.MethodPost, `{"group":""}`)
	if rec.Code != http.StatusOK || out.Group != "" {
		t.Fatalf("清除 POST code=%d group=%q body=%s", rec.Code, out.Group, rec.Body.String())
	}
	_, got = doPoolActiveRequest(t, pn, http.MethodGet, "")
	if got.Group != "" {
		t.Fatalf("清除后 GET group=%q, want 空串", got.Group)
	}
}

// TestPoolActiveGroupsAggregation groups 必须与 Pool.GroupCounts 同源：
// 未分组排最后，禁用账号计入 total 但不计入 healthy。
func TestPoolActiveGroupsAggregation(t *testing.T) {
	pl := pool.New("")
	t.Cleanup(pl.Close)
	pl.Add(&auth.Auth{UID: "u1"}) // teamA 健康
	pl.Add(&auth.Auth{UID: "u2"}) // teamA 禁用
	pl.Add(&auth.Auth{UID: "u3"}) // teamB 健康
	pl.Add(&auth.Auth{UID: "u4"}) // 未分组
	pl.SetMeta("u1", "teamA", "")
	pl.SetMeta("u2", "teamA", "")
	pl.SetMeta("u3", "teamB", "")
	pl.Disable("u2", "manual")

	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pl})
	rec, out := doPoolActiveRequest(t, pn, http.MethodGet, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET code=%d body=%s", rec.Code, rec.Body.String())
	}
	// 禁用语义（2026-10-28 起）：不再搬进「已禁用」组，禁用号留在原组，
	// 只让该组的 Healthy 减一 —— 归属是运维信息，不因禁用被改写。
	want := []struct {
		name          string
		total, health int
	}{
		{"teamA", 2, 1},
		{"teamB", 1, 1},
		{"", 1, 1},
	}
	if len(out.Groups) != len(want) {
		t.Fatalf("groups=%+v, want %+v", out.Groups, want)
	}
	for i := range want {
		g := out.Groups[i]
		if g.Name != want[i].name || g.Total != want[i].total || g.Healthy != want[i].health {
			t.Fatalf("groups[%d]=%+v, want %+v (all=%+v)", i, g, want[i], out.Groups)
		}
	}
}

// TestPoolActiveBadJSON 坏 JSON 返回 400，且不清空已有的 active group。
func TestPoolActiveBadJSON(t *testing.T) {
	pl := pool.New("")
	t.Cleanup(pl.Close)
	pl.SetActiveGroup("teamA")
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pl})

	rec, _ := doPoolActiveRequest(t, pn, http.MethodPost, `{"group":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("坏 JSON code=%d, want 400", rec.Code)
	}
	if got := pl.ActiveGroup(); got != "teamA" {
		t.Fatalf("坏 JSON 后 active group=%q, want teamA（不能被清空）", got)
	}
}
