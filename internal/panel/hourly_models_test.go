package panel

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/usage"
)

// TestUsageHourlyModelsEndpoint 图表接口：窗口参数生效、top 生效、返回模型列与小时行。
func TestUsageHourlyModelsEndpoint(t *testing.T) {
	rec := usage.New("")
	now := time.Now().Truncate(time.Hour)
	for i := 0; i < 3; i++ {
		rec.Add(now, "cn", "u1", "glm-5.2", usage.Delta{
			HasTotal: true, TotalTokens: 100, HasCredit: true, Credit: 1,
		}, true)
	}
	rec.Add(now, "cn", "u1", "hy3", usage.Delta{HasTotal: true, TotalTokens: 10}, true)
	p := New(Config{Version: "test", APIKey: "k", Usage: rec, Pool: pool.New("")})

	req := httptest.NewRequest("GET", "/panel/api/usage/hourly-models?hours=6&top=1", nil)
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Models []struct {
			Key      string `json:"key"`
			Requests int64  `json:"requests"`
		} `json:"models"`
		Hours []struct {
			Total struct {
				Requests int64 `json:"requests"`
			} `json:"total"`
			Other struct {
				Requests int64 `json:"requests"`
			} `json:"other"`
		} `json:"hours"`
		OtherModels int `json:"other_models"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != 1 || out.Models[0].Key != "cn:glm-5.2" {
		t.Fatalf("models=%+v，want 只保留请求数最高的 glm", out.Models)
	}
	if out.OtherModels != 1 {
		t.Fatalf("other_models=%d，want 1", out.OtherModels)
	}
	if len(out.Hours) != 1 || out.Hours[0].Total.Requests != 4 || out.Hours[0].Other.Requests != 1 {
		t.Fatalf("hours=%+v，want 1 行 / 合计 4 / 其他 1", out.Hours)
	}
}
