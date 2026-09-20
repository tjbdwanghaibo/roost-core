package webroute

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The parser refuses a dozen shapes of bad marker; two of them had a test.
// Each refusal is pinned by its message so that neighbouring checks cannot
// mask each other, and two gaps are closed: an option the marker does not
// know (`methd=POST`) was accepted and then reported as a missing method,
// and a missing required option was reported as `unsupported method ""`.
func TestParseFileRefusesEachBadMarkerByMessage(t *testing.T) {
	valid := "//roost:web method=POST path=/external/json body=json"
	cases := []struct{ name, marker, want string }{
		{"bare option", "//roost:web method path=/x body=json", `invalid marker option "method"`},
		{"empty value", "//roost:web method= path=/x body=json", `invalid marker option "method="`},
		{"duplicate option", "//roost:web method=POST method=GET path=/x body=raw", `duplicate marker option "method"`},
		{"unknown option", "//roost:web methd=POST path=/x body=json", `unknown marker option "methd"`},
		{"missing method", "//roost:web path=/x body=json", `missing marker option "method"`},
		{"missing path", "//roost:web method=POST body=json", `missing marker option "path"`},
		{"missing body", "//roost:web method=POST path=/x", `missing marker option "body"`},
		{"unsupported method", "//roost:web method=PUT path=/x body=json", `unsupported method "PUT"`},
		{"relative path", "//roost:web method=POST path=x body=json", `invalid path "x"`},
		{"unsupported body", "//roost:web method=POST path=/x body=xml", `unsupported body mode "xml"`},
		{"GET with json body", "//roost:web method=GET path=/x body=json", "GET routes must use body=raw"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := strings.Replace(webRouteSource, valid, tc.marker, 1)
			if source == webRouteSource {
				t.Fatalf("fixture marker not found")
			}
			_, _, err := ParseFile("handler.go", []byte(source))
			if err == nil {
				t.Fatalf("%s accepted", tc.marker)
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "handleJSON") {
				t.Fatalf("error must contain %q and the handler name; got %v", tc.want, err)
			}
		})
	}
}

func TestParseFileRefusesRawRouteWithTypedRequest(t *testing.T) {
	source := strings.Replace(webRouteSource, "request webroute.RawRequest", "request JSONRequest", 1)
	_, _, err := ParseFile("handler.go", []byte(source))
	if err == nil || !strings.Contains(err.Error(), "body=raw request must be webroute.RawRequest") || !strings.Contains(err.Error(), "handleRaw") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseFileRefusesEachSignatureDefectSeparately(t *testing.T) {
	cases := []struct{ name, from, to string }{
		{"two params", "ctx context.Context, svc *Service, request JSONRequest", "ctx context.Context, request JSONRequest"},
		{"wrong service type", "svc *Service", "svc Service"},
		{"no error result", "(JSONResponse, error)", "JSONResponse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := strings.Replace(webRouteSource, tc.from, tc.to, 1)
			if source == webRouteSource {
				t.Fatal("fixture text not found")
			}
			_, _, err := ParseFile("handler.go", []byte(source))
			if err == nil || !strings.Contains(err.Error(), "invalid signature") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

// Two files in one directory that declare different packages cannot share a
// generated file; the generator must say which directory, not write a file
// for the first package it happened to walk.
func TestGenerateDirRefusesMixedPackagesInOneDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(webRouteSource), 0o644); err != nil {
		t.Fatal(err)
	}
	other := strings.Replace(strings.Replace(webRouteSource, "package web", "package other", 1), "/external/", "/other/", -1)
	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte(other), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := GenerateDir(dir, false)
	if err == nil || !strings.Contains(err.Error(), "mixed packages") || !strings.Contains(err.Error(), dir) {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, generatedFileName)); statErr == nil {
		t.Fatal("no routes file may be written for a directory with mixed packages")
	}
}
