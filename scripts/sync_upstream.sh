#!/usr/bin/env bash
# sync_upstream.sh — 把两个上游仓库的更新三方合并进顶层合并主树。
#
# 原理：顶层代码 = 两份上游快照（workbuddy2api-panel*/）的手工合并。
# 快照目录本身永不修改，它就是"共同祖先"（base）；上游新版本是 theirs，
# 顶层合并树是 ours，逐文件做 git merge-file 三方合并。
#
# 用法：
#   scripts/sync_upstream.sh              # 只出报告，不改任何文件
#   scripts/sync_upstream.sh --apply      # 干净合并直接写入，冲突留标记(.merge)待人工处理
#   scripts/sync_upstream.sh --apply --refresh   # 同时刷新快照目录并更新基线 commit
#   scripts/sync_upstream.sh --auto       # 一键：同步 + 刷新基线 + 编译测试 + 自动提交
#   scripts/sync_upstream.sh --auto --push       # 一键并推送到 origin
#
# 退出码：0 = 全部干净 / 无更新；2 = 存在需要人工处理的冲突；3 = 构建或测试失败。
set -uo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
META="${UPSTREAMS_FILE:-$ROOT/scripts/upstreams.txt}"
APPLY=0
REFRESH=0
AUTO=0
PUSH=0
for a in "$@"; do
  case "$a" in
    --apply) APPLY=1 ;;
    --refresh) REFRESH=1 ;;
    --auto) AUTO=1; APPLY=1; REFRESH=1 ;;
    --push) PUSH=1 ;;
    -h|--help) sed -n '2,16p' "$0"; exit 0 ;;
    *) echo "未知参数: $a" >&2; exit 1 ;;
  esac
done

[ -f "$META" ] || { echo "缺少基线文件 $META" >&2; exit 1; }
command -v git >/dev/null || { echo "需要 git" >&2; exit 1; }
command -v diff >/dev/null || { echo "需要 diff" >&2; exit 1; }

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# 一键模式要求工作区干净，否则无法区分「同步带来的改动」和「你自己没提交的改动」。
if [ "$AUTO" = 1 ] && [ -n "$(git -C "$ROOT" status --porcelain 2>/dev/null)" ]; then
  echo "!! 工作区有未提交改动，一键模式已中止。请先 commit 或 stash 后再运行。" >&2
  exit 4
fi
updated=""

total_clean=0
total_conflict=0
total_new=0
failed_entries=""

merge_one() { # $1=来源名 $2=相对路径 $3=上游新文件 $4=快照旧文件
  local name=$1 rel=$2 theirs=$3 base=$4 ours="$ROOT/$2" out
  if [ ! -e "$ours" ]; then
    printf '  [新增] %s\n' "$rel"
    if [ "$APPLY" = 1 ]; then
      mkdir -p "$(dirname "$ours")"
      cp "$theirs" "$ours"
    fi
    total_new=$((total_new+1))
    return
  fi
  if [ ! -e "$base" ]; then
    printf '  [跳过·无基线] %s（顶层已有同名文件，但快照中不存在，需人工比对）\n' "$rel"
    total_conflict=$((total_conflict+1))
    SRC_CONFLICT=$((SRC_CONFLICT+1))
    failed_entries="$failed_entries  $name: $rel (无基线)\n"
    return
  fi
  out="$TMP/merged"
  git merge-file -L "顶层/$rel" -L "基线/$rel" -L "上游/$rel" -p -- "$ours" "$base" "$theirs" >"$out" 2>/dev/null
  case $? in
    0) printf '  [自动合并] %s\n' "$rel"
       [ "$APPLY" = 1 ] && cp "$out" "$ours"
       total_clean=$((total_clean+1)) ;;
    *)
      printf '  [冲突]     %s\n' "$rel"
      if [ "$APPLY" = 1 ]; then
        cp "$out" "$ours.merge"
        printf '             → 带冲突标记的结果写到 %s.merge\n' "$rel"
      fi
      total_conflict=$((total_conflict+1))
      SRC_CONFLICT=$((SRC_CONFLICT+1))
      failed_entries="$failed_entries  $name: $rel\n"
      ;;
  esac
}

while IFS=$'\t' read -r name repo dir base; do
  case "$name" in ''|'#'*) continue ;; esac
  snapshot="$ROOT/$dir"
  [ -d "$snapshot" ] || { echo "!! 快照目录不存在: $dir（跳过 $name）"; continue; }
  [ -d "$snapshot/.git" ] && { echo "!! $dir 是嵌套 git 仓库，本脚本只支持纯快照目录"; continue; }

  echo "== $name: $repo"
  head=$(git ls-remote "$repo" HEAD 2>/dev/null | awk '{print $1}')
  if [ -z "$head" ]; then
    echo "   !! 拉取失败，跳过"; failed_entries="$failed_entries  $name: <ls-remote 失败>\n"; total_conflict=$((total_conflict+1)); continue
  fi
  if [ "$head" = "$base" ]; then
    echo "   已是基线 $head，无更新"; continue
  fi
  echo "   基线 $base → 上游 $head"
  updated="$updated $name@$(printf '%.7s' "$head")"

  work="$TMP/$name"
  if ! git clone -q --depth 1 "$repo" "$work" 2>/dev/null; then
    echo "   !! clone 失败，跳过"; failed_entries="$failed_entries  $name: <clone 失败>\n"; total_conflict=$((total_conflict+1)); continue
  fi
  rm -rf "$work/.git"   # 只做文件比对，去掉 .git 避免噪声

  # 1) 上游新增/修改/删除的文件（name-status：M/D 给快照侧路径，A 给上游侧路径）
  SRC_CONFLICT=0
  git diff --no-index --name-status -- "$snapshot" "$work" >"$TMP/status" 2>/dev/null
  while IFS=$'\t' read -r st p1 p2; do
    [ -z "${st:-}" ] && continue
    case "$st" in
      M*|T*)
        rel=${p1#"$snapshot"/}; rel=${rel#"$work"/}
        merge_one "$name" "$rel" "$work/$rel" "$snapshot/$rel" ;;
      A*)
        rel=${p1#"$work"/}; rel=${rel#"$snapshot"/}
        merge_one "$name" "$rel" "$work/$rel" "" ;;
      D*)
        printf '  [上游已删除] %s（顶层保留，人工确认）\n' "${p1#"$snapshot"/}"
        SRC_CONFLICT=$((SRC_CONFLICT+1)) ;;
      R*|C*)
        printf '  [上游重命名/复制] %s → %s（人工确认）\n' "${p1#"$snapshot"/}" "${p2#"$work"/}"
        SRC_CONFLICT=$((SRC_CONFLICT+1)) ;;
    esac
  done <"$TMP/status"

  # 3) 刷新快照 + 更新基线（仅 --refresh；有未解决项时不刷新，避免丢失差异）
  if [ "$APPLY" = 1 ] && [ "$REFRESH" = 1 ]; then
    if [ "$SRC_CONFLICT" -eq 0 ]; then
      rm -rf "$snapshot" && cp -a "$work" "$snapshot"
      awk -F'\t' -v n="$name" -v c="$head" 'BEGIN{OFS="\t"} $1==n{$4=c} {print}' "$META" >"$META.tmp" && mv "$META.tmp" "$META"
      echo "   已刷新快照并更新基线 → $head"
    else
      echo "   有 $SRC_CONFLICT 个未解决项，本次不刷新快照/基线；处理完重跑 --apply --refresh"
    fi
  fi
done <"$META"

echo
if [ "$total_conflict" -eq 0 ]; then
  echo "完成：自动合并 $total_clean 个文件，新增 $total_new 个，无冲突。"
  if [ "$AUTO" = 1 ]; then
    updated=${updated# }
    if [ -z "$updated" ]; then echo "上游无更新，无需提交。"; exit 0; fi
    if [ -z "$(git -C "$ROOT" status --porcelain)" ]; then echo "无文件变化，无需提交。"; exit 0; fi
    echo "== 编译与测试 =="
    if ! ( cd "$ROOT" && export PATH="$PATH:/usr/local/go/bin" && go build ./... && go vet ./... && go test ./... >/dev/null 2>&1 ); then
      echo "!! 构建或测试失败，改动已保留、未提交。修复后手动提交或重跑。" >&2
      exit 3
    fi
    git -C "$ROOT" add -A
    git -C "$ROOT" commit -q -m "chore: sync upstream($updated)" && echo "已提交：chore: sync upstream($updated)"
    if [ "$PUSH" = 1 ]; then
      git -C "$ROOT" push origin HEAD && echo "已推送到 origin"
    else
      echo "（未推送；加 --push 可一并推送）"
    fi
    exit 0
  fi
  [ "$APPLY" = 1 ] || echo "（这是空跑报告，加 --apply 落盘）"
  exit 0
fi
echo "完成：自动合并 $total_clean 个，新增 $total_new 个，需人工处理 $total_conflict 个："
printf '%b' "$failed_entries"
echo
echo "处理后运行： go build ./... && go vet ./... && go test ./..."
exit 2
