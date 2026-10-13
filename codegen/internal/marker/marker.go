// Package marker is the single definition of the source-comment markers the
// generators read (//roost:entity, //roost:dao, //roost:register, ...).
// Every parser goes through this package, so the accepted spelling lives in
// one place. Only the //roost: prefix is read; the retired //cube: prefix is
// not a marker, and FindRetiredPrefix lets `roost generate` refuse a project
// that still carries it instead of silently generating nothing for it.
package marker

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	// Prefix is the marker prefix generators read, emit and documentation shows.
	Prefix = "//roost:"
	// retiredPrefix is the pre-rename prefix. It is not read; it is only
	// looked for so that a project still using it fails with a clear error.
	retiredPrefix = "//cube:"

	// Provenance is the comment generators write into produced .proto files to
	// record that a definition came from a Go source of truth.
	Provenance = "roost:source=go_def"
)

// Regexp builds the anchored pattern `^//roost:<kind><suffix>$`. suffix is
// appended verbatim and may contain capture groups.
func Regexp(kind, suffix string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(Prefix+kind) + suffix + `$`)
}

// Cut returns the text following "//roost:<kind>" at the start of line, and
// whether line carried the marker at all.
func Cut(line, kind string) (rest string, ok bool) {
	return strings.CutPrefix(line, Prefix+kind)
}

// Has reports whether text contains the marker "//roost:<body>". body may
// include attributes, e.g. "reverse_proto ignore=true".
func Has(text, body string) bool {
	return strings.Contains(text, Prefix+body)
}

// HasProvenance reports whether line carries the generated-proto provenance comment.
func HasProvenance(line string) bool {
	return strings.Contains(line, Provenance)
}

// FindRetiredPrefix walks root and returns, sorted, every .go file (excluding
// tests, vendor and testdata) that still uses the retired //cube: prefix.
// `roost generate` refuses such a project: the parsers no longer read it, so
// the markers would otherwise be skipped without a word.
func FindRetiredPrefix(root string) ([]string, error) {
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
		if strings.Contains(string(raw), retiredPrefix) {
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
