package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// glsvet 是 CI 与生成工程的并发边界门禁，只按退出码判定：参数指向的目录不存在、`<dir>/...` 的根不存在、或目录里有一份
// 解析不了的 .go 文件时，必须报错并以非 0 退出（2，与用法错误同级），不能把没检查到的输入算成“0 个违例”。

// TestMain lets the test binary stand in for the glsvet command, so the exit
// status — the only thing CI reads — is what these tests assert.
func TestMain(m *testing.M) {
	if os.Getenv("GLSVET_RUN_AS_MAIN") == "1" {
		os.Args = append([]string{"glsvet"}, strings.Fields(os.Getenv("GLSVET_ARGS"))...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runGlsvet(t *testing.T, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "GLSVET_RUN_AS_MAIN=1", "GLSVET_ARGS="+strings.Join(args, " "))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err == nil {
		return 0, out.String()
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("run glsvet: %v", err)
	}
	return exitErr.ExitCode(), out.String()
}

func TestMissingDirectoryArgumentFails(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-package")
	for _, argument := range []string{missing, missing + "/..."} {
		code, output := runGlsvet(t, argument)
		if code != 2 || !strings.Contains(output, "no-such-package") {
			t.Errorf("glsvet %s: exit %d, output %q; want exit 2 naming the missing directory (nothing was vetted)", argument, code, output)
		}
	}
}

func TestUnparsableFileFailsInsteadOfSkippingTheDirectory(t *testing.T) {
	dir := t.TempDir()
	// A real violation next to a file that does not parse: the old code dropped
	// the whole directory, violation included.
	if err := os.WriteFile(filepath.Join(dir, "handler.go"), []byte("package handler\n//roost:nest rollback=undo durability=strict\nfunc handlerMove() { go func() {}() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.go"), []byte("package handler\nfunc broken( {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, output := runGlsvet(t, dir)
	if code == 0 {
		t.Fatalf("glsvet on a directory with an unparsable file exited 0; output %q", output)
	}
	if !strings.Contains(output, "broken.go") {
		t.Errorf("output does not name the file that failed to parse: %q", output)
	}
}

func TestDirectoryWithoutGoFilesStillPasses(t *testing.T) {
	// Control: a directory with no Go files (docs/, deploy/ inside ./...) is
	// not an error, before or after the fix.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("docs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, output := runGlsvet(t, dir); code != 0 {
		t.Fatalf("glsvet on a directory without Go files: exit %d, output %q", code, output)
	}
	if code, output := runGlsvet(t, dir+"/..."); code != 0 {
		t.Fatalf("glsvet on %s/...: exit %d, output %q", dir, code, output)
	}
}
