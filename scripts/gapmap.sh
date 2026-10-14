#!/usr/bin/env bash
# Gap map: run the promise-revert sampler over every package with tests and
# write gapmap-report.md. Never fails on findings — it is a map, not a gate.
# Usage: scripts/gapmap.sh [--max N] [--group NAME] [pkg...]   (default: every package with tests)
#
# --group 只取样某一组的包（组按目录前缀划分，见下方 gapmap_group_of）：全量串行约 120 分钟，
# 逼近单 job 180 分钟上限，nightly-gapmap 按组拆成 matrix 并行，再用 scripts/gapmap/merge.py
# 把各组报告合成一份 gapmap-report.md。给了 pkg 列表时 --group 只保留其中属于该组的包；
# 一个都不剩时照常写出一份 0 / 0 的报告，不退回“全部包”。
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
# 分组只看目录前缀；不属于前三组的包（gameplay、service、wiring、client、cmd 等）全部归 rest，
# 新增顶层目录自动落进 rest，不会漏取样。组名与 nightly-gapmap.yml 的 matrix 一一对应，
# 根包 TestGapmapWorkflowGroupsMatchScript 钉住两边一致。
GAPMAP_GROUPS="framework infra codegen rest"
gapmap_group_of() {
  case "$1" in
    framework/*) echo framework ;;
    infra/*) echo infra ;;
    codegen/*) echo codegen ;;
    *) echo rest ;;
  esac
}
max=20
group=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --max) max="$2"; shift 2 ;;
    --group) group="$2"; shift 2 ;;
    *) break ;;
  esac
done
if [[ -n "$group" && " $GAPMAP_GROUPS " != *" $group "* ]]; then
  echo "gapmap: unknown group '$group' (known: $GAPMAP_GROUPS)" >&2
  exit 2
fi
candidates=("$@")
if [[ ${#candidates[@]} -eq 0 ]]; then
  while IFS= read -r dir; do
    [[ -n "$(ls "$dir"/*_test.go 2>/dev/null)" ]] && candidates+=("$dir")
  done < <(go list -f '{{.Dir}}' ./... | sed "s#^$(pwd)/##" | grep -v '^\.$' | sort)
fi
pkgs=()
for dir in ${candidates[@]+"${candidates[@]}"}; do
  if [[ -z "$group" || "$(gapmap_group_of "${dir%/}/")" == "$group" ]]; then
    pkgs+=("$dir")
  fi
done
echo "gapmap: sampling ${#pkgs[@]} package(s)${group:+ in group $group}" >&2
{
  echo "# Gap map — $(git rev-parse --short HEAD) — $(date -u +%Y-%m-%dT%H:%MZ)"
  echo
  echo "Guards whose neutralization leaves the package tests green. Sampled up to ${max} per package."
  python3 scripts/gapmap/revertsample.py --max "$max" ${pkgs[@]+"${pkgs[@]}"}
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
