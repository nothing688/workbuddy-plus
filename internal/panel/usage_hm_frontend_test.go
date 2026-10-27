package panel

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestUsageHourModelFrontendRender 「按小时 × 模型」堆叠柱状图的渲染回归：
//   - 图例按模型出项（含「其他 N」），可点选显隐（隐藏项有 off 标记、柱段消失）；
//   - 柱子按模型堆叠，<title> 带模型明细，跨天有日期标签；
//   - 点击某根柱子 → 明细区列出该小时各模型数值；
//   - 高度档位改变 viewBox 高（比例可调）；空数据显示空态。
func TestUsageHourModelFrontendRender(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	script := `const fs=require('fs'),vm=require('vm'),assert=require('assert');
const src=fs.readFileSync(process.argv[2],'utf8');
const a=src.indexOf('/* ── 按小时 × 模型的堆叠柱状图'),b=src.indexOf('/* parsePointTime');
assert(a>=0&&b>a,'柱状图区段定位失败');
const noop=()=>{};
const hosts={
 usHmChart:{innerHTML:'',addEventListener:noop},
 usHmLegend:{innerHTML:'',addEventListener:noop},
 usHmNote:{textContent:'',title:''},
 usHmDetail:{hidden:true,innerHTML:''},
 usHmHeight:{value:'220',onchange:null},
 usHmFull:{innerHTML:'',title:'',onclick:null},
 usHmBox:{classList:{toggle:noop,contains:()=>false}},
};
const ctx={$:id=>hosts[id],esc:s=>String(s),
 fmtTok:n=>String(Math.round(Number(n||0))),fmtCredit:n=>Number(n).toFixed(2),
 document:{addEventListener:noop}};
vm.createContext(ctx);
vm.runInContext(src.slice(a,b)+
 '\n;globalThis.__set=(d,m)=>{usHmData=d;usHmMetric=m};globalThis.__render=renderUsageHourModels;'+
 'globalThis.__sel=t=>{usHmSel=t};globalThis.__height=h=>{usHmHeight=h};globalThis.__hidden=usHmHidden;',ctx);
const cell=(req,tok,cr)=>({requests:req,errors:0,total_tokens:tok,credits:cr});
const data={
 models:[{key:'cn:glm-5.2',realm:'cn',model:'glm-5.2',requests:2},
         {key:'cn:hy3',realm:'cn',model:'hy3',requests:1}],
 hours:[
  {t:'2026-10-26T22',cells:{'cn:glm-5.2':cell(1,100,2)},other:cell(0,0,0),total:cell(1,100,2)},
  {t:'2026-10-27T00',cells:{'cn:glm-5.2':cell(2,150,3),'cn:hy3':cell(1,30,0)},other:cell(1,20,0),other_cells:{'cn:step-3':cell(1,20,0)},total:cell(4,200,3)}
 ],
 other_models:1,other_refs:[{key:'cn:step-3',realm:'cn',model:'step-3',requests:1}],truncated:false
};
ctx.__set(data,'requests');ctx.__render();
let svg=hosts.usHmChart.innerHTML,lg=hosts.usHmLegend.innerHTML;
assert(svg.startsWith('<svg'),'应渲染 SVG');
assert(lg.includes('glm-5.2')&&lg.includes('hy3')&&lg.includes('其他 1'),'图例应含模型与其他：'+lg);
assert.equal((svg.match(/class="usbar"/g)||[]).length,4,'2 根柱子共 4 段：'+svg);
assert(svg.includes('cn:glm-5.2：2 次'),'<title> 应带模型明细');
assert(svg.includes('>10-26<')&&svg.includes('>10-27<'),'跨天日期标签');
assert(svg.includes('峰值 4'),'请求峰值标注');
// 图例点选：隐藏 hy3 → 少一段柱子，图例项带 off。
ctx.__hidden.add('cn:hy3');ctx.__render();
svg=hosts.usHmChart.innerHTML;lg=hosts.usHmLegend.innerHTML;
assert.equal((svg.match(/class="usbar"/g)||[]).length,3,'隐藏 hy3 后应剩 3 段：'+svg);
assert(/class="lg off"[^>]*data-key="cn:hy3"/.test(lg),'隐藏项应有 off 标记：'+lg);
ctx.__hidden.delete('cn:hy3');
// 点击某小时：明细区展开，含该小时各模型数值。
ctx.__sel('2026-10-27T00');ctx.__render();
assert.equal(hosts.usHmDetail.hidden,false,'明细区应展开');
const det=hosts.usHmDetail.innerHTML;
assert(det.includes('2026-10-27 00')&&det.includes('cn:glm-5.2')&&det.includes('合计 4 次'),'明细内容：'+det);
assert(det.includes('cn:step-3'),'「其他」应展开成具体模型：'+det);
assert(det.includes('3.00 积分'),'积分单位应为「积分」：'+det);
// 高度档位改变 viewBox，比例可调。
ctx.__height(320);ctx.__render();
assert(hosts.usHmChart.innerHTML.includes('viewBox="0 0 1200 320"'),'加高档位应改 viewBox 高');
// 右上角放大图标：点击后换成「还原」图标（不出现文字按钮）。
hosts.usHmFull.onclick();
assert(hosts.usHmFull.innerHTML.includes('M2 6h4V2'),'放大后应切换为还原图标：'+hosts.usHmFull.innerHTML);
assert(hosts.usHmFull.title.includes('还原'),'放大后 title 应为还原');
// 空数据：只给空态。
ctx.__set({models:[],hours:[],other_models:0},'requests');ctx.__render();
assert(hosts.usHmChart.innerHTML.includes('所选区间没有按小时数据'),'空态文案');
assert(!hosts.usHmChart.innerHTML.includes('<svg'),'空态不应有 SVG');
console.log('usage hourly-models chart regression passed');`
	path := filepath.Join(t.TempDir(), "usage-hourly-models-chart.cjs")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, "app.js").CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
