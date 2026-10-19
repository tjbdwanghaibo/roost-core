// Package webroute contains the runtime support used by generated HTTP routes.
package webroute

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	"github.com/tjbdwanghaibo/roost-core/infra/network/httpserver"
)

// Registerer is implemented by a route registry that validates and installs
// generated HTTP handlers.
type Registerer interface {
	Register(method, path string, handler http.HandlerFunc) error
}

// Module registers a package-owned set of HTTP routes.
type Module interface {
	RegisterRoutes(Registerer) error
}

// RegisterModules installs modules in order and stops at the first error.
// A shared Registerer makes duplicate method/path pairs fail across packages.
func RegisterModules(reg Registerer, modules ...Module) error {
	for _, module := range modules {
		if module == nil {
			return errors.New("webroute: route module is required")
		}
		if err := module.RegisterRoutes(reg); err != nil {
			return err
		}
	}
	return nil
}

// Registrar installs routes through httpserver while rejecting duplicated
// method/path pairs before chi receives them.
type Registrar struct {
	routes httpserver.RouteRegistrar
	mu     sync.Mutex
	seen   map[string]struct{}
}

func NewRegistrar(routes httpserver.RouteRegistrar) *Registrar {
	return &Registrar{routes: routes, seen: make(map[string]struct{})}
}

// ValidatePath validates a chi route pattern without modifying the application
// router. Code generators and runtime registrars share chi's actual grammar.
func ValidatePath(path string) (err error) {
	if path == "" || !strings.HasPrefix(path, "/") {
		return fmt.Errorf("webroute: invalid route path %q", path)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("webroute: invalid route path %q: %v", path, recovered)
		}
	}()
	// chi 解析会 panic，只允许它修改一次性校验 router。
	chi.NewRouter().Get(path, func(http.ResponseWriter, *http.Request) {})
	return nil
}

func (r *Registrar) Register(method, path string, handler http.HandlerFunc) (err error) {
	if r == nil || r.routes == nil {
		return errors.New("webroute: route registrar is required")
	}
	if handler == nil {
		return errors.New("webroute: route handler is required")
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	path = strings.TrimSpace(path)
	if err := ValidatePath(path); err != nil {
		return err
	}
	if method != http.MethodGet && method != http.MethodPost {
		return fmt.Errorf("webroute: unsupported method %q", method)
	}

	key := method + " " + path
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.seen[key]; exists {
		return fmt.Errorf("webroute: duplicate route %s", key)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if cause, ok := recovered.(error); ok {
				err = fmt.Errorf("webroute: install route %s: %w", key, cause)
			} else {
				err = fmt.Errorf("webroute: install route %s: %v", key, recovered)
			}
		}
	}()

	switch method {
	case http.MethodGet:
		r.routes.Get(path, handler)
	case http.MethodPost:
		r.routes.Post(path, handler)
	}
	// 成功安装后才登记。自定义 installer 失败可能半安装；调用方应重建
	// 该 router，而不是把 error 当作对外部 router 的完整回滚。
	r.seen[key] = struct{}{}
	return nil
}

// RawRequest is a copy of the HTTP data made available to raw route handlers.
type RawRequest struct {
	Path       string
	Method     string
	RemoteAddr string
	Header     http.Header
	Body       []byte
}

func ReadRaw(w http.ResponseWriter, r *http.Request) (RawRequest, bool) {
	if r == nil {
		return RawRequest{}, false
	}
	body, ok := httpserver.ReadBody(w, r)
	if !ok {
		return RawRequest{}, false
	}
	return RawRequest{
		Path:       r.URL.Path,
		Method:     r.Method,
		RemoteAddr: r.RemoteAddr,
		Header:     r.Header.Clone(),
		Body:       append([]byte(nil), body...),
	}, true
}

// DecodeJSON reads one JSON document into target. Decode errors are returned
// to the caller as the standard HTTP 400 response.
func DecodeJSON[T any](w http.ResponseWriter, r *http.Request, target *T) bool {
	if target == nil {
		writeDecodeError(w)
		return false
	}
	body, ok := httpserver.ReadBody(w, r)
	if !ok {
		return false
	}
	if len(body) == 0 {
		return true
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if err := dec.Decode(target); err != nil {
		writeDecodeError(w)
		return false
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		writeDecodeError(w)
		return false
	}
	return true
}

// ErrorMapper 把路由处理返回的错误换成 HTTP 状态与响应体。业务要别的状态码或响应形状时传给 WriteResultWithMapper。
type ErrorMapper func(error) (status int, response any)

// DefaultErrorMapper 按框架错误码（errcode.ClientError）映射，与 RPC / 玩家协议给客户端的 (code, reason) 同一来源：
//
//   - 带客户端码的错误（errcode.Define 定义、Wrap 包装，或下游服务经 errcode.Remote 带回的码）：400 与
//     {"ok": false, "code": <码>, "reason": <定义里的 message>}。客户端按 code 分支；被包装的原因不进响应体。
//   - 其余错误（没有码，或码是 errcode.CodeInternal）：500 与 {"ok": false, "code": 1, "reason": "server error"}，
//     不把内部错误文本（库地址、驱动报错）交给客户端；原文由 WriteResultWithMapper 记进服务端日志。
//
// HTTP 状态只分“请求方可处理的业务拒绝（4xx）”与“服务端故障（5xx）”两类，具体原因看 code，所以所有业务码都是 400。
func DefaultErrorMapper(err error) (int, any) {
	code, reason := errcode.ClientError(err)
	status := http.StatusBadRequest
	if code == errcode.CodeInternal {
		status = http.StatusInternalServerError
	}
	return status, map[string]any{"ok": false, "code": code, "reason": reason}
}

func WriteResult(w http.ResponseWriter, successStatus int, response any, err error) {
	WriteResultWithMapper(w, successStatus, response, err, DefaultErrorMapper)
}

// WriteResultWithMapper 写成功响应，或按 mapper 写错误响应（mapper 为 nil 时用 DefaultErrorMapper）。映射成 5xx 的
// 错误把原文记一条 Error 日志（带请求 ID，web Mod 的 “web: request failed” 日志按同一 request_id 给出路由模板），
// 响应体里只有 mapper 给的内容。
func WriteResultWithMapper(w http.ResponseWriter, successStatus int, response any, err error, mapper ErrorMapper) {
	if err == nil {
		httpserver.JSON(w, successStatus, response)
		return
	}
	if mapper == nil {
		mapper = DefaultErrorMapper
	}
	status, errorResponse := mapper(err)
	if status >= http.StatusInternalServerError {
		slog.Error("webroute: route failed with a server error", "status", status,
			"request_id", w.Header().Get(httpserver.HeaderRequestID), "err", err)
	}
	httpserver.JSON(w, status, errorResponse)
}

func writeDecodeError(w http.ResponseWriter) {
	httpserver.JSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "decode request"})
}
