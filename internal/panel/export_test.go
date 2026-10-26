package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type exportResp struct {
	ExportedAt string `json:"exported_at"`
	Accounts   []struct {
		Account struct {
			UID      string `json:"uid"`
			Nickname string `json:"nickname"`
		} `json:"account"`
		Auth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Realm        string `json:"realm"`
		} `json:"auth"`
		Group string `json:"group"`
		Note  string `json:"note"`
	} `json:"accounts"`
}

func (e *importTestEnv) exportRaw(t *testing.T) (*httptest.ResponseRecorder, exportResp, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "/panel/api/export/accounts", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	e.panel.ServeHTTP(rec, req)
	var out exportResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("bad export json: %v", err)
		}
	}
	return rec, out, rec.Body.String()
}

// TestExportAccountsShape 导出结构与元数据：account+auth 同构、分组/备注保留。
func TestExportAccountsShape(t *testing.T) {
	e := newImportTestEnv(t)
	e.mustImport(t, `[
		{"uid":"e1","access_token":"at-e1","refresh_token":"rt-e1","expires_at":1893456000,"nickname":"导出号","domain":"www.codebuddy.cn"},
		{"uid":"e2","access_token":"at-e2","refresh_token":"rt-e2","expires_at":1893456000,"nickname":"导出号2"}
	]`, 2, 2)
	if !e.pool.SetMeta("e1", "teamA", "主力号") {
		t.Fatal("set meta failed")
	}

	rec, out, _ := e.exportRaw(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("content-disposition=%q", cd)
	}
	if len(out.Accounts) != 2 {
		t.Fatalf("accounts=%d", len(out.Accounts))
	}
	var got *int
	for i := range out.Accounts {
		if out.Accounts[i].Account.UID == "e1" {
			got = &i
		}
	}
	if got == nil {
		t.Fatalf("e1 missing: %+v", out.Accounts)
	}
	a := out.Accounts[*got]
	if a.Account.Nickname != "导出号" || a.Auth.AccessToken != "at-e1" || a.Auth.RefreshToken != "rt-e1" {
		t.Fatalf("e1 exported=%+v", a)
	}
	if a.Auth.Realm != "cn" || a.Group != "teamA" || a.Note != "主力号" {
		t.Fatalf("e1 meta=%+v", a)
	}
}

// TestExportImportRoundTrip 导出文件可直接再导入：uid/token/分组/备注都还原。
func TestExportImportRoundTrip(t *testing.T) {
	src := newImportTestEnv(t)
	src.mustImport(t, `{"uid":"rt1","access_token":"at-rt1","refresh_token":"rt-rt1","expires_at":1893456000,"nickname":"往返号"}`, 1, 1)
	if !src.pool.SetMeta("rt1", "teamB", "备注往返") {
		t.Fatal("set meta failed")
	}
	_, _, body := src.exportRaw(t)

	dst := newImportTestEnv(t)
	code, resp := dst.importRaw(t, body)
	if code != http.StatusOK || resp.Imported != 1 {
		t.Fatalf("reimport code=%d resp=%+v", code, resp)
	}
	st, ok := dst.pool.Status("rt1")
	if !ok {
		t.Fatal("rt1 not imported")
	}
	if st.Group != "teamB" || st.Note != "备注往返" {
		t.Fatalf("group/note=%q/%q", st.Group, st.Note)
	}
	a := dst.account(t, "rt1")
	if a.AccessTokenValue() != "at-rt1" || a.RefreshToken != "rt-rt1" {
		t.Fatalf("tokens lost: %+v", a)
	}
}

// TestExportAccountsSelectedSubset 多选导出：uids 只导出勾选的账号（保持池内顺序），
// 全部 UID 都匹配不到时返回 400，而不是悄悄给一个空文件。
func TestExportAccountsSelectedSubset(t *testing.T) {
	e := newImportTestEnv(t)
	e.mustImport(t, `[
		{"uid":"e1","access_token":"at-e1","refresh_token":"rt-e1","expires_at":1893456000},
		{"uid":"e2","access_token":"at-e2","refresh_token":"rt-e2","expires_at":1893456000},
		{"uid":"e3","access_token":"at-e3","refresh_token":"rt-e3","expires_at":1893456000}
	]`, 3, 3)

	get := func(q string) (*httptest.ResponseRecorder, exportResp) {
		t.Helper()
		req := httptest.NewRequest("GET", "/panel/api/export/accounts"+q, nil)
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		e.panel.ServeHTTP(rec, req)
		var out exportResp
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("bad export json: %v", err)
			}
		}
		return rec, out
	}

	rec, out := get("?uids=e1,e3")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(out.Accounts) != 2 || out.Accounts[0].Account.UID != "e1" || out.Accounts[1].Account.UID != "e3" {
		t.Fatalf("selected export=%+v，want 仅 e1/e3 且保持池内顺序", out.Accounts)
	}

	// 不认识的 UID 被忽略；剩下的有效选择照常导出。
	rec, out = get("?uids=nope,e2")
	if rec.Code != http.StatusOK || len(out.Accounts) != 1 || out.Accounts[0].Account.UID != "e2" {
		t.Fatalf("partial selection code=%d accounts=%+v", rec.Code, out.Accounts)
	}

	// 全部匹配不到：400，前端据此提示刷新，而不是下载空文件。
	rec, _ = get("?uids=nope1,nope2")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no match code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 不带 uids 仍然导出全部（兼容旧行为）。
	rec, out = get("")
	if rec.Code != http.StatusOK || len(out.Accounts) != 3 {
		t.Fatalf("export all code=%d n=%d", rec.Code, len(out.Accounts))
	}
}
