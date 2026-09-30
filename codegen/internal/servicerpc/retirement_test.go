package servicerpc

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRetiresTransportWhenInterfaceMarkerIsRemoved(t *testing.T) {
	dir := writeDir(t, goodService)
	if err := Run([]string{"-dir", dir}, io.Discard); err != nil {
		t.Fatal(err)
	}
	retired := strings.Replace(goodService, "//roost:rpc service_type=mail", "// retired rpc service_type=mail", 1)
	if err := os.WriteFile(filepath.Join(dir, "svc.go"), []byte(retired), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"-dir", dir, "-check"}, io.Discard); err == nil || !strings.Contains(err.Error(), "mail_rpc_gen.go") {
		t.Fatalf("-check accepted retired transport: %v", err)
	}
	for _, name := range []string{"mail_rpc_gen.go", "mail_rpc_assembly_gen.go"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("-check changed %s: %v", name, err)
		}
	}
	if err := Run([]string{"-dir", dir}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mail_rpc_gen.go", "mail_rpc_assembly_gen.go"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("retired %s remains: %v", name, err)
		}
	}
}

func TestRunRetiresOnlyItsAssemblyHalf(t *testing.T) {
	source := writeDir(t, goldenService)
	other := writeDir(t, strings.Replace(goldenService, "type Shop interface", "type Shelf interface", 1))
	out := t.TempDir()
	if err := Run([]string{"-dir", source, "-emit", "transport"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{source, other} {
		if err := Run([]string{"-dir", dir, "-emit", "assembly", "-out", out}, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	manual := filepath.Join(out, "manual_rpc_assembly_gen.go")
	if err := os.WriteFile(manual, []byte("package shop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	retired := strings.Replace(goldenService, "//roost:rpc service_type=shop", "// retired rpc service_type=shop", 1)
	if err := os.WriteFile(filepath.Join(source, "svc.go"), []byte(retired), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"-dir", source, "-emit", "assembly", "-out", out}
	if err := Run(append(append([]string(nil), args...), "-check"), io.Discard); err == nil || !strings.Contains(err.Error(), "shop_rpc_assembly_gen.go") {
		t.Fatalf("assembly -check accepted orphan: %v", err)
	}
	if err := Run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "shop_rpc_assembly_gen.go")); !os.IsNotExist(err) {
		t.Fatalf("retired assembly remains: %v", err)
	}
	for _, path := range []string{filepath.Join(source, "shop_rpc_gen.go"), filepath.Join(out, "shelf_rpc_assembly_gen.go"), manual} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("unrelated output %s was removed: %v", path, err)
		}
	}
}
