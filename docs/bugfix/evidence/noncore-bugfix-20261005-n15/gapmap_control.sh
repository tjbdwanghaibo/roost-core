#!/usr/bin/env bash
# NC-200 red/green: gapmap.sh 不能丢掉调用前已有的未提交跟踪修改。
set -uo pipefail
src="$1"   # worktree root providing scripts/gapmap.sh + scripts/gapmap/revertsample.py
work="$(mktemp -d "${TMPDIR:-/tmp}/n15-gapmap.XXXXXX")"
trap 'rm -rf "$work"' EXIT
cd "$work"
git init -q . && git config user.email t@example.com && git config user.name t
mkdir -p scripts/gapmap pkg
cp "$src/scripts/gapmap.sh" scripts/ && cp "$src/scripts/gapmap/revertsample.py" scripts/gapmap/
cat > go.mod <<'M'
module example.com/gm

go 1.27.0
M
cat > pkg/p.go <<'G'
package pkg

import "errors"

func Check(n int) error {
	if n < 0 {
		return errors.New("negative")
	}
	return nil
}
G
cat > pkg/p_test.go <<'G'
package pkg

import "testing"

func TestCheck(t *testing.T) { _ = Check(1) }
G
echo "notes v1" > NOTES.txt
git add -A && git commit -qm init
# 使用者手上尚未提交的工作（跟踪文件）
: clean control
GOWORK=off bash scripts/gapmap.sh --max 5 pkg > gapmap.out 2>&1
echo "gapmap exit=$?"
echo "NOTES.txt now: $(cat NOTES.txt)"
if [[ "$(cat NOTES.txt)" == "notes v2 (uncommitted work)" ]]; then echo "RESULT: uncommitted work kept"; else echo "RESULT: uncommitted work LOST"; fi
sed -n '1,40p' gapmap.out | grep -E 'gapmap:|refuse|M NOTES' || true
