#!/usr/bin/env bash
# NC-205：origin 不可达时，pretag 的“远端没有同名 tag”检查必须失败，而不是当作“不存在”放行。
set -uo pipefail
src="$1"
work="$(mktemp -d "${TMPDIR:-/tmp}/n15-pretag.XXXXXX")"
trap 'rm -rf "$work"' EXIT
cd "$work"
git init -q . && git config user.email t@example.com && git config user.name t
mkdir -p scripts codegen/ci
cp "$src/scripts/pretag.sh" scripts/
printf 'module example.com/pretagred\n\ngo 1.27.0\n' > go.mod
printf 'package pretagred\n\nfunc OK() bool { return true }\n' > a.go
printf 'release: v1.99.0\n' > codegen/ci/framework-release.yaml
git add -A && git commit -qm init
git remote add origin "$work/no-such-remote.git"
GOWORK=off GOFLAGS= bash scripts/pretag.sh v1.99.0; echo "pretag exit=$?"
