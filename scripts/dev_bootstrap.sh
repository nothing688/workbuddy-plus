#!/usr/bin/env bash
# dev_bootstrap.sh 开发环境引导：确保有 Go（缺则下载固定版本）→ 构建 → 准备 config.json。
#
#   scripts/dev_bootstrap.sh          # 只构建
#   scripts/dev_bootstrap.sh --test   # 构建 + 跑全量测试（提交前的闸门）
#
# 为什么需要：CNB 云开发环境等精简镜像里没有 go 和 docker，直接 go build 会失败；
# 这里按 go.mod 里钉的版本下载官方 tarball，装到 $HOME/.local/go，不碰系统目录。
set -euo pipefail

GO_VERSION=1.26.10 # 与 go.mod / Dockerfile 保持一致
ROOT=$(cd "$(dirname "$0")/.." && pwd)
RUN_TESTS=false
[ "${1:-}" = "--test" ] && RUN_TESTS=true

have_go() { command -v go >/dev/null 2>&1; }

if ! have_go; then
  case "$(uname -s)" in
    Linux) GOOS=linux ;;
    Darwin) GOOS=darwin ;;
    *) echo "不支持的系统：$(uname -s)；请手动安装 Go ${GO_VERSION}" >&2; exit 1 ;;
  esac
  case "$(uname -m)" in
    x86_64 | amd64) GOARCH=amd64 ;;
    aarch64 | arm64) GOARCH=arm64 ;;
    *) echo "不支持的架构：$(uname -m)" >&2; exit 1 ;;
  esac

  GO_DIR="${WB2A_GO_DIR:-$HOME/.local/go}"
  if [ ! -x "$GO_DIR/bin/go" ]; then
    echo "未找到 go，下载 go${GO_VERSION}.${GOOS}-${GOARCH} → $GO_DIR"
    mkdir -p "$(dirname "$GO_DIR")"
    tmp=$(mktemp -d)
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.${GOOS}-${GOARCH}.tar.gz" -o "$tmp/go.tgz"
    rm -rf "$GO_DIR"
    tar -C "$(dirname "$GO_DIR")" -xzf "$tmp/go.tgz"
    rm -rf "$tmp"
  fi
  export PATH="$GO_DIR/bin:$PATH"
fi

cd "$ROOT"
echo "使用 $(go version)"

CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$ROOT/wb2api" ./cmd/server
echo "构建完成：$ROOT/wb2api"

if [ ! -f "$ROOT/config.json" ]; then
  cp "$ROOT/config.example.json" "$ROOT/config.json"
  echo "已从 config.example.json 生成 config.json（记得改 api_key）"
fi

if $RUN_TESTS; then
  go build ./... && go test ./...
  echo "测试通过"
fi

cat <<'TIP'

启动：
  ./wb2api -config config.json
面板：
  http://127.0.0.1:7863/panel/    （api_key 见 config.json）

账号凭证放 auths/，运行态落 data/；两者都在 .gitignore 里，别提交。
更多（部署路径 / 目录地图 / 排查手册）见 AGENTS.md。
TIP
