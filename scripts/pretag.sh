#!/usr/bin/env bash
# Validate a release before the tag exists.
#
# Why this is a script and not only a CI job: a workflow triggered by a tag
# push runs *after* the tag is on the remote and visible to the module proxy.
# It can report that the tag is unusable; it cannot prevent it. That is how
# roost-core ended up with a pushed v2.0.0 on a module path with no /v2
# suffix — a tag no consumer can ever select:
#
#   go: ...@v2.0.0: invalid version: module contains a go.mod file,
#   so module path must match major version (".../roost-core/v2")
#
# Run this from the repository root before creating the tag:
#   ./scripts/pretag.sh v1.11.0
set -euo pipefail

version="${1:-}"
if [[ -z "$version" ]]; then
  echo "usage: $0 <version>   e.g. $0 v1.11.0" >&2
  exit 2
fi

fail() { echo "pretag: $*" >&2; exit 1; }

[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] \
  || fail "$version is not a semantic version tag"

module=$(awk '/^module /{print $2; exit}' go.mod)
[[ -n "$module" ]] || fail "could not read the module path from go.mod"

# 1. The tag's major version must match the module path suffix, or the tag is
#    unresolvable. Go requires /vN in the path for N >= 2.
major="${version#v}"; major="${major%%.*}"
suffix=$(printf '%s' "$module" | grep -oE '/v[0-9]+$' | tr -d '/v' || true)
want=""; (( major >= 2 )) && want="$major"
if [[ "${suffix:-}" != "$want" ]]; then
  if [[ -n "$want" ]]; then
    fail "$version needs module path '$module/v$want'; publishing v$major on '$module' produces a tag nothing can select"
  fi
  fail "$version is a v1 tag but the module path carries the '/v$suffix' suffix"
fi

# 2. The tag must not already exist, locally or on the remote. Re-tagging a
#    published version is worse than a bad tag: the proxy has already cached
#    the old content under that name.
if git rev-parse -q --verify "refs/tags/$version" >/dev/null; then
  fail "$version already exists locally"
fi
# --exit-code 返回 2 才表示“远端没有这个 ref”；网络 / 认证 / 远端错误是 128 等其他值。
# RR-20261005-NC-205：之前把任何非 0 都当成“不存在”，origin 不可达时这条检查被静默跳过，
# pretag 照样报告 ready to tag。
remote_code=0
remote_err="$(git ls-remote --exit-code --tags origin "refs/tags/$version" 2>&1 >/dev/null)" || remote_code=$?
case "$remote_code" in
  0) fail "$version already exists on origin" ;;
  2) ;;
  *) printf '%s\n' "$remote_err" >&2
     fail "cannot check origin for an existing $version tag (git ls-remote exit $remote_code); fix the remote or the network and rerun" ;;
esac

# 3. A releasable module has no replace directives and a clean tree.
if grep -Eq '^[[:space:]]*replace([[:space:]]|\()' go.mod; then
  grep -nE '^[[:space:]]*replace([[:space:]]|\()' go.mod >&2
  fail "go.mod contains replace directives"
fi
if [[ -n "$(git status --porcelain)" ]]; then
  git status --short >&2
  fail "working tree is not clean"
fi

# 3b. The committed generated code must be what the generators produce now
#     (F12 G7). A hand-edited or stale *_gen.go builds, vets and tests green;
#     only regenerating shows it. ci.yml runs the same check, but CI runs after
#     the tag is pushed (see the top of this file) and nobody waits for it.
#     The changed files are left in place so the diff can be read.
echo "pretag: checking go generate ./... leaves the tree unchanged"
GOWORK=off go generate ./... >/dev/null
if [[ -n "$(git status --porcelain)" ]]; then
  git status --short >&2
  fail "go generate ./... changed the tree; commit the regenerated files (git diff shows what drifted)"
fi

# 4. The framework release manifest must name the version being released.
#    The release workflow verifies it against the tag it was triggered by, so a
#    manifest that drifts turns the protected gate red AFTER the tag is pushed —
#    silently, because the tag itself is perfectly usable and the only thing
#    missing is the lock the gate produces. It drifted for ten releases in
#    roost-codegen before this check existed (U-0270); the check moved here with
#    the release when the three repositories became one.
manifest="codegen/ci/framework-release.yaml"
if [[ -f "$manifest" ]]; then
  declared=$(awk '/^release:/{print $2; exit}' "$manifest")
  [[ -n "$declared" ]] || fail "$manifest has no release: field"
  if [[ "$declared" != "$version" ]]; then
    fail "$manifest says release: $declared but this release is $version; the release workflow compares the two and fails the framework gate once the tag is pushed"
  fi
  echo "pretag: framework release manifest names $declared"
fi

# 5. It must build and test with the workspace off — the workspace hides
#    exactly the dependency mistakes a consumer would hit.
echo "pretag: building with GOWORK=off"
GOWORK=off go build ./... >/dev/null
echo "pretag: vetting with GOWORK=off"
GOWORK=off go vet ./... >/dev/null
echo "pretag: checking go.mod / go.sum are tidy"
GOWORK=off go mod tidy
git diff --quiet -- go.mod go.sum || fail "go.mod / go.sum are not tidy; commit the result of go mod tidy first (the CI unit job diffs them)"
echo "pretag: testing with GOWORK=off"
# 成功时不刷屏；失败时把失败的包与用例打出来——此前整段输出都丢进 /dev/null，
# 一次偶发失败之后无从判断是哪条（v1.20.0 发版时遇到）。
test_log="$(mktemp)"
if ! GOWORK=off go test ./... >"$test_log" 2>&1; then
  grep -E '^(--- FAIL|FAIL|panic:)' "$test_log" >&2 || tail -50 "$test_log" >&2
  fail "go test ./... failed (full output: $test_log)"
fi
rm -f "$test_log"

echo "pretag: $module@$version is ready to tag"
