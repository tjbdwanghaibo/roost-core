#!/usr/bin/env bash
# Publish a new version: preflight, commit, pretag, push, tag, downstream verification.
# --min-core updates generated consumer requirements; document the API reason separately.
# --upgrade-compat / --no-upgrade-compat override previous-project matrix updates.
# --dry-run previews changes and reports all preflight failures without writing.
# Patch releases require CHANGELOG entries; feature releases also require release notes.
# Never print credentials or replace an existing tag.
#   scripts/release.sh vX.Y.Z [--min-core] [--upgrade-compat|--no-upgrade-compat] [--dry-run]
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=scripts/release-lib.sh disable=SC1091
. "$repo_root/scripts/release-lib.sh"

usage() {
	cat <<'USAGE' >&2
Usage: scripts/release.sh vMAJOR.MINOR.PATCH [--min-core] [--upgrade-compat|--no-upgrade-compat] [--dry-run]
USAGE
	exit 2
}

version=""
dry_run=0
min_core=0
upgrade_compat=auto
for arg in "$@"; do
	case "$arg" in
	--dry-run) dry_run=1 ;;
	--min-core) min_core=1 ;;
	--upgrade-compat) upgrade_compat=yes ;;
	--no-upgrade-compat) upgrade_compat=no ;;
	-h | --help) usage ;;
	v*) [[ -z "$version" ]] || usage; version="$arg" ;;
	*) echo "release: unknown argument $arg" >&2; usage ;;
	esac
done
[[ -n "$version" ]] || usage

cd "$repo_root"

manifest="codegen/ci/framework-release.yaml"
changelog="CHANGELOG.md"
gen_manifest="codegen/internal/roost/manifest.go"
compat_wf=".github/workflows/framework-compat.yml"
upgrade_wf=".github/workflows/upgrade-compat.yml"
module_path="github.com/tjbdwanghaibo/roost-core"
today="$(date +%Y-%m-%d)"

step=0
say() { step=$((step + 1)); printf '\n[release %d] %s\n' "$step" "$*"; }
info() { printf '  %s\n' "$*"; }
die() { printf 'release: %s\n' "$*" >&2; exit 1; }

# Dry-run reports every preflight failure but still renders the preview.
failures=0
check_failed() {
	if ((dry_run)); then
		printf '  Preflight failed: %s\n' "$*"
		failures=$((failures + 1))
	else
		die "$*"
	fi
}

say "Preflight checks for $version"
rel_valid_version "$version" || die "$version must be vMAJOR.MINOR.PATCH"
[[ -f "$manifest" && -f "$changelog" && -f "$gen_manifest" && -f "$compat_wf" && -f "$upgrade_wf" ]] \
	|| die "required release files are missing; run from the repository root"
command -v go >/dev/null || die "go is not installed"

branch="$(git symbolic-ref --quiet --short HEAD || echo '(detached)')"
if [[ "$branch" == main ]]; then info "branch: main"; else check_failed "branch is $branch; releases require main"; fi

if [[ -z "$(git status --porcelain)" ]]; then
	info "working tree is clean"
else
	git status --short >&2
	check_failed "working tree is not clean"
fi

info "fetch origin main"
git fetch --quiet origin main || die "cannot fetch origin/main; check network and permissions"
head_sha="$(git rev-parse HEAD)"
origin_sha="$(git rev-parse origin/main)"
if [[ "$head_sha" == "$origin_sha" ]]; then
	info "HEAD matches origin/main: ${head_sha:0:8}"
else
	check_failed "HEAD ${head_sha:0:8} differs from origin/main ${origin_sha:0:8}; fast-forward or push existing commits first"
fi

previous="$(rel_manifest_release "$manifest")"
rel_valid_version "$previous" || die "invalid release in $manifest: $previous"
if rel_version_gt "$version" "$previous"; then
	info "version: $previous -> $version"
else
	check_failed "$version must be newer than $previous in $manifest"
fi

if git rev-parse -q --verify "refs/tags/$version" >/dev/null; then
	check_failed "tag $version already exists locally"
else
	info "tag $version does not exist locally"
fi
remote_code=0
git ls-remote --exit-code --tags origin "refs/tags/$version" >/dev/null 2>&1 || remote_code=$?
case "$remote_code" in
0) check_failed "tag $version already exists on origin" ;;
2) info "tag $version does not exist on origin" ;;
*) die "cannot check remote tag (git ls-remote exit $remote_code); fix connectivity or permissions" ;;
esac

if rel_changelog_has_unreleased "$changelog"; then
	info "CHANGELOG [Unreleased] has entries"
else
	check_failed "CHANGELOG [Unreleased] is empty; document the release first"
fi

notes="docs/release/$version-NOTES.md"
if rel_is_feature "$version"; then
	if [[ -f "$notes" ]]; then info "feature release notes exist: $notes"; else check_failed "feature releases require $notes with upgrade requirements, behavior changes and migration instructions"; fi
else
	info "patch release: CHANGELOG only; no separate release notes required"
fi

if [[ "$upgrade_compat" == auto ]]; then
	if rel_is_feature "$version"; then upgrade_compat=yes; else upgrade_compat=no; fi
fi
old_min="$(rel_min_core "$gen_manifest")"
if ((min_core)); then info "minimum core: $old_min -> $version (--min-core)"; else info "minimum core remains $old_min"; fi
if [[ "$upgrade_compat" == yes ]]; then info "append $previous to upgrade-compat matrix"; else info "upgrade-compat matrix unchanged"; fi

say "Prepare version changes"
stage="$(mktemp -d "${TMPDIR:-/tmp}/roost-release.XXXXXX")"
trap 'rm -rf -- "$stage"' EXIT
changed=()
render() { # render <file> <rel_render_function> [arguments...]
	local file="$1" fn="$2" out
	shift 2
	out="$stage/$(printf '%s' "$file" | tr '/' '_')"
	"$fn" "$file" "$@" >"$out" || die "cannot render $file"
	if ! cmp -s "$file" "$out"; then
		changed+=("$file")
		info "update $file"
	fi
}
render "$manifest" rel_render_manifest_release "$version"
if rel_changelog_has_unreleased "$changelog"; then
	render "$changelog" rel_render_changelog "$version" "$today"
fi
if [[ "$upgrade_compat" == yes ]]; then
	render "$upgrade_wf" rel_render_upgrade_compat "$previous"
fi
if ((min_core)); then
	render "$gen_manifest" rel_render_min_core "$version"
	render "$compat_wf" rel_render_compat_minimum "$old_min" "$version"
fi

staged_of() { printf '%s/%s' "$stage" "$(printf '%s' "$1" | tr '/' '_')"; }

if ((dry_run)); then
	say "Preview (dry-run; working tree unchanged)"
	for file in ${changed[@]+"${changed[@]}"}; do
		diff -u --label "a/$file" --label "b/$file" "$file" "$(staged_of "$file")" | sed -n '1,60p' || true
	done
	if ((min_core)); then
		info "--min-core changes values only; document the reason in minimumVersions in $gen_manifest"
	fi
	say "dry-run complete: commit release, run pretag, push main, create and push tag, then verify game-demo"
	if ((failures)); then
		printf 'release: dry-run found %d preflight failures; a release stops at the first failure\n' "$failures" >&2
		exit 1
	fi
	exit 0
fi

for file in ${changed[@]+"${changed[@]}"}; do
	staged="$(staged_of "$file")"
	cat "$staged" >"$file" # Preserve file permissions.
done

say "Commit release: $version"
git add -- ${changed[@]+"${changed[@]}"}
{
	printf 'release: %s\n\n' "$version"
	printf '%s release: %s -> %s; archive CHANGELOG [Unreleased] as [%s] - %s.\n' "$manifest" "$previous" "$version" "$version" "$today"
	if [[ "$upgrade_compat" == yes ]]; then
		printf 'Append %s to the upgrade-compat previous-project matrix.\n' "$previous"
	fi
	if ((min_core)); then
		printf 'Minimum core: %s -> %s (manifest.go and framework-compat minimum lane).\n' "$old_min" "$version"
	fi
	printf '\nGenerated by scripts/release.sh.\n'
} >"$stage/commit-msg"
git commit --quiet -F "$stage/commit-msg"
release_sha="$(git rev-parse HEAD)"
info "commit: ${release_sha:0:8}"

# ---- 4. pretag ---------------------------------------------------------------------------------
say "scripts/pretag.sh ${version} (build, vet, tidy, tests and generated-file checks; may take several minutes)"
if ! ./scripts/pretag.sh "$version"; then
	cat >&2 <<EOF

release: pretag failed. Nothing was pushed. Release commit ${release_sha:0:8} is local only.
  Inspect: git status && git show --stat HEAD
  Discard the local release commit AND verification changes: git reset --hard $head_sha
Fix the failure, then rerun scripts/release.sh ${version} on main.
EOF
	exit 1
fi

say "push main"
push_code=0
git push origin HEAD:main || push_code=$?
git fetch --quiet origin main || die "cannot verify push; check git merge-base --is-ancestor $release_sha origin/main"
if ((push_code != 0)) || ! git merge-base --is-ancestor "$release_sha" origin/main; then
	die "main push failed (exit $push_code or origin/main missing ${release_sha:0:8}); no tag created. Resolve the rejection before retrying"
fi
info "origin/main contains ${release_sha:0:8}"

# ---- 6. tag ------------------------------------------------------------------------------------
say "tag $version"
git tag -a "$version" -m "roost-core $version" "$release_sha"
git push origin "refs/tags/$version" || die "tag push failed; local tag exists. Retry git push origin refs/tags/$version after resolving the failure"
local_tag="$(git rev-parse "refs/tags/$version")"
remote_tag="$(git ls-remote --tags origin "refs/tags/$version" | awk '{print $1}')"
[[ "$remote_tag" == "$local_tag" ]] || die "remote tag $version is ${remote_tag:-missing}; local tag is $local_tag"
info "remote tag $version = ${local_tag:0:8} (commit ${release_sha:0:8})"

say "Post-release verification: generate game-demo using $version, resolve the tag, build and vet"
check_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-release-check.XXXXXX")"
trap 'rm -rf -- "$stage" "$check_dir"' EXIT
project="$check_dir/relcheck"
GOWORK=off go run ./codegen/cmd/roost project new relcheck -module example.com/relcheck -out "$project" \
	-template game-demo -roost-core-version "$version" -skip-deps >/dev/null \
	|| die "tag is published, but game-demo generation failed; retain the tag and investigate"
# Resolve from VCS to avoid proxy publication lag; never replace a published tag.
resolved=0
for attempt in 1 2 3 4 5 6; do
	if (cd "$project" && GOWORK=off GOPROXY=direct GONOSUMDB="$module_path" go get "$module_path@$version" \
		&& GOWORK=off GOPROXY=direct GONOSUMDB="$module_path" go mod tidy); then
		resolved=1
		break
	fi
	((attempt < 6)) || break
	info "attempt $attempt to resolve $module_path@$version failed; retry in 30 seconds"
	sleep 30
done
if ((!resolved)); then
	cat >&2 <<EOF
release: tag is published, but $module_path@${version} could not be resolved within the retry window. Do not replace the tag.
Verify later from the repository at the released tag:
  GOWORK=off go run ./codegen/cmd/roost project new relcheck -module example.com/relcheck -out <temporary-directory>/relcheck -template game-demo -roost-core-version $version -skip-deps
  cd <temporary-directory>/relcheck && GOWORK=off GOPROXY=direct GONOSUMDB=$module_path go get $module_path@$version && GOWORK=off GOPROXY=direct GONOSUMDB=$module_path go mod tidy
  GOWORK=off go build ./... && GOWORK=off go vet ./...
EOF
	exit 1
fi
(cd "$project" && GOWORK=off go build ./... && GOWORK=off go vet ./...) \
	|| die "tag is published, but game-demo build/vet failed for $version; retain the tag and investigate"
info "game-demo @ $version build and vet passed"

printf '\nrelease: %s published (commit %s, tag %s). Not waiting for GitHub CI.\n' "$version" "${release_sha:0:8}" "${local_tag:0:8}"
