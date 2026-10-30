# shellcheck shell=bash
# scripts/release.sh 的纯文本部分：版本号校验与比较、CHANGELOG 归档、版本号所在文件的改写。
#
# 只做“读文件 → 把改好的全文写到 stdout”，不碰 git、不碰网络，scripts/release_test.sh 直接
# source 本文件用临时文件测。出错时把原因写到 stderr 并返回非 0，调用方决定是否停下。
# macOS 自带 bash 3.2：不用关联数组、mapfile、${var,,}。

# rel_valid_version V：只接受 vMAJOR.MINOR.PATCH（与 codegen manifest 的 releaseVersionPattern 一致；
# 发布 tag 不带预发布后缀，数字不带前导 0）。
rel_valid_version() {
	[[ "${1:-}" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
}

# rel_version_gt A B：A 严格大于 B 时返回 0。两边都须先过 rel_valid_version。
rel_version_gt() {
	local a1 a2 a3 b1 b2 b3
	IFS=. read -r a1 a2 a3 <<<"${1#v}"
	IFS=. read -r b1 b2 b3 <<<"${2#v}"
	if ((a1 != b1)); then
		((a1 > b1))
		return
	fi
	if ((a2 != b2)); then
		((a2 > b2))
		return
	fi
	((a3 > b3))
}

# rel_is_feature V：补丁号为 0（vX.Y.0）即功能版本。upgrade-compat 矩阵只在功能版本发版时追加上一版。
rel_is_feature() {
	[[ "${1##*.}" == 0 ]]
}

# rel_manifest_release FILE：读 codegen/ci/framework-release.yaml 的 release: 值。
rel_manifest_release() {
	awk '/^release:/{print $2; exit}' "$1"
}

# rel_render_manifest_release FILE VERSION：把唯一一行 release: 改成 VERSION。
rel_render_manifest_release() {
	local file="$1" version="$2" count
	count=$(grep -c '^release:' "$file" || true)
	if [[ "$count" != 1 ]]; then
		echo "release-lib: $file 应当恰好有一行 release:，实际 $count 行" >&2
		return 1
	fi
	awk -v v="$version" '/^release:/{print "release: " v; next} {print}' "$file"
}

# rel_changelog_has_unreleased FILE：## [Unreleased] 段存在且有非空白内容时返回 0。
rel_changelog_has_unreleased() {
	awk '
		/^## \[Unreleased\]/ { inside = 1; found = 1; next }
		inside && /^## / { exit }
		inside && NF { body = 1 }
		END { exit !(found && body) }
	' "$1"
}

# rel_render_changelog FILE VERSION DATE：把 [Unreleased] 的内容原样归到新的 ## [VERSION] - DATE 下，
# 上面留一个空的 ## [Unreleased]。格式与历次归档一致：
#
#   ## [Unreleased]
#
#   ## [v1.32.0] - 2026-10-30
#
#   <原 Unreleased 内容>
rel_render_changelog() {
	local file="$1" version="$2" date="$3"
	if ! grep -q '^## \[Unreleased\]' "$file"; then
		echo "release-lib: $file 没有 ## [Unreleased] 段" >&2
		return 1
	fi
	if grep -qF "## [$version]" "$file"; then
		echo "release-lib: $file 已经有 ## [$version] 段" >&2
		return 1
	fi
	if ! rel_changelog_has_unreleased "$file"; then
		echo "release-lib: $file 的 [Unreleased] 是空的，没有可归档的变化" >&2
		return 1
	fi
	awk -v head="## [$version] - $date" '
		!done && /^## \[Unreleased\]/ { print; print ""; print head; skipping = 1; done = 1; next }
		skipping && !NF { next }
		skipping { print ""; skipping = 0 }
		{ print }
	' "$file"
}

# rel_min_core FILE：读 codegen/internal/roost/manifest.go 里 minimumVersions 的 Core（与 codegen/scripts/core-pin.sh 同一读法）。
rel_min_core() {
	awk '
		/^var minimumVersions = VersionSpec[{]/ { inside = 1; next }
		inside && /^}/ { exit }
		inside && $1 == "Core:" { gsub(/[",]/, "", $2); print $2; exit }
	' "$1"
}

# rel_render_min_core FILE VERSION：只改 minimumVersions 块里的 Core 值，注释（每次提高的原因）由人写。
rel_render_min_core() {
	local file="$1" version="$2" old
	old=$(rel_min_core "$file")
	if ! rel_valid_version "$old"; then
		echo "release-lib: 读不出 $file 的 minimumVersions.Core（得到 '$old'）" >&2
		return 1
	fi
	awk -v v="$version" '
		/^var minimumVersions = VersionSpec[{]/ { inside = 1 }
		inside && /^}/ { inside = 0 }
		inside && $1 == "Core:" { sub(/"v[0-9.]+"/, "\"" v "\"") }
		{ print }
	' "$file"
}

# rel_render_compat_minimum FILE OLD NEW：framework-compat.yml 的 minimum lane 里钉最低 core 的三处
# （-roost-core-version、Makefile 生成器检查的 grep 与报错文本）一起从 OLD 改成 NEW；
# 改完 OLD 不得残留，且两处被 TestFrameworkCompatMinimumSetMatchesTheGeneratorFloor 比对的写法都在。
rel_render_compat_minimum() {
	local file="$1" old="$2" new="$3" re out
	re="${old//./\\.}"
	if ! grep -q -- "-roost-core-version $re)" "$file"; then
		echo "release-lib: $file 里没有 '-roost-core-version $old)'，minimum lane 与生成器下限已不一致" >&2
		return 1
	fi
	out=$(sed -e "s/-roost-core-version $re)/-roost-core-version $new)/" \
		-e "s/cmd\/roost@$re /cmd\/roost@$new /g" \
		-e "s/pinned core $re\"/pinned core $new\"/" "$file")
	if grep -q -- "$re" <<<"$out"; then
		echo "release-lib: $file 改完仍有 $old，有新的钉版本写法，先更新 rel_render_compat_minimum" >&2
		return 1
	fi
	if ! grep -q -- "-roost-core-version $new)" <<<"$out" || ! grep -qF "grep -v 'cmd/roost@$new '" <<<"$out"; then
		echo "release-lib: $file 改完缺少 minimum lane 的两处钉版本写法" >&2
		return 1
	fi
	printf '%s\n' "$out"
}

# rel_render_upgrade_compat FILE VERSION：upgrade-compat.yml 里 previous-project 任务的矩阵 core: [...] 末尾追加 VERSION；
# 已在矩阵里则原样输出。只认 previous-project 任务下的第一行 core:（同文件的 player-access-project 有自己的固定矩阵）。
rel_render_upgrade_compat() {
	local file="$1" version="$2"
	if ! grep -q '^  previous-project:' "$file"; then
		echo "release-lib: $file 里没有 previous-project 任务" >&2
		return 1
	fi
	awk -v v="$version" '
		/^  [A-Za-z0-9_-]+:/ { job = $1 }
		job == "previous-project:" && !done && /^[[:space:]]+core: \[[^]]*\]/ {
			done = 1
			lb = index($0, "["); rb = index($0, "]")
			list = substr($0, lb + 1, rb - lb - 1)
			n = split(list, items, ",")
			for (i = 1; i <= n; i++) { gsub(/[[:space:]]/, "", items[i]); if (items[i] == v) { print; next } }
			sep = (list ~ /[^[:space:]]/) ? ", " : ""
			print substr($0, 1, rb - 1) sep v substr($0, rb)
			next
		}
		{ print }
		END { if (!done) { print "release-lib: previous-project 任务下没有 core: [...] 矩阵" > "/dev/stderr"; exit 1 } }
	' "$file"
}
