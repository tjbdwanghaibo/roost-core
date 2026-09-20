package servicerpc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M-11 · ARCH-01/04：接口住在 core 领域包、装配住在 kit 时，两半要能分开生成。
// `-emit transport` 只写传输半（core 包用），`-emit assembly -dir <core 包> -out .`
// 只把装配半写进另一个目录（kit 包用），文件头记录实际用的命令。
func TestEitherHalfCanBeEmittedAloneIntoAnotherDirectory(t *testing.T) {
	src := writeDir(t, goldenService)
	out := t.TempDir()
	var log strings.Builder
	if err := Run([]string{"-dir", src, "-emit", "transport"}, &log); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"-dir", src, "-emit", "assembly", "-out", out}, &log); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(src, "shop_rpc_gen.go")); err != nil {
		t.Fatalf("transport half not written next to the interface: %v", err)
	}
	if _, err := os.Stat(filepath.Join(src, "shop_rpc_assembly_gen.go")); !os.IsNotExist(err) {
		t.Fatalf("-emit transport wrote the assembly half too (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(out, "shop_rpc_gen.go")); !os.IsNotExist(err) {
		t.Fatalf("-emit assembly wrote the transport half into -out (err=%v)", err)
	}
	assembly, err := os.ReadFile(filepath.Join(out, "shop_rpc_assembly_gen.go"))
	if err != nil {
		t.Fatalf("assembly half not written into -out: %v", err)
	}
	want := "//\tgo run github.com/tjbdwanghaibo/roost-codegen/cmd/servicerpc -dir " + src + " -emit assembly -out " + out
	if !strings.Contains(string(assembly), want) {
		t.Fatalf("the assembly header does not record how it was produced; want %q in:\n%s", want, firstLines(string(assembly), 16))
	}
	// -check in the same shape sees both as current, and a stale copy as stale.
	if err := Run([]string{"-dir", src, "-emit", "assembly", "-out", out, "-check"}, &log); err != nil {
		t.Fatalf("-check right after generating reports drift: %v", err)
	}
	// A CRLF checkout of the very same file is current: newlines are the
	// working tree's business (Windows, core.autocrlf), not the transport's.
	assemblyPath := filepath.Join(out, "shop_rpc_assembly_gen.go")
	if err := os.WriteFile(assemblyPath, []byte(strings.ReplaceAll(string(assembly), "\n", "\r\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"-dir", src, "-emit", "assembly", "-out", out, "-check"}, &log); err != nil {
		t.Fatalf("-check reports a CRLF checkout of an unchanged file as stale: %v", err)
	}
	if err := os.WriteFile(filepath.Join(out, "shop_rpc_assembly_gen.go"), []byte("package shop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"-dir", src, "-emit", "assembly", "-out", out, "-check"}, &log); err == nil {
		t.Fatal("-check accepted a hand-edited assembly half")
	}
	if err := Run([]string{"-dir", src, "-emit", "sideways"}, &log); err == nil || !strings.Contains(err.Error(), "-emit") {
		t.Fatalf("an unknown half was accepted: %v", err)
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// -dir accepts an import path and resolves it in the module context of -out,
// so a kit package's go:generate can name the core package rather than a path
// into the module cache.
func TestDirAcceptsAnImportPathResolvedInTheOutModule(t *testing.T) {
	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveDir("github.com/tjbdwanghaibo/roost-codegen/internal/servicerpc", here)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(resolved) != filepath.Clean(here) {
		t.Fatalf("resolved %q, want this package's directory %q", resolved, here)
	}
	if _, err := resolveDir("no-such-dir", here); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("a bare missing path was not refused: %v", err)
	}
	if _, err := resolveDir("example.invalid/nope/pkg", here); err == nil || !strings.Contains(err.Error(), "go list") {
		t.Fatalf("an unresolvable import path was not reported via go list: %v", err)
	}
}
