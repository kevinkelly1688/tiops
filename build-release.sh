#!/usr/bin/env bash
# 打包 TiDB Ops 发布包（产物是 Linux tar.gz，可在 Linux / macOS / Git Bash 上运行）。
# 用法：
#   ./build-release.sh                                  # 版本取 VERSION 文件，架构 amd64+arm64，输出到 backend/dist/release
#   ./build-release.sh -v 1.2.0 -a amd64
#   ./build-release.sh -v 1.2.0 -n notes.md -m 1.0.0 -o /tmp/release
#   ./build-release.sh --verify backend/dist/release/tidbops-1.2.0-linux-amd64.tar.gz
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VERSION=""; ARCH="amd64,arm64"; OUT=""; NOTES=""; MIN_FROM=""; DEPLOY_TOOL=""; VERIFY=""

usage() { sed -n '2,8p' "$0"; exit "${1:-0}"; }
while [ $# -gt 0 ]; do
  case "$1" in
    -v|--version) VERSION="$2"; shift 2;;
    -a|--arch) ARCH="$2"; shift 2;;
    -o|--out) OUT="$2"; shift 2;;
    -n|--notes) NOTES="$2"; shift 2;;
    -m|--min-from) MIN_FROM="$2"; shift 2;;
    --deploy-tool) DEPLOY_TOOL="$2"; shift 2;;
    --verify) VERIFY="$2"; shift 2;;
    -h|--help) usage 0;;
    *) echo "未知参数 $1" >&2; usage 1;;
  esac
done

export GOTOOLCHAIN="${GOTOOLCHAIN:-local}" GOFLAGS="${GOFLAGS:--mod=mod}" GOPROXY="${GOPROXY:-off}"
cd "$ROOT/backend"

if [ -n "$VERIFY" ]; then
  exec go run ./cmd/tidbops-pack --verify "$(cd "$(dirname "$VERIFY")" && pwd)/$(basename "$VERIFY")"
fi

[ -n "$OUT" ] || OUT="$ROOT/backend/dist/release"
args=(--web "$ROOT" --arch "$ARCH" --out "$OUT")
[ -z "$VERSION" ] || args+=(--version "$VERSION")
[ -z "$NOTES" ] || args+=(--notes "$(cd "$(dirname "$NOTES")" && pwd)/$(basename "$NOTES")")
[ -z "$MIN_FROM" ] || args+=(--min-from "$MIN_FROM")
[ -z "$DEPLOY_TOOL" ] || args+=(--deploy-tool "$DEPLOY_TOOL")
exec go run ./cmd/tidbops-pack "${args[@]}"
