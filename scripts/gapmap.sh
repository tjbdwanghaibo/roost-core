#!/usr/bin/env bash
# Gap map: run the promise-revert sampler over every package with tests and
# write gapmap-report.md. Never fails on findings — it is a map, not a gate.
# Usage: scripts/gapmap.sh [--max N] [pkg...]   (default: every package with tests)
set -euo pipefail
cd "$(dirname "$0")/.."
# RR-20261005-NC-200：收尾会用 `git checkout -- .` 恢复取样器留下的跟踪文件修改，它分不清
# “取样留下的”和“运行前就有的”。之前没有这道检查，本地带着未提交的修改运行会把它们一起丢掉
# （checkout 不进 stash / reflog，无法找回）。所以要求跟踪文件干净再开始；未跟踪文件不受影响。
if [[ -n "$(git status --short --untracked-files=no)" ]]; then
  echo "gapmap: tracked files have uncommitted changes; commit or stash them first (the sampler edits sources in place and restores tracked files with git checkout when it finishes):" >&2
  git status --short --untracked-files=no >&2
  exit 2
fi
max=20
if [[ "${1:-}" == "--max" ]]; then max="$2"; shift 2; fi
pkgs=("$@")
if [[ ${#pkgs[@]} -eq 0 ]]; then
  while IFS= read -r dir; do
    [[ -n "$(ls "$dir"/*_test.go 2>/dev/null)" ]] && pkgs+=("$dir")
  done < <(go list -f '{{.Dir}}' ./... | sed "s#^$(pwd)/##" | grep -v '^\.$' | sort)
fi
{
  echo "# Gap map — $(git rev-parse --short HEAD) — $(date -u +%Y-%m-%dT%H:%MZ)"
  echo
  echo "Guards whose neutralization leaves the package tests green. Sampled up to ${max} per package."
  python3 scripts/gapmap/revertsample.py --max "$max" "${pkgs[@]}"
} | tee gapmap-report.md
# Restore only TRACKED files the sampler may have left modified. Never `git
# clean`: untracked files belong to whoever is working in the tree (a test
# being written alongside the run was deleted this way once).
if [[ -n "$(git status --short --untracked-files=no)" ]]; then
  echo "gapmap: tracked files left modified after sampling — a neutralized guard had a side effect; restoring:" >&2
  git status --short --untracked-files=no >&2
  git checkout -- .
fi
if [[ -n "$(git status --short | grep '^??')" ]]; then
  echo "gapmap: note — untracked files present (left alone):" >&2
  git status --short | grep '^??' >&2
fi
