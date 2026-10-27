package usage

import (
	"testing"
	"time"
)

// addHM 往记录器写一条用量（HasTotal/HasCredit 按参数构造）。
func addHM(r *Recorder, ts time.Time, realm, uid, model string, tokens int64, credit float64, ok bool) {
	r.Add(ts, realm, uid, model, Delta{
		HasTotal:    true,
		TotalTokens: tokens,
		HasCredit:   credit > 0,
		Credit:      credit,
	}, ok)
}

// TestHourlyModelsPivot 小时 × 模型聚合：请求数排名取列、TopN 外归「其他」、
// 同小时多账号合并、失败计入请求但不影响 Token/积分。
func TestHourlyModelsPivot(t *testing.T) {
	r := New("")
	now := time.Now().Truncate(time.Hour)

	addHM(r, now, "cn", "u1", "glm-5.2", 100, 2, true)
	addHM(r, now, "cn", "u2", "glm-5.2", 50, 1, true)
	addHM(r, now, "cn", "u1", "hy3", 30, 0, true)
	addHM(r, now, "global", "u3", "gpt-x", 20, 0, true)
	addHM(r, now.Add(-time.Hour), "cn", "u1", "glm-5.2", 10, 0, false)

	got := r.HourlyModels(Window{Hours: 24}, 2)
	if len(got.Models) != 2 {
		t.Fatalf("models=%+v，want 2 列", got.Models)
	}
	if got.Models[0].Key != "cn:glm-5.2" || got.Models[0].Requests != 3 {
		t.Fatalf("第一列=%+v，want cn:glm-5.2/3", got.Models[0])
	}
	if got.Models[1].Key != "cn:hy3" {
		t.Fatalf("第二列=%+v，want cn:hy3", got.Models[1])
	}
	if got.OtherModels != 1 {
		t.Fatalf("other_models=%d，want 1（gpt-x 被折叠）", got.OtherModels)
	}
	if len(got.OtherRefs) != 1 || got.OtherRefs[0].Key != "global:gpt-x" {
		t.Fatalf("other_refs=%+v，want 只含 global:gpt-x", got.OtherRefs)
	}
	if len(got.Hours) != 2 {
		t.Fatalf("hours=%d，want 2（升序两条）", len(got.Hours))
	}
	old := got.Hours[0]
	if old.T != now.Add(-time.Hour).Format(hourLayout) {
		t.Fatalf("第一行=%q，want 上一小时", old.T)
	}
	oc := old.Cells["cn:glm-5.2"]
	if oc.Requests != 1 || oc.Errors != 1 || oc.TotalTokens != 10 {
		t.Fatalf("上一小时 glm 单元格=%+v，want 1 请求/1 失败/10 tok", oc)
	}
	cur := got.Hours[1]
	gc := cur.Cells["cn:glm-5.2"]
	if gc.Requests != 2 || gc.TotalTokens != 150 || gc.Credits != 3 {
		t.Fatalf("当前小时 glm=%+v，want 2/150/3", gc)
	}
	if cur.Other.Requests != 1 || cur.Other.TotalTokens != 20 {
		t.Fatalf("当前小时 other=%+v，want gpt-x 1/20", cur.Other)
	}
	if oc := cur.OtherCells["global:gpt-x"]; oc.Requests != 1 || oc.TotalTokens != 20 {
		t.Fatalf("other_cells=%+v，want gpt-x 明细 1/20", oc)
	}
	if cur.Total.Requests != 4 || cur.Total.TotalTokens != 200 || cur.Total.Credits != 3 {
		t.Fatalf("当前小时合计=%+v，want 4/200/3", cur.Total)
	}
}

// TestHourlyModelsTruncatesLongWindow 超过 14 天只保留最近 14 天并标记 truncated。
func TestHourlyModelsTruncatesLongWindow(t *testing.T) {
	r := New("")
	now := time.Now().Truncate(time.Hour)
	addHM(r, now.Add(-20*24*time.Hour), "cn", "u1", "glm-5.2", 1, 0, true)
	addHM(r, now.Add(-time.Hour), "cn", "u1", "glm-5.2", 2, 0, true)

	got := r.HourlyModels(Window{}, 8)
	if !got.Truncated {
		t.Fatal("全部历史窗口应标记 truncated")
	}
	if len(got.Hours) != 1 || got.Hours[0].T != now.Add(-time.Hour).Format(hourLayout) {
		t.Fatalf("hours=%+v，want 只保留最近 14 天内的 1 条", got.Hours)
	}
}
