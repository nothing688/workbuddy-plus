package panel

// 通用 JSON 导入（/panel/api/import/cockpit）的形态与字段兼容性测试。
//
// 覆盖：顶层数组（cockpit 零回归）/ 单个对象 / 各种包裹对象 / 字段别名（含秒与毫秒
// 时间戳）/ token 子对象嵌套 / 缺 refresh_token 仍可导入 / 缺必填字段被跳过。

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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
	upCalls *atomic.Int32
}

// newImportTestEnv 装配最小可用的面板：池在临时目录、上游全部指向无害信封
// （导入后的签到/激活/余额查询失败只记日志，不影响导入结果）。
// taskJobOf 读作业表里某账号的作业（作业表由 StartTaskJobs 创建）。
func (e *importTestEnv) taskJobOf(uid string) (TaskJob, bool) {
	m := e.panel.getTaskJobManager()
	if m == nil {
		return TaskJob{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[uid]
	return j, ok
}

func newImportTestEnv(t *testing.T) *importTestEnv {
	t.Helper()
	authDir := filepath.Join(t.TempDir(), "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pl := pool.New("")
	t.Cleanup(pl.Close)

	calls := &atomic.Int32{}
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return probeJSONResponse(200, `{"code":0,"data":{}}`), nil
		})},
		ChatBaseCN:        "https://cn.example",
		BillingBaseCN:     "https://cn.example",
		WebBaseCN:         "https://cn.example",
		ChatBaseGlobal:    "https://global.example",
		BillingBaseGlobal: "https://global.example",
	}
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pl, Upstream: up, AuthDir: authDir})
	return &importTestEnv{panel: p, pool: pl, authDir: authDir, upCalls: calls}
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
	return e.importRawForm(t, body, nil)
}

// importRawForm 同 importRaw，额外带表单字段（group / skip_tasks 等）。
func (e *importTestEnv) importRawForm(t *testing.T, body string, fields map[string]string) (int, importTestResp) {
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
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
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

// TestImportTwoLevelWrapper 两层包裹 {"data":{"accounts":[...]}}，以及三层包裹。
func TestImportTwoLevelWrapper(t *testing.T) {
	e := newImportTestEnv(t)
	e.mustImport(t, `{"code":0,"data":{"total":2,"accounts":[
		{"uid":"tw1","access_token":"at-tw1"},
		{"account":{"uid":"tw2","nickname":"两层"},"auth":{"accessToken":"at-tw2","realm":"cn"}}
	]}}`, 2, 2)
	if a := e.account(t, "tw1"); a.AccessToken != "at-tw1" {
		t.Fatalf("tw1=%+v", a)
	}
	if a := e.account(t, "tw2"); a.Nickname != "两层" || a.AccessToken != "at-tw2" {
		t.Fatalf("tw2=%+v", a)
	}

	// 三层（data → result → accounts）仍在深度上限内。
	e2 := newImportTestEnv(t)
	e2.mustImport(t, `{"data":{"result":{"accounts":[{"uid":"d3","access_token":"at-d3"}]}}}`, 1, 1)
	e2.account(t, "d3")
}

// TestImportDeepNestingRejected 超过包裹深度上限 → 400，不进入病态递归。
func TestImportDeepNestingRejected(t *testing.T) {
	e := newImportTestEnv(t)
	code, _ := e.importRaw(t, `{"data":{"data":{"data":{"data":{"accounts":[{"uid":"x","access_token":"y"}]}}}}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400（超深嵌套应明确报错）", code)
	}
}

// TestImportJSONLCRLF JSONL：CRLF 行尾、首尾空白空行、BOM 开头，每行一个对象。
func TestImportJSONLCRLF(t *testing.T) {
	e := newImportTestEnv(t)
	body := "\xef\xbb\xbf\r\n  \r\n" +
		`{"uid":"jl1","access_token":"at-jl1","nickname":"行一"}` + "\r\n" +
		`{"account":{"uid":"jl2","nickname":"行二"},"auth":{"accessToken":"at-jl2"}}` + "\r\n" +
		"   \r\n"
	resp := e.mustImport(t, body, 2, 2)
	if len(resp.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", resp.Errors)
	}
	if a := e.account(t, "jl1"); a.Nickname != "行一" {
		t.Fatalf("jl1=%+v", a)
	}
	if a := e.account(t, "jl2"); a.Nickname != "行二" || a.AccessToken != "at-jl2" {
		t.Fatalf("jl2=%+v", a)
	}
}

// TestImportStringArray 字符串数组：JSON 对象字符串照常解析，裸 token 缺 uid 被跳过。
func TestImportStringArray(t *testing.T) {
	e := newImportTestEnv(t)
	code, resp := e.importRaw(t, `[
		"{\"uid\":\"str1\",\"access_token\":\"at-str1\",\"nickname\":\"字符串账号\"}",
		"raw-token-without-uid"
	]`)
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if resp.Total != 2 || resp.Imported != 1 || resp.Skipped != 1 {
		t.Fatalf("resp=%+v, want total=2 imported=1 skipped=1", resp)
	}
	if a := e.account(t, "str1"); a.AccessToken != "at-str1" || a.Nickname != "字符串账号" {
		t.Fatalf("str1=%+v", a)
	}
	if joined := strings.Join(resp.Errors, " | "); !strings.Contains(joined, "missing uid") {
		t.Fatalf("裸 token 应报 missing uid，得到 %v", resp.Errors)
	}
}

// TestImportMixedNonObjectItems 数组里混 null / 数字 / 布尔：只跳过这些项，其余照常导。
func TestImportMixedNonObjectItems(t *testing.T) {
	e := newImportTestEnv(t)
	code, resp := e.importRaw(t, `[
		null,
		123,
		false,
		{"uid":"mx1","access_token":"at-mx1"}
	]`)
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if resp.Total != 4 || resp.Imported != 1 || resp.Skipped != 3 {
		t.Fatalf("resp=%+v, want total=4 imported=1 skipped=3", resp)
	}
	joined := strings.Join(resp.Errors, " | ")
	for _, want := range []string{"null", "number", "boolean"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("errors 应说明 %s 项被跳过，得到 %v", want, resp.Errors)
		}
	}
	e.account(t, "mx1")
}

// TestImportNestedAccountAuthListMixed 桌面端 {account,auth} 列表混非对象项。
func TestImportNestedAccountAuthListMixed(t *testing.T) {
	e := newImportTestEnv(t)
	code, resp := e.importRaw(t, `{"accounts":[
		{"account":{"uid":"1","nickname":"a"},"auth":{"accessToken":"t1","realm":"cn"}},
		null,
		{"account":{"uid":"2","nickname":"b"},"auth":{"accessToken":"t2","realm":"cn"}}
	]}`)
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if resp.Total != 3 || resp.Imported != 2 || resp.Skipped != 1 {
		t.Fatalf("resp=%+v, want total=3 imported=2 skipped=1", resp)
	}
	if a := e.account(t, "1"); a.Nickname != "a" || a.AccessToken != "t1" || a.RealmStored() != "cn" {
		t.Fatalf("uid=1 %+v", a)
	}
	if a := e.account(t, "2"); a.Nickname != "b" || a.AccessToken != "t2" {
		t.Fatalf("uid=2 %+v", a)
	}
}

// TestImportExportedTwoAccounts 用户提供的那份「两个账号」导出文件：原有逻辑即可导 2 个。
func TestImportExportedTwoAccounts(t *testing.T) {
	e := newImportTestEnv(t)
	body := `{"exported_at":"2026-10-25T00:00:00Z","accounts":[
		{"account":{"uid":"1","nickname":"a"},"auth":{"accessToken":"t1","realm":"cn"}},
		{"account":{"uid":"2","nickname":"b"},"auth":{"accessToken":"t2","realm":"cn"}}
	]}`
	e.mustImport(t, body, 2, 2)
	e.account(t, "1")
	e.account(t, "2")
}

// TestImportSkipGrowthKeepsCheckin 勾选「跳过成长任务」时：
//   - 签到 / 余额查询照常执行（回血与可用性观测，跳过等于让新号白等一天）；
//   - 只有「新账号自动跑成长任务」被跳过（那是唯一会为点亮判据发真实对话的一步）。
func TestImportSkipGrowthKeepsCheckin(t *testing.T) {
	e := newImportTestEnv(t)
	e.panel.cfg.AutoTasksEnabled = func() bool { return true }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := e.panel.StartTaskJobs(ctx, filepath.Join(t.TempDir(), "task-jobs.json")); err != nil {
		t.Fatal(err)
	}
	// 作业是异步跑的：先停掉再让 t.TempDir 清理，否则收尾写盘会撞上目录已删。
	t.Cleanup(func() { e.panel.getTaskJobManager().stop() })

	code, resp := e.importRawForm(t,
		`[{"uid":"skip1","access_token":"at-skip1","refresh_token":"rt-skip1","expires_at":1893456000}]`,
		map[string]string{"skip_growth_tasks": "1"})
	if code != http.StatusOK || resp.Imported != 1 {
		t.Fatalf("import code=%d resp=%+v", code, resp)
	}
	if _, ok := e.pool.Status("skip1"); !ok {
		t.Fatal("skip1 未进池")
	}
	if n := e.upCalls.Load(); n == 0 {
		t.Fatal("勾选「跳过成长任务」后仍应签到 + 查余额（这一步不该被跳过）")
	}
	if _, ok := e.taskJobOf("skip1"); ok {
		t.Fatal("勾选后不该启动新账号成长任务")
	}

	// 不勾选时照旧启动成长任务作业（同一套环境，只有这一个变量不同）。
	code, resp = e.importRawForm(t,
		`[{"uid":"run1","access_token":"at-run1","refresh_token":"rt-run1","expires_at":1893456000}]`, nil)
	if code != http.StatusOK || resp.Imported != 1 {
		t.Fatalf("normal import code=%d resp=%+v", code, resp)
	}
	if _, ok := e.taskJobOf("run1"); !ok {
		t.Fatal("未勾选时应启动新账号成长任务")
	}
}
