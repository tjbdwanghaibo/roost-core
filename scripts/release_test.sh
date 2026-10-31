#!/usr/bin/env bash
# Release transformations and diagnostics regression checks; no network or Git mutations.
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
changed_lines() {
	diff "$1" "$2" | grep -c '^[<>]' || true
}

for v in v1.33.0 v0.0.1 v1.10.0 v10.0.12; do
	rel_valid_version "$v" || fail "rel_valid_version rejected valid version $v"
done
for v in 1.33.0 v1.33 v1.33.0-rc1 v01.2.3 v1.2.03 v1.2.3x "" "v1.2.3 "; do
	if rel_valid_version "$v"; then fail "rel_valid_version accepted '$v'"; fi
done

expect_gt() { rel_version_gt "$1" "$2" || fail "expected $1 > $2"; }
expect_not_gt() { if rel_version_gt "$1" "$2"; then fail "expected $1 <= $2"; fi; }
expect_gt v1.33.0 v1.32.0
expect_gt v1.32.1 v1.32.0
expect_gt v1.10.0 v1.9.9 # Compare numerically, not lexicographically.
expect_gt v2.0.0 v1.99.99
expect_not_gt v1.32.0 v1.32.0
expect_not_gt v1.31.9 v1.32.0
expect_not_gt v1.9.10 v1.10.0

rel_is_feature v1.33.0 || fail "v1.33.0 must be a feature release"
if rel_is_feature v1.32.1; then fail "v1.32.1 must not be a feature release"; fi
if rel_is_feature v1.32.10; then fail "v1.32.10 must not be a feature release"; fi

cat >"$scratch/cl.md" <<'EOF'
# Changelog

Introduction.

## [Unreleased]

### Added

- **A new feature**: description.

## [v1.32.0] - 2026-10-30

Existing content.
EOF
cat >"$scratch/cl.want" <<'EOF'
# Changelog

Introduction.

## [Unreleased]

## [v1.33.0] - 2026-11-02

### Added

- **A new feature**: description.

## [v1.32.0] - 2026-10-30

Existing content.
EOF
rel_changelog_has_unreleased "$scratch/cl.md" || fail "nonempty [Unreleased] was considered empty"
if rel_render_changelog "$scratch/cl.md" v1.33.0 2026-11-02 >"$scratch/cl.got"; then
	cmp -s "$scratch/cl.want" "$scratch/cl.got" || { fail "unexpected archived CHANGELOG"; diff -u "$scratch/cl.want" "$scratch/cl.got" >&2 || true; }
	if rel_changelog_has_unreleased "$scratch/cl.got"; then fail "[Unreleased] is not empty after archiving"; fi
else
	fail "rel_render_changelog rejected a valid CHANGELOG"
fi

printf '## [Unreleased]\n- a\n' >"$scratch/tight.md"
printf '## [Unreleased]\n\n## [v1.0.1] - D\n\n- a\n' >"$scratch/tight.want"
rel_render_changelog "$scratch/tight.md" v1.0.1 D >"$scratch/tight.got" || fail "compact layout rejected"
cmp -s "$scratch/tight.want" "$scratch/tight.got" || fail "unexpected compact archive: $(cat "$scratch/tight.got")"
printf '## [Unreleased]\n\n\n\n- a\n' >"$scratch/loose.md"
rel_render_changelog "$scratch/loose.md" v1.0.1 D >"$scratch/loose.got" || fail "blank-line layout rejected"
cmp -s "$scratch/tight.want" "$scratch/loose.got" || fail "unexpected blank-line archive: $(cat "$scratch/loose.got")"

printf '# C\n\n## [Unreleased]\n\n   \n\n## [v1.32.0] - 2026-10-30\n\n- x\n' >"$scratch/empty.md"
if rel_changelog_has_unreleased "$scratch/empty.md"; then fail "blank [Unreleased] was considered nonempty"; fi
if rel_render_changelog "$scratch/empty.md" v1.33.0 D >/dev/null 2>&1; then fail "empty [Unreleased] was archived"; fi
if rel_render_changelog "$scratch/cl.md" v1.32.0 D >/dev/null 2>&1; then fail "existing [v1.32.0] was archived again"; fi
printf '# C\n\n## [v1.32.0] - D\n\n- x\n' >"$scratch/none.md"
if rel_render_changelog "$scratch/none.md" v1.33.0 D >/dev/null 2>&1; then fail "missing [Unreleased] was accepted"; fi

if rel_changelog_has_unreleased "$repo_root/CHANGELOG.md"; then
	rel_render_changelog "$repo_root/CHANGELOG.md" v99.0.0 2099-01-01 >"$scratch/real-cl.md" || fail "real CHANGELOG archive failed"
	grep -v -x -e '## \[v99.0.0\] - 2099-01-01' "$scratch/real-cl.md" >"$scratch/real-cl.stripped" || true
	if [[ "$(grep -c -x '## \[v99.0.0\] - 2099-01-01' "$scratch/real-cl.md")" != 1 ]]; then fail "real CHANGELOG must gain exactly one heading"; fi
	[[ "$(changed_lines "$repo_root/CHANGELOG.md" "$scratch/real-cl.stripped")" -le 1 ]] || fail "archiving changed existing CHANGELOG content"
fi

# ---- framework-release.yaml ----
rel_render_manifest_release "$repo_root/codegen/ci/framework-release.yaml" v99.0.0 >"$scratch/fr.yaml" || fail "failed to render framework-release.yaml"
[[ "$(rel_manifest_release "$scratch/fr.yaml")" == v99.0.0 ]] || fail "rendered release is not v99.0.0"
[[ "$(changed_lines "$repo_root/codegen/ci/framework-release.yaml" "$scratch/fr.yaml")" == 2 ]] || fail "unexpected framework-release.yaml changes"
printf 'release: v1\nrelease: v2\n' >"$scratch/two.yaml"
if rel_render_manifest_release "$scratch/two.yaml" v3 >/dev/null 2>&1; then fail "duplicate release: entries accepted"; fi

gen="$repo_root/codegen/internal/roost/manifest.go"
old_min="$(rel_min_core "$gen")"
rel_valid_version "$old_min" || fail "invalid minimumVersions.Core in manifest.go (got '$old_min')"
[[ "$old_min" == "$(sh "$repo_root/codegen/scripts/core-pin.sh")" ]] || fail "rel_min_core differs from core-pin.sh"
rel_render_min_core "$gen" v99.0.0 >"$scratch/manifest.go" || fail "failed to render manifest.go"
[[ "$(rel_min_core "$scratch/manifest.go")" == v99.0.0 ]] || fail "rendered Core is not v99.0.0"
[[ "$(changed_lines "$gen" "$scratch/manifest.go")" == 2 ]] || fail "unexpected manifest.go changes"

compat="$repo_root/.github/workflows/framework-compat.yml"
rel_render_compat_minimum "$compat" "$old_min" v99.0.0 >"$scratch/compat.yml" || fail "failed to render framework-compat.yml"
grep -q -- '-roost-core-version v99.0.0)' "$scratch/compat.yml" || fail "core version argument was not updated"
grep -qF "grep -v 'cmd/roost@v99.0.0 '" "$scratch/compat.yml" || fail "Makefile generator check was not updated"
if grep -qF "$old_min" "$scratch/compat.yml"; then fail "framework-compat.yml still contains $old_min"; fi
if rel_render_compat_minimum "$compat" v0.0.1 v99.0.0 >/dev/null 2>&1; then fail "mismatched minimum lane accepted"; fi

upgrade="$repo_root/.github/workflows/upgrade-compat.yml"
rel_render_upgrade_compat "$upgrade" v99.0.0 >"$scratch/upgrade.yml" || fail "failed to render upgrade-compat.yml"
grep -qE '^[[:space:]]+core: \[[^]]*, v99\.0\.0\]$' "$scratch/upgrade.yml" || fail "v99.0.0 not appended to upgrade-compat matrix: $(grep 'core: \[' "$scratch/upgrade.yml")"
[[ "$(changed_lines "$upgrade" "$scratch/upgrade.yml")" == 2 ]] || fail "unexpected upgrade-compat.yml changes"
rel_render_upgrade_compat "$scratch/upgrade.yml" v99.0.0 >"$scratch/upgrade2.yml" || fail "second append failed"
cmp -s "$scratch/upgrade.yml" "$scratch/upgrade2.yml" || fail "version appended twice"
printf 'jobs:\n  previous-project:\n    matrix:\n        core: []\n  other:\n        core: [v0.1.0]\n' >"$scratch/empty-matrix.yml"
rel_render_upgrade_compat "$scratch/empty-matrix.yml" v1.0.0 >"$scratch/empty-matrix.got" || fail "empty matrix append failed"
grep -qx '        core: \[v1.0.0\]' "$scratch/empty-matrix.got" || fail "incorrect empty matrix append"
grep -qx '        core: \[v0.1.0\]' "$scratch/empty-matrix.got" || fail "unrelated job matrix changed"
grep -qx '        core: \[v1.26.1\]' "$scratch/upgrade.yml" || fail "player-access-project matrix changed"

pretag_message="$(sed -n '/^say "scripts\/pretag.sh /p' "$repo_root/scripts/release.sh")"
[[ -n "$pretag_message" ]] || fail "missing pretag message"
cat >"$scratch/message.sh" <<'MESSAGE'
version=v1.2.3
say() { printf '%s\n' "$1"; }
MESSAGE
printf '%s\n' "$pretag_message" >>"$scratch/message.sh"
for message_locale in C en_US.UTF-8; do
 if ! LC_ALL="$message_locale" "$BASH" -u "$scratch/message.sh" >"$scratch/message.log" 2>&1; then
  cat "$scratch/message.log" >&2
  fail "pretag message expansion failed under $message_locale"
 elif ! grep -q '^scripts/pretag.sh v1.2.3' "$scratch/message.log"; then
  fail "pretag message missing target version"
 fi
done

if ((failures)); then
	echo "release_test: $failures failures" >&2
	exit 1
fi
echo "release_test: ok"
