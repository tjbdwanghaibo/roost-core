package registry

import (
	"strings"
	"testing"
)

// A file the generator cannot parse is reported, not skipped: skipping would
// drop the registrations in it and the aggregate would silently register
// less than the project declares. Silencing the error left every test green
// (roost-codegen U-0030).
func TestScanReportsAnUnparsableFileInsteadOfDroppingItsRegistrations(t *testing.T) {
	root := writeProject(t, map[string]string{
		"game/ok/ok.go":         "package ok\n\n//roost:register phase=entity\nfunc RegisterOK() {}\n",
		"game/broken/broken.go": "package broken\n\n//roost:register phase=entity\nfunc RegisterBroken( {\n",
	})
	_, err := Scan(root, "example.com/planet")
	if err == nil {
		t.Fatal("Scan accepted a project with an unparsable file")
	}
	if !strings.Contains(err.Error(), "parse") || !strings.Contains(err.Error(), "broken.go") {
		t.Fatalf("error %q does not name the unparsable file", err)
	}
}

// A registration that returns an error is called with its error checked and
// wrapped into RegisterAll's result; one that returns nothing is a plain
// call. Dropping the check made a failing registration invisible while the
// generated code still compiled (roost-codegen U-0030).
func TestRenderChecksTheErrorOfErrorReturningRegistrations(t *testing.T) {
	content, err := render("example.com/p", []Registration{
		{ImportPath: "example.com/p/game/a", Func: "RegisterA", Phase: "entity", ReturnsError: true},
		{ImportPath: "example.com/p/game/b", Func: "RegisterB", Phase: "entity", ReturnsError: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	src := string(content)
	if !strings.Contains(src, "if err := a.RegisterA(); err != nil {") || !strings.Contains(src, `fmt.Errorf("registry: example.com/p/game/a.RegisterA: %w", err)`) {
		t.Fatalf("error-returning registration is not checked and wrapped:\n%s", src)
	}
	if !strings.Contains(src, "\n\tb.RegisterB()\n") {
		t.Fatalf("plain registration is not a plain call:\n%s", src)
	}
}
