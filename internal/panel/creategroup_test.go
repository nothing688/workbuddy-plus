package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
)

// TestGroupsCreateAPI 空分组创建接口：成功返回 groups 列表，空名 400。
func TestGroupsCreateAPI(t *testing.T) {
	pl := pool.New("")
	t.Cleanup(pl.Close)
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pl})
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/panel/api/groups", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}
	if rec := post(`{"name":"teamX"}`); rec.Code != http.StatusOK {
		t.Fatalf("create code=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := post(`{"name":"  "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty name code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(post(`{"name":"teamY"}`).Body.String(), "teamX") {
		t.Fatal("创建响应应包含既有分组列表")
	}
}
