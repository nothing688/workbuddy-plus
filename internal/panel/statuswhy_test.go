package panel

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestAppJSStatusWhyHtml 状态列原因行：上游业务码/HTTP 状态码要变成胶囊，正文不要
// 重复开头的码；上游信封里的 zh 原文优先于英文 msg。这块只在前端做，后端测试覆盖
// 不到，所以用 node 沙箱直接切函数求值。
func TestAppJSStatusWhyHtml(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; status why test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function statusWhyHtml('), end = src.indexOf('function renderAccounts(');
assert.ok(start >= 0 && end > start, 'statusWhyHtml not found');
const ctx = { String, Object, Array, JSON, RegExp, console, esc: s => String(s == null ? '' : s) };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.statusWhyHtml = statusWhyHtml;', ctx);
const html = (s, pr) => ctx.statusWhyHtml(s || {}, pr || null);
const chips = h => (h.match(/class="code">([^<]*)</g) || []).map(x => x.replace(/class="code">|</g, ''));
const text = h => (h.match(/class="txt">([^<]*)</) || [])[1] || '';

// 1) 11140 硬禁用：码进胶囊，正文是上游中文原文
let h = html({ disabled: true, disabled_reason: '11140 账号存在异常行为，已暂停（需在 WorkBuddy 内提交反馈申诉解封）' });
assert.deepStrictEqual(chips(h), ['11140'], '业务码胶囊: ' + h);
assert.ok(text(h).indexOf('账号存在异常行为') === 0, '正文应去掉开头的码: ' + text(h));

// 2) 冷却原因 429：状态码也能单独成胶囊
h = html({ cooling: true, reason: '429 rate limit (retry-after)' });
assert.deepStrictEqual(chips(h), ['429'], '429 胶囊: ' + h);
assert.strictEqual(text(h), 'rate limit (retry-after)');

// 3) 手动禁用没有码，只有人话
h = html({ disabled: true, disabled_reason: '面板手动禁用（管理员操作）' });
assert.deepStrictEqual(chips(h), [], '无码时不出胶囊: ' + h);
assert.strictEqual(text(h), '面板手动禁用（管理员操作）');

// 4) 测试失败的上游信封：HTTP 403 + 11140 都要出，正文取 zh
h = html({}, { ok: false, error: 'upstream account_fault (http 403): {"code":11140,"msg":"request illegal","zh":"账号存在异常行为，已暂停"}' });
assert.deepStrictEqual(chips(h), ['HTTP 403', '11140'], '信封码: ' + h);
assert.strictEqual(text(h), '账号存在异常行为，已暂停');

// 5) 网关 401：中文前缀后跟状态码
h = html({ cooling: true, reason: '网关 401 × 3 且 refresh 验证失败（token 已确认失效）' });
assert.deepStrictEqual(chips(h), ['401'], '网关 401 胶囊: ' + h);

// 6) 没有任何原因 → 不渲染
assert.strictEqual(html({}), '');
console.log('status why render passed');`
	path := filepath.Join(t.TempDir(), "statuswhy.cjs")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, "app.js").CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
