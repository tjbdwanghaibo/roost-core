package webroute

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const webRouteSource = `package web

import (
    "context"
    "github.com/tjbdwanghaibo/roost-core/webroute"
)

type Service struct{}
type JSONRequest struct { Name string ` + "`json:\"name\"`" + ` }
type JSONResponse struct { Message string ` + "`json:\"message\"`" + ` }
type RawResponse struct { Code int ` + "`json:\"code\"`" + ` }

//roost:web method=POST path=/external/json body=json
func handleJSON(ctx context.Context, svc *Service, request JSONRequest) (JSONResponse, error) {
    return JSONResponse{}, nil
}

//roost:web method=POST path=/external/event body=raw
func handleRaw(ctx context.Context, svc *Service, request webroute.RawRequest) (RawResponse, error) {
    return RawResponse{}, nil
}
`

func TestParseJSONRoute(t *testing.T) {
	routes, pkg, err := ParseFile("handler.go", []byte(webRouteSource))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if pkg != "web" {
		t.Fatalf("package = %q", pkg)
	}
	if len(routes) != 2 {
		t.Fatalf("routes = %d", len(routes))
	}
	if routes[0].Method != "POST" || routes[0].Path != "/external/json" || routes[0].BodyMode != bodyJSON {
		t.Fatalf("route = %+v", routes[0])
	}
}

func TestParseRejectsInvalidHandler(t *testing.T) {
	_, _, err := ParseFile("handler.go", []byte(`package web
import "context"
type Service struct{}
//roost:web method=POST path=/x body=json
func bad(ctx context.Context, svc *Service) error { return nil }
`))
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("error = %v", err)
	}
}

func TestGenerateRoutes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "handler.go")
	if err := os.WriteFile(path, []byte(webRouteSource), 0644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	changed, err := GenerateDir(dir, false)
	if err != nil {
		t.Fatalf("GenerateDir: %v", err)
	}
	if len(changed) != 1 {
		t.Fatalf("changed = %v", changed)
	}
	content, err := os.ReadFile(filepath.Join(dir, generatedFileName))
	if err != nil {
		t.Fatalf("read generated file: %v", err)
	}
	source := string(content)
	for _, want := range []string{
		"func RegisterRoutes(reg webroute.Registerer, svc *Service) error",
		"func (svc *Service) RegisterRoutes(reg webroute.Registerer) error",
		"webroute.DecodeJSON(w, r, &request)",
		"webroute.ReadRaw(w, r)",
		`reg.Register("POST", "/external/json"`,
		`reg.Register("POST", "/external/event"`,
	} {
		if !strings.Contains(source, want) {
			t.Errorf("generated source missing %q", want)
		}
	}
}

func TestGenerateRejectsDuplicateRoute(t *testing.T) {
	dir := t.TempDir()
	source := strings.Replace(webRouteSource, "/external/event", "/external/json", 1)
	if err := os.WriteFile(filepath.Join(dir, "handler.go"), []byte(source), 0644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	_, err := GenerateDir(dir, false)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error = %v", err)
	}
}
