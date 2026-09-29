#!/usr/bin/env bash
# 只遍历源码包，避免生产备份目录的权限影响本地检查。
set -euo pipefail
cd "$(dirname "$0")/.."
args=()
if [[ "${1:-}" == "--race" ]]; then
  args+=(-race)
elif [[ $# -gt 0 ]]; then
  echo '用法：bash scripts/check.sh [--race]' >&2
  exit 2
fi
go test "${args[@]}" ./cmd/... ./internal/... ./tests/web/fixture/...
go vet ./cmd/... ./internal/... ./tests/web/fixture/...
node --test tests/*.test.mjs tests/web/*.test.cjs
git diff --check
