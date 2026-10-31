# shellcheck shell=bash
# Pure release-file transformations. Callers stage output before replacing source files.
# Compatible with macOS Bash 3.2. No Git/network mutations; errors go to stderr.

# Accept release tags only: vMAJOR.MINOR.PATCH, without leading zeros or suffixes.
rel_valid_version() {
	[[ "${1:-}" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
}

# Return success when validated version A is numerically newer than B.
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

# A zero patch number denotes a feature release.
rel_is_feature() {
	[[ "${1##*.}" == 0 ]]
}

# Read the release version from the repository release manifest.
rel_manifest_release() {
	awk '/^release:/{print $2; exit}' "$1"
}

# Replace exactly one release entry, preserving unrelated lines.
rel_render_manifest_release() {
	local file="$1" version="$2" count
	count=$(grep -c '^release:' "$file" || true)
	if [[ "$count" != 1 ]]; then
		echo "release-lib: expected exactly one release: entry in $file, got $count" >&2
		return 1
	fi
	awk -v v="$version" '/^release:/{print "release: " v; next} {print}' "$file"
}

# Require nonblank content in the Unreleased section.
rel_changelog_has_unreleased() {
	awk '
		/^## \[Unreleased\]/ { inside = 1; found = 1; next }
		inside && /^## / { exit }
		inside && NF { body = 1 }
		END { exit !(found && body) }
	' "$1"
}

#   ## [Unreleased]
#   ## [v1.32.0] - 2026-10-30
# Archive Unreleased under VERSION and DATE, leaving a new empty Unreleased section.
rel_render_changelog() {
	local file="$1" version="$2" date="$3"
	if ! grep -q '^## \[Unreleased\]' "$file"; then
		echo "release-lib: missing [Unreleased] section in $file" >&2
		return 1
	fi
	if grep -qF "## [$version]" "$file"; then
		echo "release-lib: [$version] already exists in $file" >&2
		return 1
	fi
	if ! rel_changelog_has_unreleased "$file"; then
		echo "release-lib: [Unreleased] in $file has no changes to archive" >&2
		return 1
	fi
	awk -v head="## [$version] - $date" '
		!done && /^## \[Unreleased\]/ { print; print ""; print head; skipping = 1; done = 1; next }
		skipping && !NF { next }
		skipping { print ""; skipping = 0 }
		{ print }
	' "$file"
}

# Read the generated consumer minimum version from manifest.go.
rel_min_core() {
	awk '
		/^var minimumVersions = VersionSpec[{]/ { inside = 1; next }
		inside && /^}/ { exit }
		inside && $1 == "Core:" { gsub(/[",]/, "", $2); print $2; exit }
	' "$1"
}

# Update only the generated consumer minimum version.
rel_render_min_core() {
	local file="$1" version="$2" old
	old=$(rel_min_core "$file")
	if ! rel_valid_version "$old"; then
		echo "release-lib: invalid minimumVersions.Core in $file: $old" >&2
		return 1
	fi
	awk -v v="$version" '
		/^var minimumVersions = VersionSpec[{]/ { inside = 1 }
		inside && /^}/ { inside = 0 }
		inside && $1 == "Core:" { sub(/"v[0-9.]+"/, "\"" v "\"") }
		{ print }
	' "$file"
}

# Update both known minimum-lane pins; fail if an unknown old pin remains.
rel_render_compat_minimum() {
	local file="$1" old="$2" new="$3" re out
	re="${old//./\\.}"
	if ! grep -q -- "-roost-core-version $re)" "$file"; then
		echo "release-lib: minimum lane in $file does not match $old" >&2
		return 1
	fi
	out=$(sed -e "s/-roost-core-version $re)/-roost-core-version $new)/" \
		-e "s/cmd\/roost@$re /cmd\/roost@$new /g" \
		-e "s/pinned core $re\"/pinned core $new\"/" "$file")
	if grep -q -- "$re" <<<"$out"; then
		echo "release-lib: $old remains in $file; update rel_render_compat_minimum for new pin formats" >&2
		return 1
	fi
	if ! grep -q -- "-roost-core-version $new)" <<<"$out" || ! grep -qF "grep -v 'cmd/roost@$new '" <<<"$out"; then
		echo "release-lib: missing minimum lane version pins after rendering $file" >&2
		return 1
	fi
	printf '%s\n' "$out"
}

# Append to previous-project only; leave unrelated matrices unchanged.
rel_render_upgrade_compat() {
	local file="$1" version="$2"
	if ! grep -q '^  previous-project:' "$file"; then
		echo "release-lib: missing previous-project job in $file" >&2
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
		END { if (!done) { print "release-lib: previous-project has no core: [...] matrix" > "/dev/stderr"; exit 1 } }
	' "$file"
}
