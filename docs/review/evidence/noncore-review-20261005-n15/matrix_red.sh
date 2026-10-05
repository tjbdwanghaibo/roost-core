#!/usr/bin/env bash
# NC-207：故障矩阵的一格若 -run 什么都没匹配到（go test 输出 "no tests to run"、退出 0），不能记 PASS。
# 用替身 go 与替身 bash 拦住 heal / 生成工程脚本，锁目录在临时根里，不碰任何隔离环境。
set -uo pipefail
src="$1"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/n15-matrix.XXXXXX")"; tmp="$(cd "$tmp" && pwd -P)"
label="n15-matrix-red-$$"
trap 'rm -rf "$tmp" "$src/artifacts/perf/remote/$label"' EXIT
mkdir -p "$tmp/bin" "$tmp/root"
real_bash="$(command -v bash)"
cat > "$tmp/bin/go" <<'G'
#!/bin/sh
case "$1" in
  version) echo "go version stub"; exit 0 ;;
  test) echo "testing: warning: no tests to run"; echo "PASS"; echo "ok  	stub/pkg	0.01s [no tests to run]"; exit 0 ;;
esac
exit 0
G
cat > "$tmp/bin/bash" <<G
#!$real_bash
case "\${1:-}" in
  kit/scripts/integration/dataengine-env.sh|scripts/test-remote-generated.sh) echo "stub \$*"; exit 0 ;;
esac
exec "$real_bash" "\$@"
G
chmod +x "$tmp/bin/go" "$tmp/bin/bash"
env -i HOME="$HOME" PATH="$tmp/bin:/usr/bin:/bin:/usr/sbin:/sbin" ROOST_DATAENGINE_IT_ROOT="$tmp/root" ROOST_REMOTE_MATRIX_LABEL="$label" \
  "$real_bash" "$src/scripts/test-remote-matrix.sh" >/dev/null 2>&1
echo "matrix exit=$?"
cat "$src/artifacts/perf/remote/$label/results.tsv"
