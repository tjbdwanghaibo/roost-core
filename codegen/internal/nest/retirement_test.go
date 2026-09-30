package nest

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestRunRetiresNestOutputsAfterLastMarkerRemoved(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "handler.go")
	write := func(marker string) {
		t.Helper()
		content := "package handler\ntype IPlayerEntity interface { ID() int64 }\n//" + marker + "\nfunc handlerPing(p IPlayerEntity) {}\n"
		if err := os.WriteFile(source, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("roost:nest")
	if err := run([]string{"-dir", dir}, io.Discard); err != nil {
		t.Fatal(err)
	}
	paths := []string{
		filepath.Join(dir, "handler_nest_gen.go"),
		filepath.Join(dir, "sender", "handler_nest_gen.go"),
		filepath.Join(dir, "sender", "handler_nest_gen_test.go"),
		filepath.Join(dir, "syncsender", "handler_nest_gen.go"),
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("generated output missing %s: %v", path, err)
		}
	}
	manual := filepath.Join(dir, "manual_nest_gen.go")
	if err := os.WriteFile(manual, []byte("package handler\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	write("retired nest")
	if err := run([]string{"-dir", dir}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("retired output remains %s: %v", path, err)
		}
	}
	if _, err := os.Stat(manual); err != nil {
		t.Fatalf("manual file removed: %v", err)
	}
}

func TestRunWithoutSenderRetiresOnlySenderOutputs(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "handler.go")
	content := "package handler\ntype IPlayerEntity interface { ID() int64 }\n//roost:nest\nfunc handlerPing(p IPlayerEntity) {}\n"
	if err := os.WriteFile(source, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-dir", dir}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-dir", dir, "-sender=false"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "handler_nest_gen.go")); err != nil {
		t.Fatalf("wrapper was removed: %v", err)
	}
	for _, path := range []string{
		filepath.Join(dir, "sender", "handler_nest_gen.go"),
		filepath.Join(dir, "sender", "handler_nest_gen_test.go"),
		filepath.Join(dir, "syncsender", "handler_nest_gen.go"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("sender output remains %s: %v", path, err)
		}
	}
}
