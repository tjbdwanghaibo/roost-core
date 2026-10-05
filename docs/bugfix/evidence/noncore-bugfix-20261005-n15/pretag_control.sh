#!/usr/bin/env bash
# NC-205 控制：origin 可达且没有该 tag → 照常 ready；origin 已有该 tag → 拒绝。
set -uo pipefail
src="$1"
work="$(mktemp -d "${TMPDIR:-/tmp}/n15-pretag.XXXXXX")"
trap 'rm -rf "$work"' EXIT
git init -q --bare "$work/origin.git"
cd "$work" && mkdir repo && cd repo
git init -q . && git config user.email t@example.com && git config user.name t
mkdir -p scripts codegen/ci && cp "$src/scripts/pretag.sh" scripts/
printf 'module example.com/pretagctl\n\ngo 1.27.0\n' > go.mod
printf 'package pretagctl\n\nfunc OK() bool { return true }\n' > a.go
printf 'release: v1.99.0\n' > codegen/ci/framework-release.yaml
git add -A && git commit -qm init && git remote add origin "$work/origin.git" && git push -q origin HEAD:refs/heads/main
GOWORK=off bash scripts/pretag.sh v1.99.0 2>&1 | tail -1; echo "reachable, no tag: exit=${PIPESTATUS[0]}"
git tag v1.99.0 && git push -q origin v1.99.0 && git tag -d v1.99.0 >/dev/null
GOWORK=off bash scripts/pretag.sh v1.99.0 2>&1 | tail -1; echo "tag on origin: exit=${PIPESTATUS[0]}"
