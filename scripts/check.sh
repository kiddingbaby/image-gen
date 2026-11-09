#!/usr/bin/env bash
# 本仓唯一的校验入口：本地、Gitea、GitHub 都跑这一条。
#
# 用法：scripts/check.sh [--fix]
#   --fix  只做 gofmt -w（给 make fmt 用），不跑后续检查。
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  printf '错误：需要在 git 工作区里运行（gofmt 依赖 git ls-files）。\n' >&2
  exit 1
fi

fix=0
if [ "${1:-}" = "--fix" ]; then
  fix=1
  shift
fi
if [ "$#" -gt 0 ]; then
  printf '未知参数：%s\n' "$*" >&2
  exit 2
fi

step() {
  printf '\n== %s\n' "$1"
}

mapfile -t files < <(git ls-files '*.go')

if [ "$fix" = 1 ]; then
  gofmt -w "${files[@]}"
  printf '已格式化 %d 个文件。\n' "${#files[@]}"
  exit 0
fi

step "gofmt"
if [ "${#files[@]}" -gt 0 ]; then
  out="$(gofmt -l "${files[@]}")"
  if [ -n "$out" ]; then
    printf '以下文件未格式化，请运行 make fmt 或 gofmt -w：\n%s\n' "$out" >&2
    exit 1
  fi
fi

step "go vet"
go vet ./...

step "go test"
go test ./...

step "go build"
go build -o /dev/null .