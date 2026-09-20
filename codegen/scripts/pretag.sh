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
if git ls-remote --exit-code --tags origin "refs/tags/$version" >/dev/null 2>&1; then
  fail "$version already exists on origin"
fi

# 3. A releasable module has no replace directives and a clean tree.
if grep -Eq '^[[:space:]]*replace([[:space:]]|\()' go.mod; then
  grep -nE '^[[:space:]]*replace([[:space:]]|\()' go.mod >&2
  fail "go.mod contains replace directives"
fi
if [[ -n "$(git status --porcelain)" ]]; then
  git status --short >&2
  fail "working tree is not clean"
fi

# 4. The framework release manifest must name the version being released.
#    The release workflow verifies it against the tag it was triggered by
#    (`framework verify --expected-codegen "$RELEASE_TAG"`), so a manifest
#    that drifts turns the protected framework gate red AFTER the tag is
#    pushed — silently, because the tag itself is perfectly usable and the
#    only thing missing is the lock the gate produces. v1.15.20 … v1.15.29
#    were all released with this field stuck at v1.15.19 (U-0270).
manifest="ci/framework-release.yaml"
if [[ -f "$manifest" ]]; then
  declared=$(awk '/^codegen:/{print $2; exit}' "$manifest")
  [[ -n "$declared" ]] || fail "$manifest has no codegen: field"
  if [[ "$declared" != "$version" ]]; then
    fail "$manifest says codegen: $declared but this release is $version; the release workflow compares the two and fails the framework gate once the tag is pushed"
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
GOWORK=off go test ./... >/dev/null

echo "pretag: $module@$version is ready to tag"
