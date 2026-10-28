// taskpause.go 任务暂停开关：一键停掉「会消耗积分」的自动任务，保留签到等回血链路。
//
// 背景（2026-10-28）：成长任务队列与夜猫子要靠真实对话点亮判据，额度紧张时用户
// 想的是「停止做任务、只保留每日签到回血」，而不是逐个去关 schedule.* 开关。
//
// 三档作用域（从粗到细，任一命中即暂停）：
//   - 全局：任务中心「暂停任务」按钮；
//   - 分组：账号池按分组批量暂停（新加入该分组的账号自动继承）；
//   - 单号：账号池行内「停任务」/ 多选批量。
//
// 语义（暂停期间）：
//   - 停：成长任务队列（每日排程）、新账号自动跑任务、夜猫子夜间对话
//   - 不停：签到 / 活跃上报 / 猫猫旅行 / token 保活 / 余额刷新（这些不花积分，
//     签到还是唯一的自动回血路径）、补领清扫（只领不发，纯收益）
//   - 手动入口不受限：面板「执行全部待办」「单任务」「一键完成」照旧可点，
//     暂停只挡自动触发，避免用户想手动补一个任务时被自己的开关挡住。
//
// 状态落盘到 <DataDir>/task-pause.json（safefile 原子写），重启后保持。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/safefile"
)

// taskPauseFile 暂停状态。Accounts/Groups 是显式名单（暂停 = 在名单里）。
type taskPauseFile struct {
	Version  int      `json:"version"`
	Paused   bool     `json:"paused"`
	At       string   `json:"at,omitempty"`
	Accounts []string `json:"accounts,omitempty"`
	Groups   []string `json:"groups,omitempty"`
}

type taskPause struct {
	mu   sync.Mutex
	path string
	st   taskPauseFile
}

func newTaskPause(path string) *taskPause {
	t := &taskPause{path: path, st: taskPauseFile{Version: 2}}
	if path == "" {
		return t
	}
	b, err := safefile.Read(path, nil)
	if err != nil {
		return t // 不存在/读失败：默认未暂停（暂停是安全兜底，不能因读不到文件就锁死任务）
	}
	var f taskPauseFile
	if err := json.Unmarshal(b, &f); err != nil {
		log.Printf("panel: 任务暂停状态解析失败（按未暂停继续）: %v", err)
		return t
	}
	t.st = f
	t.st.Version = 2
	return t
}

// Paused 全局是否暂停自动任务。
func (t *taskPause) Paused() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.st.Paused
}

// AccountPaused 该账号是否被单号暂停。
func (t *taskPause) AccountPaused(uid string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, v := range t.st.Accounts {
		if v == uid {
			return true
		}
	}
	return false
}

// GroupPaused 该分组是否被暂停（空分组名不参与：未分组不是可批量管理的对象）。
func (t *taskPause) GroupPaused(group string) bool {
	if t == nil || group == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, v := range t.st.Groups {
		if v == group {
			return true
		}
	}
	return false
}

// Snapshot 当前三档状态（overview / GET 接口透出）。
func (t *taskPause) Snapshot() (paused bool, accounts, groups []string) {
	if t == nil {
		return false, nil, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.st.Paused, append([]string(nil), t.st.Accounts...), append([]string(nil), t.st.Groups...)
}

// Set 切换全局暂停并落盘。
func (t *taskPause) Set(paused bool) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.st.Paused = paused
	t.st.At = time.Now().Format(time.RFC3339)
	return t.saveLocked()
}

// SetScope 按名单批量暂停/恢复：paused=true 时并入名单，false 时移出。
func (t *taskPause) SetScope(accounts, groups []string, paused bool) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.st.Accounts = mergeNames(t.st.Accounts, accounts, paused)
	t.st.Groups = mergeNames(t.st.Groups, groups, paused)
	t.st.At = time.Now().Format(time.RFC3339)
	return t.saveLocked()
}

// mergeNames 把 add 并入/移出 list，返回排序去重后的副本。
func mergeNames(list, add []string, paused bool) []string {
	set := make(map[string]bool, len(list)+len(add))
	for _, v := range list {
		if v != "" {
			set[v] = true
		}
	}
	for _, v := range add {
		if v == "" {
			continue
		}
		if paused {
			set[v] = true
		} else {
			delete(set, v)
		}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// saveLocked 落盘（须持锁）。失败返回错误，调用方决定是否提示。
func (t *taskPause) saveLocked() error {
	if t.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(t.st, "", "  ")
	if err != nil {
		return err
	}
	if err := safefile.WriteKeep(t.path, b, 0o600); err != nil {
		log.Printf("panel: 保存任务暂停状态失败: %v", err)
		return err
	}
	return nil
}

// AccountTasksPaused 该账号是否被任意一档「暂停任务」覆盖（全局 / 分组 / 单号）。
// 排程侧（夜猫子）与后台作业都走这一个判据，避免三处各写一套。
func (p *Panel) AccountTasksPaused(uid string) bool {
	if p.taskPause.Paused() {
		return true
	}
	if p.taskPause.AccountPaused(uid) {
		return true
	}
	if p.cfg.Pool == nil {
		return false
	}
	st, ok := p.cfg.Pool.Status(uid)
	return ok && p.taskPause.GroupPaused(st.Group)
}

// TasksPaused 全局暂停态（任务中心按钮用）。
func (p *Panel) TasksPaused() bool { return p.taskPause.Paused() }

// taskPauseAccounts / taskPauseGroups overview 透出用的名单副本。
func (p *Panel) taskPauseAccounts() []string {
	_, accounts, _ := p.taskPause.Snapshot()
	return accounts
}

func (p *Panel) taskPauseGroups() []string {
	_, _, groups := p.taskPause.Snapshot()
	return groups
}

// taskPauseGet GET /panel/api/tasks/pause —— 三档状态。
func (p *Panel) taskPauseGet(w http.ResponseWriter, _ *http.Request) {
	paused, accounts, groups := p.taskPause.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"paused": paused, "accounts": accounts, "groups": groups,
	})
}

// taskPauseSet POST /panel/api/tasks/pause {"paused":true|false} —— 切换全局。
func (p *Panel) taskPauseSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paused *bool `json:"paused"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Paused == nil {
		writeErr(w, http.StatusBadRequest, "paused required")
		return
	}
	if err := p.taskPause.Set(*body.Paused); err != nil {
		// 内存状态已生效，只是没落盘——明确告知，避免重启后「自己恢复了」而无从解释。
		log.Printf("panel: 任务暂停 → %v（落盘失败）: %v", *body.Paused, err)
		writeErr(w, http.StatusInternalServerError, "状态已生效但落盘失败："+err.Error())
		return
	}
	if *body.Paused {
		log.Printf("panel: 自动任务已暂停（成长队列 / 新号自动任务 / 夜猫子）；签到等回血任务照常")
	} else {
		log.Printf("panel: 自动任务已恢复")
	}
	writeJSON(w, http.StatusOK, map[string]any{"paused": *body.Paused})
}

// taskPauseBatch POST /panel/api/tasks/pause/batch
// {"accounts":["uid"],"groups":["组名"],"paused":true|false} —— 按账号 / 分组批量暂停。
func (p *Panel) taskPauseBatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Accounts []string `json:"accounts"`
		Groups   []string `json:"groups"`
		Paused   *bool    `json:"paused"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Paused == nil {
		writeErr(w, http.StatusBadRequest, "paused required")
		return
	}
	if len(body.Accounts) == 0 && len(body.Groups) == 0 {
		writeErr(w, http.StatusBadRequest, "accounts 或 groups 至少给一个")
		return
	}
	if err := p.taskPause.SetScope(body.Accounts, body.Groups, *body.Paused); err != nil {
		log.Printf("panel: 批量任务暂停（落盘失败）: %v", err)
		writeErr(w, http.StatusInternalServerError, "状态已生效但落盘失败："+err.Error())
		return
	}
	verb := "暂停"
	if !*body.Paused {
		verb = "恢复"
	}
	log.Printf("panel: 批量%s任务：账号 %d 个 / 分组 %d 个（只挡自动任务；签到等回血照常）",
		verb, len(body.Accounts), len(body.Groups))
	paused, accounts, groups := p.taskPause.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"paused": paused, "accounts": accounts, "groups": groups,
	})
}
