package panel

// 通用 JSON 导入（/panel/api/import/cockpit）的形态与字段兼容性测试。
//
// 覆盖：顶层数组（cockpit 零回归）/ 单个对象 / 各种包裹对象 / 字段别名（含秒与毫秒
// 时间戳）/ token 子对象嵌套 / 缺 refresh_token 仍可导入 / 缺必填字段被跳过。

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

type importTestEnv struct {
	panel   *Panel
	pool    *pool.Pool
	authDir string
}

// newImportTestEnv 装配最小可用的面板：池在临时目录、上游全部指向无害信封
// （导入后的签到/激活/余额查询失败只记日志，不影响导入结果）。
func newImportTestEnv(t *testing.T) *importTestEnv {
	t.Helper()
	authDir := filepath.Join(t.TempDir(), "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pl := pool.New("")
	t.Cleanup(pl.Close)

	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			return probeJSONResponse(200, `{"code":0,"data":{}}`), nil
		})},
		ChatBaseCN:        "https://cn.example",
		BillingBaseCN:     "https://cn.example",
		WebBaseCN:         "https://cn.example",
		ChatBaseGlobal:    "https://global.example",
		BillingBaseGlobal: "https://global.example",
	}
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pl, Upstream: up, AuthDir: authDir})
	return &importTestEnv{panel: p, pool: pl, authDir: authDir}
}

type importTestResp struct {
	OK       bool     `json:"ok"`
	Total    int      `json:"total"`
	Imported int      `json:"imported"`
	Skipped  int      `json:"skipped"`
	Errors   []string `json:"errors"`
	Group    string   `json:"group"`
}

func (e *importTestEnv) importRaw(t *testing.T, body string) (int, importTestResp) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "accounts.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/panel/api/import/cockpit", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	e.panel.ServeHTTP(rec, req)

	var resp importTestResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("bad response json: %v (%s)", err, rec.Body.String())
		}
	}
	return rec.Code, resp
}

// mustImport 要求导入成功且全部落池，返回解析后的响应。
func (e *importTestEnv) mustImport(t *testing.T, body string, wantTotal, wantImported int) importTestResp {
	t.Helper()
	code, resp := e.importRaw(t, body)
	if code != http.StatusOK {
		t.Fatalf("import -> %d %+v", code, resp)
	}
	if resp.Total != wantTotal || resp.Imported != wantImported {
		t.Fatalf("total/imported = %d/%d, want %d/%d; errors=%v",
			resp.Total, resp.Imported, wantTotal, wantImported, resp.Errors)
	}
	return resp
}

// account 从池中取回导入结果，顺带校验 auth 文件确实落盘。
func (e *importTestEnv) account(t *testing.T, uid string) *auth.Auth {
	t.Helper()
	a := e.pool.AuthByUID(uid)
	if a == nil {
		t.Fatalf("uid=%s 未进池", uid)
	}
	if _, err := os.Stat(filepath.Join(e.authDir, "workbuddy-"+uid+".json")); err != nil {
		t.Fatalf("uid=%s 凭证文件未落盘: %v", uid, err)
	}
	return a
}

// TestImportTopLevelArrayCockpitUnchanged 原 cockpit 导出格式零回归。
func TestImportTopLevelArrayCockpitUnchanged(t *testing.T) {
	e := newImportTestEnv(t)
	expMS := time.Now().Add(90 * 24 * time.Hour).UnixMilli()

	resp := e.mustImport(t, `[
		{"uid":"c1","access_token":"at1","refresh_token":"rt1","expires_at":`+itoa(expMS)+`,"nickname":"一号","email":"a@b.c","domain":"www.codebuddy.cn"},
		{"uid":"c2","access_token":"at2","refresh_token":"rt2","expires_at":`+itoa(expMS)+`,"email":"c2@b.c"}
	]`, 2, 2)
	if len(resp.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", resp.Errors)
	}

	a1 := e.account(t, "c1")
	if a1.AccessToken != "at1" || a1.RefreshToken != "rt1" || a1.Nickname != "一号" {
		t.Fatalf("c1=%+v", a1)
	}
	if a1.ExpiresAt != expMS/1000 {
		t.Fatalf("c1 expiresAt=%d want %d（毫秒应转秒）", a1.ExpiresAt, expMS/1000)
	}
	if a1.RealmStored() != "cn" {
		t.Fatalf("c1 realm=%q want cn", a1.RealmStored())
	}
	// cockpit 没有 nickname 时沿用 email（原行为）。
	if a2 := e.account(t, "c2"); a2.Nickname != "c2@b.c" {
		t.Fatalf("c2 nickname=%q want email fallback", a2.Nickname)
	}
}

// TestImportSingleAccountObject 单个账号对象（非数组）。
func TestImportSingleAccountObject(t *testing.T) {
	e := newImportTestEnv(t)
	e.mustImport(t, `{"uid":"s1","access_token":"at-s1","refresh_token":"rt-s1"}`, 1, 1)
	if a := e.account(t, "s1"); a.AccessToken != "at-s1" {
		t.Fatalf("s1=%+v", a)
	}
}

// TestImportWrappedObjects 各种包裹对象都取其中的数组。
func TestImportWrappedObjects(t *testing.T) {
	for _, key := range []string{"accounts", "data", "items", "list", "result"} {
		t.Run(key, func(t *testing.T) {
			e := newImportTestEnv(t)
			uid := "w-" + key
			body := `{"` + key + `":[{"uid":"` + uid + `","access_token":"at-` + uid + `","refresh_token":"rt-` + uid + `"}]}`
			e.mustImport(t, body, 1, 1)
			e.account(t, uid)
		})
	}
}

// TestImportFieldAliases 别名字段：id/token/refresh/name/expiresAt（秒与毫秒）。
func TestImportFieldAliases(t *testing.T) {
	e := newImportTestEnv(t)
	sec := time.Now().Add(30 * 24 * time.Hour).Unix()
	ms := time.Now().Add(60 * 24 * time.Hour).UnixMilli()

	e.mustImport(t, `[
		{"id":"al1","token":"at-al1","refresh":"rt-al1","name":"别名一","expiresAt":`+itoa(sec)+`},
		{"userId":"al2","accessToken":"at-al2","refreshToken":"rt-al2","displayName":"别名二","expires_at":`+itoa(ms)+`}
	]`, 2, 2)

	a1 := e.account(t, "al1")
	if a1.AccessToken != "at-al1" || a1.RefreshToken != "rt-al1" || a1.Nickname != "别名一" {
		t.Fatalf("al1=%+v", a1)
	}
	if a1.ExpiresAt != sec {
		t.Fatalf("al1 expiresAt=%d want %d（秒不应再除 1000）", a1.ExpiresAt, sec)
	}
	if a2 := e.account(t, "al2"); a2.Nickname != "别名二" || a2.ExpiresAt != ms/1000 {
		t.Fatalf("al2=%+v（expiresAt want %d）", a2, ms/1000)
	}
}

// TestImportRFC3339Expires RFC3339 时间字符串。
func TestImportRFC3339Expires(t *testing.T) {
	e := newImportTestEnv(t)
	when := time.Now().Add(10 * 24 * time.Hour).UTC().Truncate(time.Second)
	e.mustImport(t, `{"uid":"r1","access_token":"at-r1","refresh_token":"rt-r1","expires_at":"`+when.Format(time.RFC3339)+`"}`, 1, 1)
	if a := e.account(t, "r1"); a.ExpiresAt != when.Unix() {
		t.Fatalf("expiresAt=%d want %d", a.ExpiresAt, when.Unix())
	}
}

// TestImportNestedTokenObject token / credentials 子对象浅合并。
func TestImportNestedTokenObject(t *testing.T) {
	e := newImportTestEnv(t)
	ms := time.Now().Add(20 * 24 * time.Hour).UnixMilli()
	e.mustImport(t, `[
		{"uid":"n1","token":{"accessToken":"at-n1","refreshToken":"rt-n1","expiresAt":`+itoa(ms)+`,"nickname":"嵌套"}},
		{"uid":"n2","credentials":{"access_token":"at-n2","refresh_token":"rt-n2","email":"n2@b.c"}}
	]`, 2, 2)

	a1 := e.account(t, "n1")
	if a1.AccessToken != "at-n1" || a1.RefreshToken != "rt-n1" || a1.Nickname != "嵌套" {
		t.Fatalf("n1=%+v", a1)
	}
	if a1.ExpiresAt != ms/1000 {
		t.Fatalf("n1 expiresAt=%d want %d", a1.ExpiresAt, ms/1000)
	}
	if a2 := e.account(t, "n2"); a2.AccessToken != "at-n2" || a2.Nickname != "n2@b.c" {
		t.Fatalf("n2=%+v", a2)
	}
}

// TestImportMissingRefreshTokenAllowed refresh_token 可空（静态 token 账号）。
func TestImportMissingRefreshTokenAllowed(t *testing.T) {
	e := newImportTestEnv(t)
	e.mustImport(t, `{"uid":"no-rt","access_token":"only-access"}`, 1, 1)
	a := e.account(t, "no-rt")
	if a.AccessToken != "only-access" || a.RefreshToken != "" {
		t.Fatalf("no-rt=%+v", a)
	}
}

// TestImportSkipsMissingRequired 缺 uid / access_token 的条目被跳过并写入 errors。
func TestImportSkipsMissingRequired(t *testing.T) {
	e := newImportTestEnv(t)
	code, resp := e.importRaw(t, `[
		{"access_token":"at-orphan","refresh_token":"rt"},
		{"uid":"no-at","refresh_token":"rt"},
		{"uid":"good","access_token":"at-good","refresh_token":"rt-good"}
	]`)
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if resp.Total != 3 || resp.Imported != 1 || resp.Skipped != 2 {
		t.Fatalf("resp=%+v, want total=3 imported=1 skipped=2", resp)
	}
	joined := strings.Join(resp.Errors, " | ")
	if !strings.Contains(joined, "missing uid") || !strings.Contains(joined, "missing access_token") {
		t.Fatalf("errors 应说明缺哪个字段，得到 %v", resp.Errors)
	}
	if e.pool.AuthByUID("no-at") != nil || e.pool.AuthByUID("good") == nil {
		t.Fatal("跳过/导入结果不符")
	}
}

// TestImportExplicitRealmOverridesDomain realm 别名优先于 domain 推断（intl → global）。
func TestImportExplicitRealmOverridesDomain(t *testing.T) {
	e := newImportTestEnv(t)
	e.mustImport(t, `{"uid":"g1","access_token":"at-g1","refresh_token":"rt-g1","domain":"www.codebuddy.cn","realm":"intl"}`, 1, 1)
	if a := e.account(t, "g1"); a.RealmStored() != "global" {
		t.Fatalf("realm=%q want global（intl 应归一为 global 且优先于 domain）", a.RealmStored())
	}
}

// TestImportWrappedEmptyArray 包裹对象里数组为空 → 400（与原空数组口径一致）。
func TestImportWrappedEmptyArray(t *testing.T) {
	e := newImportTestEnv(t)
	code, _ := e.importRaw(t, `{"accounts":[]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", code)
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

// TestImportNestedAccountAuth 覆盖 account + auth 两层嵌套（CodeBuddy 桌面端导出形态）：
// uid/nickname 在内层 account，token/到期/realm 在内层 auth。
func TestImportNestedAccountAuth(t *testing.T) {
	e := newImportTestEnv(t)
	body := `{
	  "account": {"enterpriseId":"","nickname":"15980960215","uid":"nested-uid-1"},
	  "auth": {"accessToken":"at-nested","refreshToken":"rt-nested","domain":"www.codebuddy.cn","expiresAt":1794269489,"realm":"cn"}
	}`
	e.mustImport(t, body, 1, 1)
	a := e.account(t, "nested-uid-1")
	if a.Nickname != "15980960215" {
		t.Fatalf("nickname=%q", a.Nickname)
	}
	if a.Realm() != "cn" {
		t.Fatalf("realm=%q", a.Realm())
	}
	if got := a.AccessTokenValue(); got != "at-nested" {
		t.Fatalf("access token=%q", got)
	}
	if a.ExpiresAt != 1794269489 {
		t.Fatalf("expires=%d", a.ExpiresAt)
	}
}
