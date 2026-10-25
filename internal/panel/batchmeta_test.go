package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
)

// newBatchMetaPool 建一个三账号的池：u1/u2 在 teamOld 组且各带备注，u3 未分组。
// 备注是断言「批量移动不清备注」的前提，所以初始值必须非空。
func newBatchMetaPool(t *testing.T) *pool.Pool {
	t.Helper()
	pl := pool.New("")
	t.Cleanup(pl.Close)
	for _, acc := range []struct{ uid, note string }{
		{"u1", "主号，勿动"},
		{"u2", "备用"},
	} {
		pl.Add(&auth.Auth{
			UID:          acc.uid,
			AccessToken:  "at-" + acc.uid,
			RefreshToken: "rt-" + acc.uid,
			ExpiresAt:    9999999999,
			Domain:       "www.codebuddy.cn",
		})
		pl.SetMeta(acc.uid, "teamOld", acc.note)
	}
	pl.Add(&auth.Auth{
		UID:          "u3",
		AccessToken:  "at-u3",
		RefreshToken: "rt-u3",
		ExpiresAt:    9999999999,
		Domain:       "www.codebuddy.cn",
	})
	return pl
}

// metaBatchResp 批量移动的固定回包。
type metaBatchResp struct {
	OK      bool     `json:"ok"`
	Updated int      `json:"updated"`
	Missing []string `json:"missing"`
}

// doMetaBatch 发一次批量移动请求（复用 accountmeta_test.go 的带鉴权请求构造）。
func doMetaBatch(t *testing.T, p *Panel, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doAccountMetaRequest(t, p, "POST", "/panel/api/accounts/meta_batch", body)
}

// TestAccountsMetaBatch 正常批量移动：分组生效、备注保持不变（两个号都改）。
func TestAccountsMetaBatch(t *testing.T) {
	pl := newBatchMetaPool(t)
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pl})

	rec := doMetaBatch(t, pn, `{"uids":["u1","u2"],"group":"  teamA  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp metaBatchResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Updated != 2 || len(resp.Missing) != 0 {
		t.Fatalf("resp=%+v, want ok updated=2 missing=[]", resp)
	}
	// 断言 missing 是 [] 而不是 null：前端直接遍历，不该再判空。
	if !strings.Contains(rec.Body.String(), `"missing":[]`) {
		t.Fatalf("body=%s, want missing:[]", rec.Body.String())
	}

	for uid, wantNote := range map[string]string{"u1": "主号，勿动", "u2": "备用"} {
		st, ok := pl.Status(uid)
		if !ok {
			t.Fatalf("uid=%s 消失", uid)
		}
		if st.Group != "teamA" {
			t.Fatalf("uid=%s group=%q, want teamA", uid, st.Group)
		}
		if st.Note != wantNote {
			t.Fatalf("uid=%s note=%q, want %q（备注必须保留）", uid, st.Note, wantNote)
		}
	}
	// 未被选中的账号不受影响。
	if st, _ := pl.Status("u3"); st.Group != "" {
		t.Fatalf("u3 group=%q, want 未分组", st.Group)
	}
}

// TestAccountsMetaBatchPartialMissing 含不存在的 UID：部分成功仍 200，missing 只报那个号。
func TestAccountsMetaBatchPartialMissing(t *testing.T) {
	pl := newBatchMetaPool(t)
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pl})

	rec := doMetaBatch(t, pn, `{"uids":["u1","ghost"],"group":"teamB"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp metaBatchResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Updated != 1 {
		t.Fatalf("resp=%+v, want ok updated=1", resp)
	}
	if len(resp.Missing) != 1 || resp.Missing[0] != "ghost" {
		t.Fatalf("missing=%v, want [ghost]", resp.Missing)
	}
	if st, _ := pl.Status("u1"); st.Group != "teamB" || st.Note != "主号，勿动" {
		t.Fatalf("u1 group/note=%q/%q", st.Group, st.Note)
	}
}

// TestAccountsMetaBatchClearGroup 空 group = 移出分组（回到未分组），备注仍然保留。
func TestAccountsMetaBatchClearGroup(t *testing.T) {
	pl := newBatchMetaPool(t)
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pl})

	rec := doMetaBatch(t, pn, `{"uids":["u1","u2"],"group":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp metaBatchResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Updated != 2 {
		t.Fatalf("updated=%d, want 2", resp.Updated)
	}
	for uid, wantNote := range map[string]string{"u1": "主号，勿动", "u2": "备用"} {
		st, _ := pl.Status(uid)
		if st.Group != "" {
			t.Fatalf("uid=%s group=%q, want 未分组", uid, st.Group)
		}
		if st.Note != wantNote {
			t.Fatalf("uid=%s note=%q, want %q", uid, st.Note, wantNote)
		}
	}
}

// TestAccountsMetaBatchBadRequest 空 uids / 坏 JSON / 超上限都是 400，且一个账号都不改。
func TestAccountsMetaBatchBadRequest(t *testing.T) {
	pl := newBatchMetaPool(t)
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pl})

	cases := []struct{ name, body, wantErr string }{
		{"empty", `{"uids":[],"group":"teamB"}`, "uids required"},
		{"missing field", `{"group":"teamB"}`, "uids required"},
		{"only blanks", `{"uids":["  ",""],"group":"teamB"}`, "uids required"},
		{"bad json", `{"uids":`, ""}, // 文案带 decoder 细节，只断言 400
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := doMetaBatch(t, pn, c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code=%d body=%s, want 400", rec.Code, rec.Body.String())
			}
			var errResp map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
				t.Fatal(err)
			}
			if c.wantErr != "" && errResp["error"] != c.wantErr {
				t.Fatalf("error=%v, want %q", errResp["error"], c.wantErr)
			}
			// 400 必须整批不动：坏 JSON 解到一半就写会把账号改成半套。
			for _, uid := range []string{"u1", "u2"} {
				st, _ := pl.Status(uid)
				if st.Group != "teamOld" {
					t.Fatalf("uid=%s group=%q, want teamOld（400 不应修改）", uid, st.Group)
				}
			}
		})
	}

	// 超上限：1001 条直接拒绝，不进入逐条 SetMeta。
	many := make([]string, 0, metaBatchMaxUIDs+1)
	for i := 0; i <= metaBatchMaxUIDs; i++ {
		many = append(many, "uid-"+strconv.Itoa(i))
	}
	body, err := json.Marshal(map[string]any{"uids": many, "group": "teamB"})
	if err != nil {
		t.Fatal(err)
	}
	rec := doMetaBatch(t, pn, string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("over limit code=%d, want 400", rec.Code)
	}
	var errResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatal(err)
	}
	if errResp["error"] != "too many uids (max 1000)" {
		t.Fatalf("error=%v, want too many uids (max 1000)", errResp["error"])
	}
	if st, _ := pl.Status("u1"); st.Group != "teamOld" {
		t.Fatalf("u1 group=%q, want teamOld（超限不应修改）", st.Group)
	}
}

// TestAccountsMetaBatchDedup 重复 UID（含前后空白）只处理一次，updated 不重复计数。
func TestAccountsMetaBatchDedup(t *testing.T) {
	pl := newBatchMetaPool(t)
	pn := New(Config{Version: "test", APIKey: "test-key", Pool: pl})

	rec := doMetaBatch(t, pn, `{"uids":["u1"," u1 ","u1"],"group":"teamC"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp metaBatchResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Updated != 1 {
		t.Fatalf("updated=%d, want 1（重复只计一次）", resp.Updated)
	}
	if len(resp.Missing) != 0 {
		t.Fatalf("missing=%v, want []", resp.Missing)
	}
	if st, _ := pl.Status("u1"); st.Group != "teamC" || st.Note != "主号，勿动" {
		t.Fatalf("u1 group/note=%q/%q", st.Group, st.Note)
	}

	// 去重后为空（全是重复的空白项）同样按 uids required 处理。
	rec = doMetaBatch(t, pn, `{"uids":[" ","  "],"group":"teamD"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("blank-only code=%d, want 400", rec.Code)
	}
}
