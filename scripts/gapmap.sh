#!/usr/bin/env bash
# Gap map: run the promise-revert sampler over every package with tests and
# write gapmap-report.md. Never fails on findings — it is a map, not a gate.
# Usage: scripts/gapmap.sh [--max N] [pkg...]   (default: every package with tests)
set -euo pipefail
cd "$(dirname "$0")/.."
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
if [[ -n "$(git status --short)" ]]; then
  echo "gapmap: working tree is not clean after sampling — a neutralized guard left a side effect:" >&2
  git status --short >&2
  git checkout -- . && git clean -fdq
fi
