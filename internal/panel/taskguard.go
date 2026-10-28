// taskguard.go 任务动作的「无收益重跑」熔断。
//
// 背景（2026-10-28 积分净收益审计）：成长任务队列每天把所有「未领取 + 未锁定」
// 的任务扫进待办，但判据是否点亮由上游决定。动作跑完而进度没动时，
// executeAutoTask 返回 awaiting_progress，次日队列会原样再跑一遍——判据一直
// 点不亮的任务（skill_1 这类 Attempt 项、被反作弊回滚的任务）于是每天白烧一次
// 真实对话，收益恒为 0。单次成本很低，但成本 × 账号数 × 天数会一直累积。
//
// 判据（只对自动队列生效，面板手动执行不受影响）：
//   - 动作已执行、进度快照前后一致 → 记一轮「未点亮」；
//   - 连续 maxStuckRuns 轮未点亮且进度快照仍是那一份 → 队列跳过该任务；
//   - 进度快照一变（服务端补计分、手动跑成功、任务被重置）→ 熔断自动解除，
//     不依赖任何人去点「恢复」，所以不会把「第 4 天才计分」的任务永久拉黑。
package panel

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/safefile"
)

// maxStuckRuns 连续多少轮「动作已跑、进度未动」后从自动队列剔除。
const maxStuckRuns = 2

// taskGuardRecord 单账号单任务的熔断状态。Progress 是熔断时看到的进度快照，
// 用它判断「上游是否给了新证据」——只有快照完全相同才保持熔断。
type taskGuardRecord struct {
	Runs     int    `json:"runs"`
	Progress string `json:"progress"`
	At       string `json:"at"`
}

type taskGuardFile struct {
	Version int                        `json:"version"`
	Records map[string]taskGuardRecord `json:"records"`
}

// taskGuard 并发安全的任务熔断表。path 为空 = 纯内存（不落盘）。
type taskGuard struct {
	mu   sync.Mutex
	path string
	rec  map[string]taskGuardRecord
}

func newTaskGuard(path string) *taskGuard {
	g := &taskGuard{path: path, rec: map[string]taskGuardRecord{}}
	if path == "" {
		return g
	}
	b, err := safefile.Read(path, nil)
	if err != nil {
		return g // 文件不存在/读失败：从空表开始，丢历史好过起不来
	}
	var f taskGuardFile
	if err := json.Unmarshal(b, &f); err != nil {
		log.Printf("panel: 任务熔断表解析失败（按空表继续）: %v", err)
		return g
	}
	if f.Records != nil {
		g.rec = f.Records
	}
	return g
}

func taskGuardKey(uid, code string) string { return uid + "|" + code }

// blocked 该任务是否已被熔断：连续 maxStuckRuns 轮未点亮，且进度仍停在当时的快照。
func (g *taskGuard) blocked(uid, code, progress string) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	r, ok := g.rec[taskGuardKey(uid, code)]
	return ok && r.Runs >= maxStuckRuns && r.Progress == progress
}

// noteNoProgress 记一轮「动作已跑、进度未动」。
func (g *taskGuard) noteNoProgress(uid, code, progress string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	k := taskGuardKey(uid, code)
	r := g.rec[k]
	if r.Progress != progress {
		r.Runs = 0 // 进度快照变了：重新计数
	}
	r.Runs++
	if r.Runs > maxStuckRuns {
		r.Runs = maxStuckRuns
	}
	r.Progress = progress
	r.At = time.Now().Format(time.RFC3339)
	g.rec[k] = r
	g.saveLocked()
}

// clear 进度有推进（或已领奖）时撤掉熔断记录。
func (g *taskGuard) clear(uid, code string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	k := taskGuardKey(uid, code)
	if _, ok := g.rec[k]; !ok {
		return
	}
	delete(g.rec, k)
	g.saveLocked()
}

// saveLocked 落盘（须持锁）。失败只记日志：熔断是省成本的优化，不能影响任务执行。
func (g *taskGuard) saveLocked() {
	if g.path == "" {
		return
	}
	b, err := json.MarshalIndent(taskGuardFile{Version: 1, Records: g.rec}, "", "  ")
	if err != nil {
		log.Printf("panel: 序列化任务熔断表失败: %v", err)
		return
	}
	if err := safefile.WriteKeep(g.path, b, 0o600); err != nil {
		log.Printf("panel: 保存任务熔断表失败: %v", err)
	}
}
