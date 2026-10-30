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

// GeneratedFileName 是每个路由包里生成的注册文件；roost add webroute 回滚时按它找文件。
const GeneratedFileName = "webroute_gen.go"

const (
	bodyJSON            = "json"
	bodyRaw             = "raw"
	compressGzip        = "gzip"
	generatedFileName   = GeneratedFileName
	generatedFileSuffix = "_webroute_gen.go"
)

// requiredMarkerKeys 是 //roost:web 必须写的选项；headers= / compress= 可选。
var requiredMarkerKeys = []string{"method", "path", "body"}

type Route struct {
	Handler      string
	Method       string
	Path         string
	BodyMode     string
	RequestType  string
	ResponseType string
	// Headers 来自 headers=Name:Field,...（只用于 body=json）：JSON 解码之后，生成代码按声明顺序把
	// r.Header.Get(Name) 赋给 request.Field。
	Headers []HeaderBinding
	// Compress 来自 compress=，目前只有 gzip：成功响应经 webroute.WriteCompressedResult 按 Accept-Encoding 协商压缩。
	Compress string
}

// HeaderBinding 是一条请求头到请求结构字段的绑定。Name 已按 http.CanonicalHeaderKey 规范化。
type HeaderBinding struct {
	Name  string
	Field string
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

// marker.Web.Parse refuses unknown keys; requiredMarkerKeys drives the
// missing-key check.
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
		// The error names the file and line of the marker.
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
		for _, required := range requiredMarkerKeys {
			if _, ok := options[required]; !ok {
				return nil, true, fmt.Errorf("missing marker option %q (want //roost:web method=GET|POST path=/… body=json|raw [headers=Name:Field,…] [compress=gzip])", required)
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
	headers, err := parseHeaderBindings(options["headers"], bodyMode)
	if err != nil {
		return Route{}, err
	}
	compress := strings.ToLower(options["compress"])
	if compress != "" && compress != compressGzip {
		return Route{}, fmt.Errorf("unsupported compress %q (only compress=gzip)", options["compress"])
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
		Headers:      headers,
		Compress:     compress,
	}, nil
}

// parseHeaderBindings 解析 headers=Name:Field,Name:Field（与下游 cube fork 的写法一致）。每项恰好一个冒号；Name 是合法
// HTTP 头名（RFC 9110 token），生成期规范化，大小写不同的同名头算重复；Field 是请求结构上的一个 Go 标识符（直接字段或
// 嵌入提升的字段），必须是 string——生成器不做类型检查，类型不对时生成文件的赋值行编译失败。只用于 body=json：raw
// 路由自己读 RawRequest.Header。
func parseHeaderBindings(value, bodyMode string) ([]HeaderBinding, error) {
	if value == "" {
		return nil, nil
	}
	if bodyMode != bodyJSON {
		return nil, fmt.Errorf("headers= needs body=%s (a body=raw handler reads request.Header itself)", bodyJSON)
	}
	items := strings.Split(value, ",")
	bindings := make([]HeaderBinding, 0, len(items))
	names := make(map[string]bool, len(items))
	fields := make(map[string]bool, len(items))
	for _, item := range items {
		name, field, found := strings.Cut(item, ":")
		if !found || strings.Contains(field, ":") {
			return nil, fmt.Errorf("invalid header binding %q; want Header-Name:Field", item)
		}
		if !validHeaderName(name) {
			return nil, fmt.Errorf("invalid header name %q in headers=", name)
		}
		if !token.IsIdentifier(field) {
			return nil, fmt.Errorf("invalid request field %q in headers=; want one Go field name", field)
		}
		name = http.CanonicalHeaderKey(name)
		if names[name] {
			return nil, fmt.Errorf("duplicate header %q in headers=", name)
		}
		if fields[field] {
			return nil, fmt.Errorf("duplicate request field %q in headers=", field)
		}
		names[name], fields[field] = true, true
		bindings = append(bindings, HeaderBinding{Name: name, Field: field})
	}
	return bindings, nil
}

// validHeaderName 报告 name 是 RFC 9110 的 token（HTTP 头名允许的字符）。
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0 {
			continue
		}
		return false
	}
	return true
}

// validateChiPath 用 chi 自己的解析器校验路由模式，与运行期 webroute.ValidatePath
// 同一语法。生成器不得 import core 运行时包
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
