#!/usr/bin/env bash
# 使用 setup-go 的持久编译缓存；每个架构必须在同架构 runner 上构建。
set -euo pipefail
cd "$(dirname "$0")/.."
buildArch=${1:?需要 amd64 或 arm64}
buildVersion=${2:?需要提交或发布版本}
[[ "$buildArch" == amd64 || "$buildArch" == arm64 ]] || { echo '不支持的镜像架构。' >&2; exit 1; }
[[ "$(go env GOHOSTARCH)" == "$buildArch" ]] || { echo '镜像构建必须使用原生架构 runner。' >&2; exit 1; }
if [[ -r /proc/meminfo ]] && command -v nproc >/dev/null 2>&1; then
  printf '原生构建架构=%s，CPU=%s\n' "$buildArch" "$(nproc)"
  awk '/MemTotal/ { printf "构建内存=%.0f MiB\n", $2 / 1024 }' /proc/meminfo
fi
mkdir -p ".ci/bin/$buildArch"
CGO_ENABLED=0 GOOS=linux GOARCH="$buildArch" go build -trimpath -ldflags="-s -w -X main.version=$buildVersion" -o ".ci/bin/$buildArch/lxsc" ./cmd/lxsc
go version -m ".ci/bin/$buildArch/lxsc"
