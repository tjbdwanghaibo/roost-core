package webroute

import (
	"fmt"
	"github.com/tjbdwanghaibo/roost-codegen/internal/marker"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"net/http"
	"slices"
	"strings"
)

const (
	bodyJSON            = "json"
	bodyRaw             = "raw"
	generatedFileName   = "webroute_gen.go"
	generatedFileSuffix = "_webroute_gen.go"
)

type Route struct {
	Handler      string
	Method       string
	Path         string
	BodyMode     string
	RequestType  string
	ResponseType string
}

// ParseFile extracts and validates all annotated routes in source.
func ParseFile(path string, source []byte) ([]Route, string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, source, parser.ParseComments)
	if err != nil {
		return nil, "", err
	}

	routes := make([]Route, 0)
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Recv != nil {
			continue
		}
		options, found, err := parseMarker(function.Doc)
		if err != nil {
			return nil, "", fmt.Errorf("%s: %w", function.Name.Name, err)
		}
		if !found {
			continue
		}
		route, err := parseRoute(function, options)
		if err != nil {
			return nil, "", fmt.Errorf("%s: %w", function.Name.Name, err)
		}
		routes = append(routes, route)
	}
	return routes, file.Name.Name, nil
}

// markerOptions is every option //roost:web understands; all of them are
// required, so the same list drives both the unknown-key and the missing-key
// checks.
var markerOptions = []string{"method", "path", "body"}

func parseMarker(group *ast.CommentGroup) (map[string]string, bool, error) {
	if group == nil {
		return nil, false, nil
	}
	for _, comment := range group.List {
		text := strings.TrimSpace(comment.Text)
		body, isMarker := marker.Cut(text, "web")
		if !isMarker {
			continue
		}
		fields := strings.Fields(strings.TrimSpace(body))
		options := make(map[string]string, len(fields))
		for _, field := range fields {
			parts := strings.SplitN(field, "=", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return nil, true, fmt.Errorf("invalid marker option %q", field)
			}
			if _, exists := options[parts[0]]; exists {
				return nil, true, fmt.Errorf("duplicate marker option %q", parts[0])
			}
			if !slices.Contains(markerOptions, parts[0]) {
				// A misspelt key used to be accepted here and surface later as
				// `unsupported method ""`, pointing away from the typo.
				return nil, true, fmt.Errorf("unknown marker option %q (known: %s)", parts[0], strings.Join(markerOptions, ", "))
			}
			options[parts[0]] = parts[1]
		}
		for _, required := range markerOptions {
			if _, ok := options[required]; !ok {
				return nil, true, fmt.Errorf("missing marker option %q (want //roost:web method=GET|POST path=/… body=json|raw)", required)
			}
		}
		return options, true, nil
	}
	return nil, false, nil
}

func parseRoute(function *ast.FuncDecl, options map[string]string) (Route, error) {
	method := strings.ToUpper(options["method"])
	path := options["path"]
	bodyMode := strings.ToLower(options["body"])
	if method != http.MethodGet && method != http.MethodPost {
		return Route{}, fmt.Errorf("unsupported method %q", method)
	}
	if path == "" || !strings.HasPrefix(path, "/") {
		return Route{}, fmt.Errorf("invalid path %q", path)
	}
	if bodyMode != bodyJSON && bodyMode != bodyRaw {
		return Route{}, fmt.Errorf("unsupported body mode %q", bodyMode)
	}
	if method == http.MethodGet && bodyMode == bodyJSON {
		return Route{}, fmt.Errorf("GET routes must use body=%s", bodyRaw)
	}
	if function.Type.Params == nil || len(function.Type.Params.List) != 3 || function.Type.Results == nil || len(function.Type.Results.List) != 2 {
		return Route{}, fmt.Errorf("invalid signature; want func(context.Context, *Service, Request) (Response, error)")
	}
	params := function.Type.Params.List
	if types.ExprString(params[0].Type) != "context.Context" || types.ExprString(params[1].Type) != "*Service" {
		return Route{}, fmt.Errorf("invalid signature; want func(context.Context, *Service, Request) (Response, error)")
	}
	requestType := types.ExprString(params[2].Type)
	if requestType == "" || types.ExprString(function.Type.Results.List[1].Type) != "error" {
		return Route{}, fmt.Errorf("invalid signature; want func(context.Context, *Service, Request) (Response, error)")
	}
	if bodyMode == bodyRaw && requestType != "webroute.RawRequest" {
		return Route{}, fmt.Errorf("body=raw request must be webroute.RawRequest, got %s", requestType)
	}
	return Route{
		Handler:      function.Name.Name,
		Method:       method,
		Path:         path,
		BodyMode:     bodyMode,
		RequestType:  requestType,
		ResponseType: types.ExprString(function.Type.Results.List[0].Type),
	}, nil
}
