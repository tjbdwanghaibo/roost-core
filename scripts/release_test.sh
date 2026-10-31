#!/usr/bin/env bash
# scripts/release-lib.sh 的自检：版本号校验与比较、CHANGELOG 归档、各版本号文件的改写。
# 只写临时目录，不碰 git 与网络；根包 TestReleaseScriptSelfTest 在每次 go test 里跑它。
#   bash scripts/release_test.sh
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=scripts/release-lib.sh disable=SC1091
. "$repo_root/scripts/release-lib.sh"

scratch="$(mktemp -d "${TMPDIR:-/tmp}/roost-release-test.XXXXXX")"
trap 'rm -rf -- "$scratch"' EXIT

failures=0
fail() {
	echo "FAIL: $*" >&2
	failures=$((failures + 1))
}
# changed_lines A B：两个文件不同的行数（只数 diff 的 +/- 行）。
changed_lines() {
	diff "$1" "$2" | grep -c '^[<>]' || true
}

# ---- 版本号 ----
for v in v1.33.0 v0.0.1 v1.10.0 v10.0.12; do
	rel_valid_version "$v" || fail "rel_valid_version 拒绝了合法版本 $v"
done
for v in 1.33.0 v1.33 v1.33.0-rc1 v01.2.3 v1.2.03 v1.2.3x "" "v1.2.3 "; do
	if rel_valid_version "$v"; then fail "rel_valid_version 接受了 '$v'"; fi
done

expect_gt() { rel_version_gt "$1" "$2" || fail "期望 $1 > $2"; }
expect_not_gt() { if rel_version_gt "$1" "$2"; then fail "期望 $1 不大于 $2"; fi; }
expect_gt v1.33.0 v1.32.0
expect_gt v1.32.1 v1.32.0
expect_gt v1.10.0 v1.9.9 # 按数值比，不按字典序
expect_gt v2.0.0 v1.99.99
expect_not_gt v1.32.0 v1.32.0
expect_not_gt v1.31.9 v1.32.0
expect_not_gt v1.9.10 v1.10.0

rel_is_feature v1.33.0 || fail "v1.33.0 应是功能版本"
if rel_is_feature v1.32.1; then fail "v1.32.1 不是功能版本"; fi
if rel_is_feature v1.32.10; then fail "v1.32.10 不是功能版本"; fi

# ---- CHANGELOG 归档 ----
cat >"$scratch/cl.md" <<'EOF'
# Changelog

前言。

## [Unreleased]

### 新增

- **一条新功能**：说明。

## [v1.32.0] - 2026-10-30

旧内容。
EOF
cat >"$scratch/cl.want" <<'EOF'
# Changelog

前言。

## [Unreleased]

## [v1.33.0] - 2026-11-02

### 新增

- **一条新功能**：说明。

## [v1.32.0] - 2026-10-30

旧内容。
EOF
rel_changelog_has_unreleased "$scratch/cl.md" || fail "有内容的 [Unreleased] 被判为空"
if rel_render_changelog "$scratch/cl.md" v1.33.0 2026-11-02 >"$scratch/cl.got"; then
	cmp -s "$scratch/cl.want" "$scratch/cl.got" || { fail "CHANGELOG 归档结果不符"; diff -u "$scratch/cl.want" "$scratch/cl.got" >&2 || true; }
	# 归档后的 [Unreleased] 必须是空的，再归档一次要被拒绝
	if rel_changelog_has_unreleased "$scratch/cl.got"; then fail "归档后 [Unreleased] 不为空"; fi
else
	fail "rel_render_changelog 拒绝了正常的 CHANGELOG"
fi

# Unreleased 标题下没有空行、有多个空行，都归一成标题之间一个空行
printf '## [Unreleased]\n- a\n' >"$scratch/tight.md"
printf '## [Unreleased]\n\n## [v1.0.1] - D\n\n- a\n' >"$scratch/tight.want"
rel_render_changelog "$scratch/tight.md" v1.0.1 D >"$scratch/tight.got" || fail "紧凑写法被拒绝"
cmp -s "$scratch/tight.want" "$scratch/tight.got" || fail "紧凑写法归档结果不符：$(cat "$scratch/tight.got")"
printf '## [Unreleased]\n\n\n\n- a\n' >"$scratch/loose.md"
rel_render_changelog "$scratch/loose.md" v1.0.1 D >"$scratch/loose.got" || fail "多空行写法被拒绝"
cmp -s "$scratch/tight.want" "$scratch/loose.got" || fail "多空行写法归档结果不符：$(cat "$scratch/loose.got")"

# 空的 [Unreleased]（只有空白行）、已有同版本段、没有 [Unreleased] 段：都拒绝
printf '# C\n\n## [Unreleased]\n\n   \n\n## [v1.32.0] - 2026-10-30\n\n- x\n' >"$scratch/empty.md"
if rel_changelog_has_unreleased "$scratch/empty.md"; then fail "只有空白的 [Unreleased] 被判为非空"; fi
if rel_render_changelog "$scratch/empty.md" v1.33.0 D >/dev/null 2>&1; then fail "空 [Unreleased] 也归档了"; fi
if rel_render_changelog "$scratch/cl.md" v1.32.0 D >/dev/null 2>&1; then fail "已有 [v1.32.0] 段仍归档"; fi
printf '# C\n\n## [v1.32.0] - D\n\n- x\n' >"$scratch/none.md"
if rel_render_changelog "$scratch/none.md" v1.33.0 D >/dev/null 2>&1; then fail "没有 [Unreleased] 段也归档了"; fi

# 仓库真实 CHANGELOG：只多出新版本标题与一个空行，其余逐字不变。
# （发版刚结束、[Unreleased] 为空时跳过这一条。）
if rel_changelog_has_unreleased "$repo_root/CHANGELOG.md"; then
	rel_render_changelog "$repo_root/CHANGELOG.md" v99.0.0 2099-01-01 >"$scratch/real-cl.md" || fail "真实 CHANGELOG 归档失败"
	grep -v -x -e '## \[v99.0.0\] - 2099-01-01' "$scratch/real-cl.md" >"$scratch/real-cl.stripped" || true
	if [[ "$(grep -c -x '## \[v99.0.0\] - 2099-01-01' "$scratch/real-cl.md")" != 1 ]]; then fail "真实 CHANGELOG 没有恰好一个新标题"; fi
	# 去掉新标题后与原文只差一行空行
	[[ "$(changed_lines "$repo_root/CHANGELOG.md" "$scratch/real-cl.stripped")" -le 1 ]] || fail "真实 CHANGELOG 归档改动了既有内容"
fi

# ---- framework-release.yaml ----
rel_render_manifest_release "$repo_root/codegen/ci/framework-release.yaml" v99.0.0 >"$scratch/fr.yaml" || fail "改写 framework-release.yaml 失败"
[[ "$(rel_manifest_release "$scratch/fr.yaml")" == v99.0.0 ]] || fail "framework-release.yaml 改写后 release: 不是 v99.0.0"
[[ "$(changed_lines "$repo_root/codegen/ci/framework-release.yaml" "$scratch/fr.yaml")" == 2 ]] || fail "framework-release.yaml 改动不止 release: 一行"
printf 'release: v1\nrelease: v2\n' >"$scratch/two.yaml"
if rel_render_manifest_release "$scratch/two.yaml" v3 >/dev/null 2>&1; then fail "两行 release: 没有被拒绝"; fi

# ---- 生成物最低 core ----
gen="$repo_root/codegen/internal/roost/manifest.go"
old_min="$(rel_min_core "$gen")"
rel_valid_version "$old_min" || fail "读不出真实 manifest.go 的 minimumVersions.Core（得到 '$old_min'）"
[[ "$old_min" == "$(sh "$repo_root/codegen/scripts/core-pin.sh")" ]] || fail "rel_min_core 与 codegen/scripts/core-pin.sh 读出的不同"
rel_render_min_core "$gen" v99.0.0 >"$scratch/manifest.go" || fail "改写 manifest.go 失败"
[[ "$(rel_min_core "$scratch/manifest.go")" == v99.0.0 ]] || fail "manifest.go 改写后 Core 不是 v99.0.0"
[[ "$(changed_lines "$gen" "$scratch/manifest.go")" == 2 ]] || fail "manifest.go 改动不止 Core 一行"

compat="$repo_root/.github/workflows/framework-compat.yml"
rel_render_compat_minimum "$compat" "$old_min" v99.0.0 >"$scratch/compat.yml" || fail "改写 framework-compat.yml 失败"
grep -q -- '-roost-core-version v99.0.0)' "$scratch/compat.yml" || fail "framework-compat.yml 的 -roost-core-version 没改"
grep -qF "grep -v 'cmd/roost@v99.0.0 '" "$scratch/compat.yml" || fail "framework-compat.yml 的 Makefile 生成器检查没改"
if grep -qF "$old_min" "$scratch/compat.yml"; then fail "framework-compat.yml 仍残留 $old_min"; fi
if rel_render_compat_minimum "$compat" v0.0.1 v99.0.0 >/dev/null 2>&1; then fail "旧下限与工作流不一致时没有拒绝"; fi

# ---- upgrade-compat 矩阵 ----
upgrade="$repo_root/.github/workflows/upgrade-compat.yml"
rel_render_upgrade_compat "$upgrade" v99.0.0 >"$scratch/upgrade.yml" || fail "改写 upgrade-compat.yml 失败"
grep -qE '^[[:space:]]+core: \[[^]]*, v99\.0\.0\]$' "$scratch/upgrade.yml" || fail "upgrade-compat 矩阵末尾没有追加 v99.0.0：$(grep 'core: \[' "$scratch/upgrade.yml")"
[[ "$(changed_lines "$upgrade" "$scratch/upgrade.yml")" == 2 ]] || fail "upgrade-compat.yml 改动不止矩阵一行"
rel_render_upgrade_compat "$scratch/upgrade.yml" v99.0.0 >"$scratch/upgrade2.yml" || fail "第二次追加失败"
cmp -s "$scratch/upgrade.yml" "$scratch/upgrade2.yml" || fail "已在矩阵里的版本又追加了一次"
printf 'jobs:\n  previous-project:\n    matrix:\n        core: []\n  other:\n        core: [v0.1.0]\n' >"$scratch/empty-matrix.yml"
rel_render_upgrade_compat "$scratch/empty-matrix.yml" v1.0.0 >"$scratch/empty-matrix.got" || fail "空矩阵追加失败"
grep -qx '        core: \[v1.0.0\]' "$scratch/empty-matrix.got" || fail "空矩阵追加结果不对"
grep -qx '        core: \[v0.1.0\]' "$scratch/empty-matrix.got" || fail "改到了 previous-project 以外任务的矩阵"
# 真实文件里 player-access-project 的固定矩阵不动
grep -qx '        core: \[v1.26.1\]' "$scratch/upgrade.yml" || fail "改到了 player-access-project 的矩阵"

# 执行真实发布脚本的 pretag 提示：macOS Bash 3.2 在 UTF-8 locale 下
# 会把紧挨未加花括号变量的中文标点误作变量名；不能只检查 bash -n。
pretag_message="$(sed -n '/^say "scripts\/pretag.sh /p' "$repo_root/scripts/release.sh")"
[[ -n "$pretag_message" ]] || fail "缺少 pretag 提示"
for message_locale in C en_US.UTF-8; do
 if ! LC_ALL="$message_locale" "$BASH" -uc 'version=v1.2.3; say() { printf "%s\n" "$1"; }; eval "$1"' _ "$pretag_message" >"$scratch/message.log" 2>&1; then
  cat "$scratch/message.log" >&2
  fail "pretag 提示在 $message_locale 下展开失败"
 elif ! grep -q '^scripts/pretag.sh v1.2.3' "$scratch/message.log"; then
  fail "pretag 提示缺少目标版本"
 fi
done

if ((failures)); then
	echo "release_test: $failures 项失败" >&2
	exit 1
fi
echo "release_test: ok"
