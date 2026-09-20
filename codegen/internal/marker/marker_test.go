package marker

import (
	"os"
	"path/filepath"
	"testing"
)

// Business code carries these markers by hand, so the old prefix must keep
// parsing for one release — and the new one must parse identically, or the
// migration guide is wrong.
func TestBothPrefixesParseIdentically(t *testing.T) {
	re := Regexp("entity", `\s+(.+)`)
	for _, line := range []string{"//roost:entity kind=Player", "//cube:entity kind=Player"} {
		match := re.FindStringSubmatch(line)
		if match == nil || match[1] != "kind=Player" {
			t.Fatalf("%q: got %v", line, match)
		}
	}
	for _, line := range []string{"//roost:dao persist", "//cube:dao persist"} {
		rest, ok := Cut(line, "dao")
		if !ok || rest != " persist" {
			t.Fatalf("Cut(%q): rest=%q ok=%v", line, rest, ok)
		}
	}
	for _, text := range []string{"x\n//roost:reverse_proto ignore=true\n", "x\n//cube:reverse_proto ignore=true\n"} {
		if !Has(text, "reverse_proto ignore=true") {
			t.Fatalf("Has missed %q", text)
		}
	}
	for _, line := range []string{"// roost:source=go_def", "// cube:source=go_def"} {
		if !HasProvenance(line) {
			t.Fatalf("HasProvenance missed %q", line)
		}
	}
}

// A kind must not match a longer kind that shares its prefix, and the anchor
// must hold: "//roost:entity" is not "//roost:entitykinds".
func TestRegexpIsAnchoredAndKindIsLiteral(t *testing.T) {
	re := Regexp("entity", `\s+(.+)`)
	for _, line := range []string{"//roost:entitykinds a", "x //roost:entity a", "//roost:entity"} {
		if re.MatchString(line) {
			t.Fatalf("%q should not match", line)
		}
	}
	if _, ok := Cut("//roost:daoish x", "dao"); !ok {
		// Cut is a prefix cut by design; the caller validates the remainder.
		t.Fatal("Cut is documented as a prefix cut")
	}
	// A kind containing regexp metacharacters is quoted, not interpreted.
	if !Regexp("a.b", ``).MatchString("//roost:a.b") || Regexp("a.b", ``).MatchString("//roost:aXb") {
		t.Fatal("kind was interpreted as a pattern instead of a literal")
	}
}

// FindLegacy is what tells the author what to migrate; it must see production
// sources only and must report the provenance spelling too.
func TestFindLegacyReportsProductionSourcesOnly(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("game/a/a.go", "package a\n\n//cube:entity kind=A\ntype A struct{}\n")
	write("game/b/b.go", "package b\n\n//roost:entity kind=B\ntype B struct{}\n")
	write("protocol/gen.proto.go", "package p\n// cube:source=go_def\n")
	write("game/a/a_test.go", "package a\n//cube:entity\n")
	write("game/a/testdata/x.go", "package x\n//cube:entity\n")
	write("vendor/v/v.go", "package v\n//cube:entity\n")

	got, err := FindLegacy(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"game/a/a.go", "protocol/gen.proto.go"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}
