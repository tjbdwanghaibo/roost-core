#!/usr/bin/env bash
# 一条命令发版（维护者 2026-10-30 决定）：在 main 的干净工作树上
#
#   1. 前置检查：在 main、工作树干净、与 origin/main 一致、版本号合法且比 framework-release.yaml 新、
#      tag 本地与远端都不存在、CHANGELOG [Unreleased] 非空；
#   2. 改版本号：framework-release.yaml 的 release:；CHANGELOG 把 [Unreleased] 归档为 [vX.Y.Z] - 今天；
#      功能版本（vX.Y.0）给 upgrade-compat 的 previous-project 矩阵追加上一版；--min-core 时把生成物最低 core
#      （manifest.go minimumVersions.Core）与 framework-compat 的 minimum lane 一起提到本版本；
#   3. 提交 “release：vX.Y.Z”；
#   4. scripts/pretag.sh vX.Y.Z（失败即停，不推送，打印回退命令）；
#   5. push main，fetch 后独立核对远端包含本提交（管道会掩盖被拒的 push，v1.26.0 遇到过）；
#   6. annotated tag，push tag，ls-remote 核对远端 tag 与本地一致；
#   7. 发布后验证：临时目录用本 tag 的生成器生成 game-demo，GOPROXY=direct 解析刚推的 tag，build + vet，删临时目录。
#
# 用法：
#   scripts/release.sh vX.Y.Z [--min-core] [--upgrade-compat|--no-upgrade-compat] [--dry-run]
#
#   --min-core            本版生成物用到了新 API：同时提高生成物最低 core（原因写进 manifest.go 的注释，脚本只改值）
#   --upgrade-compat      补丁版也追加上一版到 upgrade-compat 矩阵（缺省只有功能版本追加）
#   --no-upgrade-compat   功能版本也不追加
#   --dry-run             只做前置检查并预览本地改动（diff），不改工作树、不提交、不推送；预检失败也继续预览，最后以 1 退出
#
# 版本说明文档不在本脚本里：补丁版只靠 CHANGELOG；功能版在跑本脚本之前把 docs/release/vX.Y.Z-NOTES.md
# 写好并合进 main（规则见 docs/maintenance/README.md §5），前置检查会核对它存在。
#
# 不打印任何环境变量或远端 URL（远端 URL 可能带凭据）。
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=scripts/release-lib.sh disable=SC1091
. "$repo_root/scripts/release-lib.sh"

usage() {
	sed -n '/^# 用法：/,/^# 版本说明文档/p' "$0" | sed -e 's/^# \{0,1\}//' -e '$d' >&2
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
	*) echo "release: 未知参数 $arg" >&2; usage ;;
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

# ---- 1. 前置检查 -------------------------------------------------------------------------------
# dry-run 时检查失败只记下来、继续往下预览；正式发版第一条失败就停。
failures=0
check_failed() {
	if ((dry_run)); then
		printf '  预检失败：%s\n' "$*"
		failures=$((failures + 1))
	else
		die "$*"
	fi
}

say "前置检查 $version"
rel_valid_version "$version" || die "$version 不是 vMAJOR.MINOR.PATCH"
[[ -f "$manifest" && -f "$changelog" && -f "$gen_manifest" && -f "$compat_wf" && -f "$upgrade_wf" ]] \
	|| die "缺少发版要改的文件（应在 roost-core 仓库根运行）"
command -v go >/dev/null || die "找不到 go"

branch="$(git symbolic-ref --quiet --short HEAD || echo '(detached)')"
if [[ "$branch" == main ]]; then info "分支 main"; else check_failed "当前分支是 $branch，发版只在 main 上做"; fi

if [[ -z "$(git status --porcelain)" ]]; then
	info "工作树干净"
else
	git status --short >&2
	check_failed "工作树不干净"
fi

info "fetch origin main"
git fetch --quiet origin main || die "fetch origin main 失败（网络或权限），不能确认与远端一致"
head_sha="$(git rev-parse HEAD)"
origin_sha="$(git rev-parse origin/main)"
if [[ "$head_sha" == "$origin_sha" ]]; then
	info "HEAD 与 origin/main 一致：${head_sha:0:8}"
else
	check_failed "HEAD ${head_sha:0:8} 与 origin/main ${origin_sha:0:8} 不一致（先 pull --ff-only 或推送已有提交）"
fi

previous="$(rel_manifest_release "$manifest")"
rel_valid_version "$previous" || die "$manifest 的 release: 读不出合法版本（得到 '$previous'）"
if rel_version_gt "$version" "$previous"; then
	info "版本 $previous → $version"
else
	check_failed "$version 不比 $manifest 当前的 $previous 新"
fi

if git rev-parse -q --verify "refs/tags/$version" >/dev/null; then
	check_failed "tag $version 本地已存在"
else
	info "本地没有 tag $version"
fi
# 与 pretag 同一判法：--exit-code 返回 2 才是“远端没有”，其他非 0 是网络 / 认证错误，不能当作不存在。
remote_code=0
git ls-remote --exit-code --tags origin "refs/tags/$version" >/dev/null 2>&1 || remote_code=$?
case "$remote_code" in
0) check_failed "tag $version 远端已存在" ;;
2) info "远端没有 tag $version" ;;
*) die "查不了远端 tag（git ls-remote 退出码 $remote_code），修好网络或权限再跑" ;;
esac

if rel_changelog_has_unreleased "$changelog"; then
	info "CHANGELOG [Unreleased] 有 $(awk '/^## \[Unreleased\]/{f=1;next} f&&/^## /{exit} f&&/^- /{n++} END{print n+0}' "$changelog") 条顶层条目"
else
	check_failed "CHANGELOG [Unreleased] 是空的：先把本版变化写进去"
fi

# 发版文档分级（docs/maintenance/README.md §5）：功能版本（vX.Y.0）必须先有一篇发布说明；补丁版只靠 CHANGELOG。
notes="docs/release/$version-NOTES.md"
if rel_is_feature "$version"; then
	if [[ -f "$notes" ]]; then info "功能版本，发布说明 $notes 已在"; else check_failed "功能版本要先写 $notes（升级要求、行为变化、迁移）并合进 main"; fi
else
	info "补丁版本：不需要 docs/release 文档，只靠 CHANGELOG"
fi

if [[ "$upgrade_compat" == auto ]]; then
	if rel_is_feature "$version"; then upgrade_compat=yes; else upgrade_compat=no; fi
fi
old_min="$(rel_min_core "$gen_manifest")"
if ((min_core)); then info "生成物最低 core $old_min → $version（--min-core）"; else info "生成物最低 core 保持 $old_min"; fi
if [[ "$upgrade_compat" == yes ]]; then info "upgrade-compat 矩阵追加上一版 $previous"; else info "upgrade-compat 矩阵不变"; fi

# ---- 2. 生成改动 -------------------------------------------------------------------------------
say "生成版本号改动"
stage="$(mktemp -d "${TMPDIR:-/tmp}/roost-release.XXXXXX")"
trap 'rm -rf -- "$stage"' EXIT
changed=()
render() { # render <file> <rel_render_* 函数> [参数...]
	local file="$1" fn="$2" out
	shift 2
	out="$stage/$(printf '%s' "$file" | tr '/' '_')"
	"$fn" "$file" "$@" >"$out" || die "改写 $file 失败"
	if ! cmp -s "$file" "$out"; then
		changed+=("$file")
		info "改 $file"
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
	say "预览（dry-run：工作树不改）"
	for file in ${changed[@]+"${changed[@]}"}; do
		diff -u --label "a/$file" --label "b/$file" "$file" "$(staged_of "$file")" | sed -n '1,60p' || true
	done
	if ((min_core)); then
		info "提醒：--min-core 只改值；提高原因写进 $gen_manifest 的 minimumVersions 注释（与本版说明同一句话）"
	fi
	say "dry-run 结束：之后会提交 release：$version，跑 scripts/pretag.sh $version，push main，打 tag 并 push，最后生成 game-demo 验证"
	if ((failures)); then
		printf 'release: dry-run 有 %d 项预检失败，正式发版会在第一项停下\n' "$failures" >&2
		exit 1
	fi
	exit 0
fi

for file in ${changed[@]+"${changed[@]}"}; do
	staged="$(staged_of "$file")"
	cat "$staged" >"$file" # 用 cat 覆盖而不是 mv：保留原文件权限
done

# ---- 3. 提交 -----------------------------------------------------------------------------------
say "提交 release：$version"
git add -- ${changed[@]+"${changed[@]}"}
{
	printf 'release：%s\n\n' "$version"
	printf '%s release: %s → %s；CHANGELOG [Unreleased] 归档为 [%s] - %s。\n' "$manifest" "$previous" "$version" "$version" "$today"
	if [[ "$upgrade_compat" == yes ]]; then
		printf 'upgrade-compat previous-project 矩阵追加 %s。\n' "$previous"
	fi
	if ((min_core)); then
		printf '生成物最低 core %s → %s（manifest.go minimumVersions.Core 与 framework-compat minimum lane）。\n' "$old_min" "$version"
	fi
	printf '\n由 scripts/release.sh 生成。\n'
} >"$stage/commit-msg"
git commit --quiet -F "$stage/commit-msg"
release_sha="$(git rev-parse HEAD)"
info "提交 ${release_sha:0:8}"

# ---- 4. pretag ---------------------------------------------------------------------------------
say "scripts/pretag.sh $version（整仓 build / vet / tidy / test 与生成漂移检查，约十几分钟）"
if ! ./scripts/pretag.sh "$version"; then
	cat >&2 <<EOF

release: pretag 失败，什么都没推送。release 提交 ${release_sha:0:8} 只在本地。
  看现场：git status && git show --stat HEAD
  全部回退（丢弃 release 提交和 pretag 留下的改动）：git reset --hard $head_sha
修好之后在 main 上重新跑 scripts/release.sh $version。
EOF
	exit 1
fi

# ---- 5. push main 并独立核对 ------------------------------------------------------------------
say "push main"
push_code=0
git push origin HEAD:main || push_code=$?
git fetch --quiet origin main || die "push 后 fetch 失败，无法确认远端；手工核对 git merge-base --is-ancestor $release_sha origin/main"
if ((push_code != 0)) || ! git merge-base --is-ancestor "$release_sha" origin/main; then
	die "push main 没有生效（退出码 $push_code，origin/main 不含 ${release_sha:0:8}）；没有打 tag。先处理被拒原因（远端前进了就 pull --rebase 后重跑）"
fi
info "origin/main 已包含 ${release_sha:0:8}"

# ---- 6. tag ------------------------------------------------------------------------------------
say "tag $version"
git tag -a "$version" -m "roost-core $version" "$release_sha"
git push origin "refs/tags/$version" || die "push tag 失败；本地 tag 已建，修好后 git push origin refs/tags/$version"
local_tag="$(git rev-parse "refs/tags/$version")"
remote_tag="$(git ls-remote --tags origin "refs/tags/$version" | awk '{print $1}')"
[[ "$remote_tag" == "$local_tag" ]] || die "远端 tag $version 是 '${remote_tag:-无}'，本地是 $local_tag"
info "远端 tag $version = ${local_tag:0:8}（指向 ${release_sha:0:8}）"

# ---- 7. 发布后验证 -----------------------------------------------------------------------------
say "发布后验证：用 $version 的生成器生成 game-demo，解析刚推的 tag，build + vet"
check_dir="$(mktemp -d "${TMPDIR:-/tmp}/roost-release-check.XXXXXX")"
trap 'rm -rf -- "$stage" "$check_dir"' EXIT
project="$check_dir/relcheck"
GOWORK=off go run ./codegen/cmd/roost project new relcheck -module example.com/relcheck -out "$project" \
	-template game-demo -roost-core-version "$version" -skip-deps >/dev/null \
	|| die "tag 已发布，但生成 game-demo 失败（发布后验证未通过，tag 不动，排查后补验）"
# 模块代理与 sumdb 对新 tag 有几分钟延迟：直连 VCS、跳过本模块的 sumdb，失败就隔一会儿重试。
resolved=0
for attempt in 1 2 3 4 5 6; do
	if (cd "$project" && GOWORK=off GOPROXY=direct GONOSUMDB="$module_path" go get "$module_path@$version" \
		&& GOWORK=off GOPROXY=direct GONOSUMDB="$module_path" go mod tidy); then
		resolved=1
		break
	fi
	((attempt < 6)) || break
	info "第 $attempt 次解析 $module_path@$version 失败，30 秒后重试"
	sleep 30
done
if ((!resolved)); then
	cat >&2 <<EOF
release: tag 已发布，但约 3 分钟内解析不到 $module_path@$version（发布后验证未通过，tag 不动）。
稍后手工补验（仓库根、tag 检出上）：
  GOWORK=off go run ./codegen/cmd/roost project new relcheck -module example.com/relcheck -out <临时目录>/relcheck -template game-demo -roost-core-version $version -skip-deps
  cd <临时目录>/relcheck && GOWORK=off GOPROXY=direct GONOSUMDB=$module_path go get $module_path@$version && GOWORK=off GOPROXY=direct GONOSUMDB=$module_path go mod tidy
  GOWORK=off go build ./... && GOWORK=off go vet ./...
EOF
	exit 1
fi
(cd "$project" && GOWORK=off go build ./... && GOWORK=off go vet ./...) \
	|| die "tag 已发布，但 game-demo 对 $version build / vet 失败（发布后验证未通过，tag 不动）"
info "game-demo @ $version build + vet 通过"

printf '\nrelease: %s 已发布（提交 %s，tag %s）。不等 GitHub CI。\n' "$version" "${release_sha:0:8}" "${local_tag:0:8}"
