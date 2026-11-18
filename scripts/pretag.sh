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
# 测试范围（维护者 2026-11-18：改动不多时只跑有影响的）。上一个发布 tag 以来的改动交给 scripts/pretag-scope.py：
# 受影响的 Go 包（含反向依赖）、生成工程脚本（生成器目录或生成代码会 import 的包改了才跑）、C# / TS SDK（客户端相关改了才跑）。
# 改了 go.mod / go.sum / 发版脚本、改动超过上限、找不到上一个 tag、或设了 ROOST_PRETAG_FULL=1 时整跑。
# 生成工程脚本的完整清单（只写在这里，根包 TestGeneratedScriptsAreInPretag 读它）；本次跑哪些由下面的范围决定。
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
  scripts/test-gate-generated.sh
)
prev_tag="$(git describe --tags --abbrev=0 --match 'v[0-9]*' HEAD^ 2>/dev/null || true)"
if [[ -z "$prev_tag" ]]; then
  echo "pretag: no previous release tag found; full run"
  scope=full; go_packages=(./...); run_dotnet=1; run_ts=1
else
  scope_out="$(python3 scripts/pretag-scope.py "$prev_tag" "${genre_scripts[@]}")" || fail "scripts/pretag-scope.py failed"
  eval "$scope_out"
fi
echo "pretag: test scope = $scope (since ${prev_tag:-<none>}): ${#go_packages[@]} package pattern(s), ${#genre_scripts[@]} generated-project script(s), C# $run_dotnet, TS $run_ts"
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
# ROOST_REQUIRE_DOTNET=1：发版机器必须装 .NET 10 SDK。没有它时 TestClientConfigCSharpRuntime（导出的 C# 配置能编译、
# Load 读回、规则违反报行与字段）只会 SKIP，go test 仍然 ok，发版就少了这条覆盖却看不出来；设了它缺 dotnet 直接失败。
test_log="$(mktemp)"
if ! ROOST_REQUIRE_DOTNET=1 GOWORK=off go test -timeout 60m "${go_packages[@]}" >"$test_log" 2>&1; then
  grep -E '^(--- FAIL|FAIL|panic:)' "$test_log" >&2 || tail -50 "$test_log" >&2
  fail "go test failed (full output: $test_log)"
fi
rm -f "$test_log"

if (( ${#genre_scripts[@]} == 0 )); then
  echo "pretag: generated-project scripts skipped (nothing they depend on changed)"
else
  # Generate real consumers and run offline genre and generator checks.
  # Clear REAL switches to avoid touching shared services during release checks.
  echo "pretag: genre vertical slices and offline generator scripts (generated projects, offline default mode)"
  genre_logs="$(mktemp -d "${TMPDIR:-/tmp}/roost-pretag-genres.XXXXXX")"
  genre_start=$SECONDS
  # The child Bash, not this shell, expands the positional parameters in its command.
  # shellcheck disable=SC2016
  if ! printf '%s\n' "${genre_scripts[@]}" | env -u ROOST_SLG_REAL -u ROOST_MMORPG_REAL -u ROOST_MOBA_REAL -u ROOST_MOBA_ROUTING \
      -u ROOST_CARD_RPG_REAL -u ROOST_TOWER_DEFENSE_REAL -u ROOST_SURVIVAL_EXTRACTION_REAL -u ROOST_MERGE_CASUAL_REAL \
      -u ROOST_IDLE_SIM_REAL -u ROOST_AUTO_CHESS_REAL -u ROOST_PARTY_ROOM_REAL \
      -u ROOST_TURN_PVP_REAL -u ROOST_COOP_ROGUELIKE_REAL -u ROOST_SOCIAL_SIM_REAL \
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
fi

if [[ "$run_dotnet" == 1 ]]; then
  echo "pretag: C# client SDK golden and self tests"
  ROOST_REQUIRE_DOTNET=1 ROOST_DOTNET_GOLDEN_ONLY=1 ./scripts/test-client-dotnet.sh >/dev/null || fail "C# client SDK tests failed (rerun ./scripts/test-client-dotnet.sh to see the output)"
else
  echo "pretag: C# client SDK tests skipped (client SDK unchanged)"
fi
if [[ "$run_ts" == 1 ]]; then
  echo "pretag: TS client SDK tests"
  ./scripts/test-client-ts.sh >/dev/null 2>&1 || fail "TS client SDK tests failed (rerun ./scripts/test-client-ts.sh to see the output)"
else
  echo "pretag: TS client SDK tests skipped (client SDK unchanged)"
fi

echo "pretag: $module@$version is ready to tag"
