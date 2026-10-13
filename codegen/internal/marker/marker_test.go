package marker

import (
	"os"
	"path/filepath"
	"testing"
)

// 只读 //roost: 前缀：退役的 //cube: 不是标记（Regexp / Cut / Has / HasProvenance 都不认），
// 由 FindRetiredPrefix 让 roost generate 明确拒绝。
func TestOnlyTheRoostPrefixIsRead(t *testing.T) {
	re := Regexp("entity", `\s+(.+)`)
	if match := re.FindStringSubmatch("//roost:entity kind=Player"); match == nil || match[1] != "kind=Player" {
		t.Fatalf("//roost:entity: got %v", match)
	}
	if re.MatchString("//cube:entity kind=Player") {
		t.Fatal("the retired //cube: prefix still matches")
	}
	if rest, ok := Cut("//roost:dao persist", "dao"); !ok || rest != " persist" {
		t.Fatalf("Cut(//roost:dao): rest=%q ok=%v", rest, ok)
	}
	if _, ok := Cut("//cube:dao persist", "dao"); ok {
		t.Fatal("Cut still reads the retired //cube: prefix")
	}
	if !Has("x\n//roost:reverse_proto ignore=true\n", "reverse_proto ignore=true") || Has("x\n//cube:reverse_proto ignore=true\n", "reverse_proto ignore=true") {
		t.Fatal("Has must read //roost: only")
	}
	if !HasProvenance("// roost:source=go_def") || HasProvenance("// cube:source=go_def") {
		t.Fatal("HasProvenance must read the roost spelling only")
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

// FindRetiredPrefix 只看生产源码（不看测试、testdata、vendor），找出仍写 //cube: 的文件。
func TestFindRetiredPrefixReportsProductionSourcesOnly(t *testing.T) {
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
	write("game/c/c.go", "package c\n\n//cube:dao persist\ntype C struct{}\n")
	write("game/a/a_test.go", "package a\n//cube:entity\n")
	write("game/a/testdata/x.go", "package x\n//cube:entity\n")
	write("vendor/v/v.go", "package v\n//cube:entity\n")

	got, err := FindRetiredPrefix(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"game/a/a.go", "game/c/c.go"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}
