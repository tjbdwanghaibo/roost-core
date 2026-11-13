#!/usr/bin/env bash
# Validate a release before the tag exists.
# Why this is a script and not only a CI job: a workflow triggered by a tag
# push runs *after* the tag is on the remote and visible to the module proxy.
# It can report that the tag is unusable; it cannot prevent it. That is how
# roost-core ended up with a pushed v2.0.0 on a module path with no /v2
#   go: ...@v2.0.0: invalid version: module contains a go.mod file,
#   so module path must match major version (".../roost-core/v2")
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

#    exactly the dependency mistakes a consumer would hit.
echo "pretag: building with GOWORK=off"
GOWORK=off go build ./... >/dev/null
echo "pretag: vetting with GOWORK=off"
GOWORK=off go vet ./... >/dev/null
echo "pretag: checking go.mod / go.sum are tidy"
GOWORK=off go mod tidy
git diff --quiet -- go.mod go.sum || fail "go.mod / go.sum are not tidy; commit the result of go mod tidy first (the CI unit job diffs them)"
echo "pretag: testing with GOWORK=off"
# -timeout 60m: go test's default is 10 minutes per test binary. codegen/internal/roost
# generates, syncs and compiles whole game-demo projects and spends almost all of that time in
# file-system calls, so it tracks the host's I/O load more than its own work: on this 10-core
# macOS host it took 377-784s alone on 2026-11-04 (1-minute load 10-19), and full runs panicked
# with "test timed out after 10m0s" with no failing test. After that day's t.Parallel / fixture
# round it took 299-444s under load 18-25 (an intermediate state of that round reached ~815s
# while other jobs saturated the disk), so the cap sits well above the slowest package instead
# of at the default; a real hang still fails, just later.
# 2026-11-13: with 42 business modules the package takes 2024-2151s in full repo runs on this
# host (each module scenario generates and compiles a project), so the v1.45.0 pretag panicked at
# 30m with no failing test. The cap is now 60m, again well above the slowest observed run.
test_log="$(mktemp)"
if ! GOWORK=off go test -timeout 60m ./... >"$test_log" 2>&1; then
  grep -E '^(--- FAIL|FAIL|panic:)' "$test_log" >&2 || tail -50 "$test_log" >&2
  fail "go test ./... failed (full output: $test_log)"
fi
rm -f "$test_log"

# Generate real consumers and run offline genre and generator checks.
# Clear REAL switches to avoid touching shared services during release checks.
echo "pretag: genre vertical slices and offline generator scripts (generated projects, offline default mode)"
genre_scripts=(
  scripts/test-slg-generated.sh
  scripts/test-mmorpg-generated.sh
  scripts/test-mmorpg-scenes.sh
  scripts/test-moba-generated.sh
  scripts/test-card-rpg-generated.sh
  scripts/test-tower-defense-generated.sh
  scripts/test-survival-extraction-generated.sh
  scripts/test-merge-casual-generated.sh
  scripts/test-idle-sim-generated.sh
  scripts/test-auto-chess-generated.sh
  scripts/test-heartbeat-generated.sh
  scripts/test-level-generated.sh
  scripts/test-sync-modes-generated.sh
  scripts/test-party-room-generated.sh
  scripts/test-turn-pvp-generated.sh
  scripts/test-coop-roguelike-generated.sh
  scripts/test-social-sim-generated.sh
  scripts/test-modules-generated.sh
)
genre_logs="$(mktemp -d "${TMPDIR:-/tmp}/roost-pretag-genres.XXXXXX")"
genre_start=$SECONDS
# The child Bash, not this shell, expands the positional parameters in its command.
# shellcheck disable=SC2016
if ! printf '%s\n' "${genre_scripts[@]}" | env -u ROOST_SLG_REAL -u ROOST_MMORPG_REAL -u ROOST_MOBA_REAL -u ROOST_MOBA_ROUTING \
    -u ROOST_CARD_RPG_REAL -u ROOST_TOWER_DEFENSE_REAL -u ROOST_SURVIVAL_EXTRACTION_REAL -u ROOST_MERGE_CASUAL_REAL \
    -u ROOST_IDLE_SIM_REAL -u ROOST_AUTO_CHESS_REAL -u ROOST_PARTY_ROOM_REAL \
    xargs -P "${ROOST_PRETAG_GENRE_JOBS:-4}" -I{} bash -c '
      name="$(basename "$1" .sh)"; start=$SECONDS
      if "./$1" >"$2/$name.log" 2>&1; then
        echo "pretag:   ok   $name ($((SECONDS - start))s)"
      else
        echo "pretag:   FAIL $name ($((SECONDS - start))s, log: $2/$name.log)" >&2
        exit 1
      fi' _ {} "$genre_logs"; then
  fail "a genre vertical slice failed (logs: $genre_logs)"
fi
echo "pretag: genre vertical slices passed in $((SECONDS - genre_start))s"
rm -rf "$genre_logs"
[[ -z "$(git status --porcelain)" ]] || { git status --short >&2; fail "a genre script left files in the checkout"; }

echo "pretag: C# client SDK golden and self tests"
ROOST_DOTNET_GOLDEN_ONLY=1 ./scripts/test-client-dotnet.sh >/dev/null || fail "C# client SDK tests failed (rerun ./scripts/test-client-dotnet.sh to see the output)"

echo "pretag: $module@$version is ready to tag"
