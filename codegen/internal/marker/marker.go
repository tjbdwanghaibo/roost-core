// Package marker is the single definition of the source-comment markers the
// generators read (//roost:entity, //roost:dao, //roost:register, ...).
//
// The markers were renamed from //cube: to //roost:. Business code carries
// them by hand, so the old prefix is still accepted for this release and
// reported as deprecated; every parser goes through this package so the
// acceptance rule and the deprecation live in one place.
package marker

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	// Prefix is the marker prefix generators emit and documentation shows.
	Prefix = "//roost:"
	// LegacyPrefix is accepted when reading and never emitted.
	LegacyPrefix = "//cube:"

	// Provenance is the comment generators write into produced .proto files to
	// record that a definition came from a Go source of truth.
	Provenance       = "roost:source=go_def"
	legacyProvenance = "cube:source=go_def"
)

// Regexp builds the anchored pattern `^//(roost|cube):<kind><suffix>$`.
// suffix is appended verbatim and may contain capture groups; the prefix
// group is non-capturing so callers' group numbering is unchanged.
func Regexp(kind, suffix string) *regexp.Regexp {
	return regexp.MustCompile(`^//(?:roost|cube):` + regexp.QuoteMeta(kind) + suffix + `$`)
}

// Cut returns the text following "//roost:<kind>" or "//cube:<kind>" at the
// start of line, and whether line carried the marker at all.
func Cut(line, kind string) (rest string, ok bool) {
	for _, prefix := range []string{Prefix, LegacyPrefix} {
		if rest, ok = strings.CutPrefix(line, prefix+kind); ok {
			return rest, true
		}
	}
	return "", false
}

// Has reports whether text contains the marker "<prefix><body>" under either
// prefix. body may include attributes, e.g. "reverse_proto ignore=true".
func Has(text, body string) bool {
	return strings.Contains(text, Prefix+body) || strings.Contains(text, LegacyPrefix+body)
}

// HasProvenance reports whether line carries the generated-proto provenance
// comment under either spelling.
func HasProvenance(line string) bool {
	return strings.Contains(line, Provenance) || strings.Contains(line, legacyProvenance)
}

// Both returns the current and legacy spellings of a marker body, for callers
// that match against a fixed list rather than parsing a line.
func Both(body string) []string {
	return []string{Prefix + body, LegacyPrefix + body}
}

// FindLegacy walks root and returns, sorted, every .go file (excluding tests,
// vendor and testdata) that still uses the //cube: prefix. It exists so
// `roost generate` can tell the author what to migrate; the parsers accept
// the old spelling silently.
func FindLegacy(root string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != root && (name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(raw), LegacyPrefix) || strings.Contains(string(raw), legacyProvenance) {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(found)
	return found, nil
}
