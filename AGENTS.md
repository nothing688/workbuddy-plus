# AGENTS.md — 给接手的 agent 的作业手册

本文件是**给自动化 agent 看的**：怎么把仓库跑起来、代码在哪、改哪里、哪些不能碰。
面向人（安装 / 面板截图 / 功能清单）的说明在 `README.md`。

## 这是什么

WorkBuddy2API：把多个 WorkBuddy 账号聚合成 OpenAI 兼容 API 的网关，附带内嵌管理面板
（账号池、定时签到/保活/任务自动化、用量统计、在线改配置）。

- 后端 Go 单二进制，前端 `index.html` / `app.js` 通过 `go:embed` 打包，**无前端构建步骤**
- 运行需要一个 `config.json` + `auths/` 账号目录 + `data/` 运行态目录
- 默认监听 `:7863`，面板在 `/panel/`

## 5 分钟跑起来

```bash
# 1. 一条龙（新环境没有 Go 时自动下载固定版本，然后构建）
scripts/dev_bootstrap.sh              # 加 --test 会顺带跑全量测试

# 2. 等价的手工步骤
go build -trimpath -ldflags="-s -w" -o wb2api ./cmd/server   # Go >= 1.26.10（见 go.mod）
cp config.example.json config.json                            # 首次：没有配置就从样例复制
./wb2api -config config.json                                  # 面板 http://127.0.0.1:7863/panel/

# 3. 闸门（提交前必跑）
go build ./... && go test ./...
```

- `config.json` 里的 `api_key` 是面板与 `/v1/*` 共用的鉴权；为空 = 不鉴权（仅限本机）
- `auths/` 放账号凭证（`workbuddy-<uid>.json`），**空目录也能起**，只是没号可用
- 没有 `node` 时前端两条 JS 用例自动跳过（`TestAppJSSyntax` / `TestAppJSTopLevelSmoke`），
  有 node 才会真正校验 `app.js`

## 三种部署路径

| 场景 | 做法 |
|---|---|
| 服务器（推荐，长期） | `docker compose -f docker-compose.prod.yml up -d`，拉 CNB 镜像源 + watchtower 自动更新，见「部署与链路」 |
| CNB 云开发环境（临时公网预览） | 环境内跑 `./wb2api -config config.json`，端口通过 `CNB_VSCODE_PROXY_URI`（形如 `https://<workspace>-{{port}}.cnb.run`）自动暴露公网；环境重启后进程需重拉 |
| 本地开发 | `go run ./cmd/server -config config.json` |

开发环境里常见的两个坑：**没有 docker / go**（用 `scripts/dev_bootstrap.sh` 装 Go）、
**`/tmp` 会被清理**（构建产物别放 `/tmp`，放仓库根或 `~`）。

## 目录地图

| 路径 | 职责 |
|---|---|
| `cmd/server/` | 唯一装配点 `main.go` + `config.go`（配置 schema/默认值/热更新） |
| `internal/server/` | 网关 HTTP：chat completions、responses、messages、搜索工具、续写、starved 救援、鉴权 |
| `internal/upstream/` | 上游协议层：对话/SSE、任务、余额、签到、旅行、夜猫子、成长、出图、报告事件 |
| `internal/pool/` | 账号池：选号、冷却/熔断/降权、分组、`state.json` 持久化 |
| `internal/scheduler/` | 定时任务：签到、旅行、活跃上报、保活、夜猫子、成长队列、补领清扫、连登管家 |
| `internal/panel/` | 面板后端 + 前端资源（`index.html` / `app.js` 同目录，`go:embed`） |
| `internal/panel/autotask.go` | 任务动作表 `autoActions` + 统一执行器 `executeAutoTask` |
| `internal/panel/taskcenter.go` | 成长队列：扫描待办 → 排队 → 执行（`RunGrowthQueueNow`） |
| `internal/panel/taskjobs.go` | 持久化后台作业（`daily` / `new_account` / `manual` 三种触发） |
| `internal/panel/tasksweep.go` | 补领清扫（只 accept / claim，不跑动作） |
| `internal/panel/taskguard.go` | 「无收益重跑」熔断表 |
| `internal/panel/taskpause.go` | 「暂停任务」三档开关（全局 / 分组 / 单号） |
| `internal/{auth,balance,usage,reqlog,credithist,updater,apikeys,livecfg}/` | 凭证、余额采样、用量、请求日志、积分账本、自更新、子密钥、热配置 |
| `workbuddy2api-panel/`、`workbuddy2api-panel-plus/` | **上游快照目录，只读**，基线见 `scripts/upstreams.txt` |
| `scripts/` | 上游同步、任务脚本、探测工具、`dev_bootstrap.sh` |

## 面板任务体系的三道闸（改之前先读）

1. **暂停任务**（`taskpause.go`）：全局 / 分组 / 单号任一命中，跳过成长队列、新号自动任务、
   夜猫子；签到 / 活跃 / 旅行 / 保活 / 余额刷新 / 补领清扫照常。手动入口不受限。
2. **无收益重跑熔断**（`taskguard.go`）：动作跑完进度没动记一轮，连续 2 轮从自动队列剔除，
   进度快照一变自动恢复。状态在 `data/task_attempts.json`。
3. **清扫退避**（`tasksweep.go`）：全局防重入 + 同一任务连续 3 次领奖失败退避 6 小时。

三者都只在**自动路径**生效（`queued=true`）；面板手动「单任务 / 一键完成」永远放行。
判断某个账号是否该停任务，用 `Panel.AccountTasksPaused(uid)`，不要各写一套。

## 继续优化的下手点

| 想加什么 | 改哪里 |
|---|---|
| 新的任务动作 | `internal/panel/autotask.go` 的 `autoActions` 表 + 一个 `runXxx` 函数 + `internal/panel/*_test.go` |
| 新的定时任务 | `internal/scheduler/scheduler.go` 的 `Config` + `runBatch` 分支 + `cmd/server/main.go` 装配 |
| 新的面板接口 | `internal/panel/panel.go` 的 `routes()` + handler + `app.js` 调用（注意 `TestAppJSIdsExistInHTML` 要求 id 一致） |
| 新的上游端点 | `internal/upstream/client.go` 的 base/paths 常量集中定义，别在业务代码里拼 URL |
| 新的调度配置项 | `cmd/server/config.go` 的 `Schedule` + 默认值 + 面板表单（`index.html`）+ `app.js` 的 `CFG_MAP` |
| 新面板视图 | `internal/panel/index.html` 的 `<section class="view">` + `app.js` 的 `go()` 路由 |

约定：新增行为要配**能失败的测试**（不是只有 happy path）；上游协议细节写进代码注释，
解释「为什么这么判」，不要只写「做什么」。

## 红线

- **不要提交** `auths/`、`data/`、`config.json`、`API_KEY.txt`——`.gitignore` 已挡
- `.gitignore` 里 `*.md` 只放行 `README.md`、`AGENTS.md`、`defaultprompt.md`；新文档请并进这两个文件
  （或先改 `.gitignore`，否则文件根本进不了版本库）
- **不要改** `workbuddy2api-panel/`、`workbuddy2api-panel-plus/`：它们是上游快照，
  `scripts/sync_upstream.sh` 会覆盖；需要改就改顶层对应文件
- 不要手改发版用的 `appVersion`；本地构建不注入（面板显示 `dev`）
- 不要在面板里做「更新自己 / 同步上游」这类需要 git 或源码树的功能——发布镜像里没有

## 环境坑（都踩过）

| 现象 | 原因与做法 |
|---|---|
| 打的版本号比实际早 8 小时 | Windows/git-bash 无 tzdata，`TZ=Asia/Shanghai date` 静默回落 UTC；按「版本发布」里的显式 +8h 写法 |
| `pgrep -f 'wb2api' \| xargs kill` 把自己杀了 | `-f` 会匹配到当前 shell 命令行；用 `pgrep -x wb2api` |
| 新环境 `/tmp` 里的东西和 gh 登录态消失 | `/tmp` 会被清；凭证/二进制放持久位置，或重新登录 |
| 假凭证账号每轮刷 WARN | 定时任务会定期查余额/签到；纯看界面时先把 `schedule.*_enabled` 关掉 |
| 面板一片「冷却 / 登录失效」 | 上游 401/12153 的既有语义，不是面板 bug；先看 `data/server.log` 的 `WARN:` 行 |
| 提交被 `.gitignore` 吃掉 | 见「红线」，尤其 `*.md` 与 `docs/` |

## 排查手册

运行态都在 `data/`：

| 文件 | 含义 |
|---|---|
| `state.json` | 账号池状态：积分、冷却、分组、禁用原因（掉电有 `.bak`） |
| `task-runs.json` | 启动补跑去重（每个时点每天只补一次） |
| `task-jobs.json` | 后台任务作业的进度与结果 |
| `task_attempts.json` | 无收益重跑熔断表 |
| `task-pause.json` | 任务暂停三档名单 |
| `credit-history.json` | 积分变动流水 |
| `usage.json` / `request-logs/` | 用量与请求归档 |
| `server.log` | 服务日志（部署时的 stdout 也在这里） |

常用关键字：`WARN: checkin`、`WARN: activity`、`WARN: 补领`、`blackcat`、`streak-bonus`、
`growth 后台任务`、`panel: task auto`、`补领清扫：`、`启动补跑`。

## 版本发布

不跟上游 `1.x`。发版是打时间标签并推送，工作流看到 `v*` 才创建 Release。

版本号是上海时间，形如 `v2026.9.26.711`：

- `2026.9.26` 是日期，月和日不补零
- `711` 是 7:11，时不补零、分两位、不带秒
- 标签、Release、二进制用同一个号；二进制里不带 `v`，面板会自己加
- 不要打 `v1.x` 标签，也不要手改 `appVersion`

```bash
# 注意：本机（Windows / git-bash）没有 tzdata，`TZ=Asia/Shanghai date` 会**静默回落 UTC**，
# 打出来的标签整整早 8 小时。用显式加 8 小时，兼容有无 tzdata 两种环境：
VERSION=$(TZ=Asia/Shanghai date +%Y.%-m.%-d.%-H%M)
[ "$(TZ=UTC date +%H)" = "$(TZ=Asia/Shanghai date +%H)" ] && VERSION=$(date -u -d '+8 hours' +%Y.%-m.%-d.%-H%M)
git tag "v$VERSION"
git push origin "v$VERSION"
```

本地构建不注入，面板显示 `dev`。同一分钟再发才会撞号。

## 上游同步

顶层代码由两个来源项目合并而来，它们以纯快照目录内嵌：`workbuddy2api-panel/`（misakano7545）
和 `workbuddy2api-panel-plus/`（JACKY199503）。快照对应的上游 commit 记在
`scripts/upstreams.txt`，是三方合并的基线。

```bash
scripts/sync_upstream.sh                 # 只出差异报告，不改文件
scripts/sync_upstream.sh --auto          # 一键：合并 + 刷新基线 + 编译测试 + 自动提交
scripts/sync_upstream.sh --auto --push   # 同上并推送
```

冲突时结果写到 `<文件>.merge`（带冲突标记），快照和基线**不会前进**，处理完重跑；
`--auto` 只在工作区干净时运行，构建/测试失败则保留改动不提交。

仓库页「代码-分支详情」有「同步上游」按钮（`.cnb/web_trigger.yml` + `.cnb.yml`），
点一下在构建环境跑同一套 `--auto` 流程并可选推回 main；不要把该功能放进面板——
发布镜像里没有 git 和源码树。

## 部署与链路

`main` 有提交（包括面板/按钮触发的同步推送）→ `.cnb.yml` 的 `main.push` 构建镜像并推到
`docker.cnb.cool/nothing-2026/workbuddy-plus`（标签 `latest` + `sha-<8位>`）。
GitHub 侧 `.github/workflows/` 另出四平台二进制与 `ghcr.io/nothing688/workbuddy-plus`。

服务器首次部署：

```bash
docker login docker.cnb.cool -u cnb -p <CNB 访问令牌>
docker compose -f docker-compose.prod.yml up -d
```

之后 watchtower 每 5 分钟检查 `latest`，有更新自动拉取重建。想手动更新：
`docker compose -f docker-compose.prod.yml pull && docker compose -f docker-compose.prod.yml up -d`。

GitHub 镜像源用覆盖文件组合：
`docker compose -f docker-compose.prod.yml -f docker-compose.ghcr.yml up -d`。

服务器部署时 `config.json` / `auths/` / `data/` 由 compose 挂载宿主机目录，**升级不丢**；
注意挂载目录属主与容器内 `PUID/PGID`（缺省 10001）一致，否则写盘失败。

## 提交与推送

- 提交信息：`feat|fix|docs|chore(scope): 中文摘要`，正文说清「为什么」和边界
- 两个远端：`origin`（CNB，触发镜像构建）与 `github`（触发 Actions），两边都推
- 推送前：工作区干净、`go build ./... && go test ./...` 通过、不夹带 `auths/` 与 `data/`
