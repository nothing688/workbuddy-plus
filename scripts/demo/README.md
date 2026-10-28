# 演示：积分效益（ROI 闸门）实跑

一套**完全离线**的最小环境，用来在没有真实账号的情况下把「成长任务积分的
收益 / 成本」跑通并观察余额变化。**不连接任何真实服务，凭据全是占位字符串。**

```
scripts/demo/
  mock_upstream.py   假上游（python3 标准库，监听 127.0.0.1:18080）
  run_mode.py        按 task_mode 跑一遍并打印计划 / 实测账
  config.json        指向假上游的面板配置（task_mode=roi 起步）
  auths/demo01.json  占位凭据（uid=demo01）
```

## 假上游的积分口径

- 每次真实对话（`POST /v2/chat/completions`）**扣 30 分**（= `schedule.task_unit_chat_cost` 默认值）。
- 遥测上报（`/v2/report`、桌面事件、外观设置等）**不扣分**——与真实上游一致。
- 任务奖励：只对仓库里实证过奖励值的任务下发 `reward_credit`
  （`first_buddy +300`、`create_canvas +300`、`expert_use_* +50`、
  `school_season +100`、`Sequential_Tasks_1/2/3/7 +100/+200/+300/+500`），
  其余任务**不下发奖励字段**——正是线上「不知道这个任务值多少分」的情形。
- 领奖即入账（余额加回该任务奖励）。
- 对话 SSE 的 `id` 必须是 `cmb-`+32 hex（`internal/upstream` 的 `idRegex` 口径），
  否则专家「召唤+使用」链拿不到服务端 requestId，会如实报
  `SSE 中未找到服务端 requestId`。

## 跑法

```bash
python3 mock_upstream.py &            # 假上游
../../workbuddy-plus                  # 面板（cwd = scripts/demo）

python3 run_mode.py roi               # 三选一：roi / free / all
```

`run_mode.py` 会改写 `config.json` 的 `schedule.task_mode` 并重启面板（配置只在启动时读），
复位假上游账本，先打印 `GET /panel/api/accounts/<uid>/tasks/plan` 的预估，再跑
`tasks/auto_all` 并打印实测花费 / 到账 / 净收益。

## 实测结果（2026-10-28，起始余额 3000，单次对话成本 30 分）

| 模式 | 计划对话数 | 实际对话 | 花费 | 到账 | 净收益 | 余额 |
|---|---|---|---|---|---|---|
| `all`  | 19 | 16 | 480 | 2000 | **+1520** | 4520 |
| `free` | 0  | 0  | 0   | 1800 | **+1800** | 4800 |
| `roi`  | 4  | 4  | 120 | 2000 | **+1880** | 4880 |

三条结论：

1. `all` 把「奖励未知」的计费任务也全跑完，比 `free` 多花 480 分却多赚不到 200 分
   （2 个任务在 mock 里恰好没有奖励字段，线上同理）——**满量跑是负优化**，与
   2026-10-27 事故（单号一天净亏 520–666 分）的方向一致。
2. `free` 完全不花钱，但也放弃了 `expert_use_*` 这类「小奖励 + 单次对话」的薄利任务。
3. `roi` 花 120 分把奖励拿满（到账与 `all` 相同 2000），净收益最高；它对每个计费动作
   先算「上游 `reward_credit` − 预估成本」，奖励字段缺失的任务直接跳过并在结果里写明
   判据（`status=skipped_roi`），所以跳过不是静默丢任务。
