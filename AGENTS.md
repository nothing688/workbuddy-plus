# 版本发布

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

# 上游同步

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
