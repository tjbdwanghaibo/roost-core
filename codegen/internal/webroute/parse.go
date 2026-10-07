package webroute

import (
	"fmt"
	"github.com/go-chi/chi/v5"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/marker"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"net/http"
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
		options, found, err := parseMarker(fset, function.Doc)
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

// Every option of marker.Web is required, so its key list drives both the
// unknown-key and the missing-key checks.
func parseMarker(fset *token.FileSet, group *ast.CommentGroup) (map[string]string, bool, error) {
	if group == nil {
		return nil, false, nil
	}
	for _, comment := range group.List {
		text := strings.TrimSpace(comment.Text)
		body, isMarker := marker.Cut(text, "web")
		if !isMarker {
			continue
		}
		// RR-20261006-56: the error names the file and line of the marker.
		options, err := marker.Web.Parse(body)
		if err != nil {
			position := fset.Position(comment.Pos())
			return nil, true, fmt.Errorf("%s:%d: %w", position.Filename, position.Line, err)
		}
		for key, value := range options {
			if value == "" {
				return nil, true, fmt.Errorf("invalid marker option %q", key+"=")
			}
		}
		for _, required := range marker.Web.Keys {
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
	// 与正式运行期共用 chi 的路由语法，在扫描阶段拒绝，尚未写任何生成物。
	if err := validateChiPath(path); err != nil {
		return Route{}, err
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

// validateChiPath 用 chi 自己的解析器校验路由模式，与运行期 webroute.ValidatePath
// （RR-20261004-NC-07）同一语法。生成器不得 import core 运行时包
// （dependency_boundary_test.go 的 codegen 层规则），所以这里直接调用 chi，
// 而不是引用 roost-core/webroute。chi 解析非法模式会 panic，只让它改一次性 router。
func validateChiPath(path string) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("invalid path %q: %v", path, recovered)
		}
	}()
	chi.NewRouter().Get(path, func(http.ResponseWriter, *http.Request) {})
	return nil
}
