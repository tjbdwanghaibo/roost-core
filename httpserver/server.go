package httpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	HeaderRequestID        = "X-Request-ID"
	DefaultMaxJSONBody     = int64(1 << 20)
	defaultRequestIDPrefix = "req"
)

type contextKey string

const (
	requestIDContextKey contextKey = "httpserver.request_id"
	maxBodyContextKey   contextKey = "httpserver.max_body"
)

type Config struct {
	MaxBodyBytes      int64
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
}

type Option func(*Config)

func WithMaxBodyBytes(n int64) Option {
	return func(cfg *Config) {
		if n > 0 {
			cfg.MaxBodyBytes = n
		}
	}
}

func WithTimeouts(readHeader, read, write, idle time.Duration) Option {
	return func(cfg *Config) {
		if readHeader > 0 {
			cfg.ReadHeaderTimeout = readHeader
		}
		if read > 0 {
			cfg.ReadTimeout = read
		}
		if write > 0 {
			cfg.WriteTimeout = write
		}
		if idle > 0 {
			cfg.IdleTimeout = idle
		}
	}
}

func defaultConfig() Config {
	return Config{
		MaxBodyBytes:      DefaultMaxJSONBody,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

type Engine struct {
	router chi.Router
	cfg    Config
}

// RouteRegistrar is the minimal HTTP route contract used by higher-level
// registration packages. Engine and Group both implement it.
type RouteRegistrar interface {
	Get(path string, handler http.HandlerFunc)
	Post(path string, handler http.HandlerFunc)
}

func NewEngine(opts ...Option) *Engine {
	cfg := defaultConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	r := chi.NewRouter()
	e := &Engine{router: r, cfg: cfg}
	e.Use(e.requestContextMiddleware, e.recoverMiddleware)
	return e
}

func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if e == nil || e.router == nil {
		http.NotFound(w, r)
		return
	}
	e.router.ServeHTTP(w, r)
}

func (e *Engine) Handler() http.Handler {
	if e == nil {
		return http.NewServeMux()
	}
	return e
}

func (e *Engine) Use(middlewares ...func(http.Handler) http.Handler) {
	if e == nil || e.router == nil {
		return
	}
	e.router.Use(middlewares...)
}

func (e *Engine) Group(prefix string) *Group {
	if e == nil || e.router == nil {
		return &Group{}
	}
	return &Group{router: chi.NewRouter(), mount: func(path string, handler http.Handler) {
		e.router.Mount(path, handler)
	}, prefix: normalizePath(prefix)}
}

func (e *Engine) Get(path string, handler http.HandlerFunc) {
	if e == nil || e.router == nil {
		return
	}
	e.router.Get(path, handler)
}

func (e *Engine) Post(path string, handler http.HandlerFunc) {
	if e == nil || e.router == nil {
		return
	}
	e.router.Post(path, handler)
}

type Group struct {
	router chi.Router
	mount  func(string, http.Handler)
	prefix string
}

func (g *Group) Use(middlewares ...func(http.Handler) http.Handler) {
	if g == nil || g.router == nil {
		return
	}
	g.router.Use(middlewares...)
}

func (g *Group) Get(path string, handler http.HandlerFunc) {
	if g == nil || g.router == nil {
		return
	}
	g.ensureMounted()
	g.router.Get(path, handler)
}

func (g *Group) Post(path string, handler http.HandlerFunc) {
	if g == nil || g.router == nil {
		return
	}
	g.ensureMounted()
	g.router.Post(path, handler)
}

func (g *Group) ensureMounted() {
	if g == nil || g.mount == nil {
		return
	}
	g.mount(g.prefix, g.router)
	g.mount = nil
}

func NewServer(addr string, handler http.Handler, opts ...Option) *http.Server {
	cfg := defaultConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if handler == nil {
		handler = NewEngine(opts...)
	}
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}
}

func RequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(requestIDContextKey).(string)
	return v
}

// JSON 先完整编码，再写状态码和响应体。编码失败（NaN/Inf、不可编码类型、
// MarshalJSON 返回错误）时改回 500 固定错误体并记录日志：状态码一旦发出就不能
// 再改，先写 2xx 再编码会让调用方把空体当成功（RR-20261005-NC-80）。
// MarshalJSON panic 发生在写头之前，交给 Engine 的 recover 中间件回 500。
//
// json.Encoder 在池化缓冲里完整编码、成功后才一次性 Write（标准实现与 jsonv2
// 实现都如此），所以把状态码推迟到第一次 Write 就得到“先编码后写”，不再为每个
// 响应另复制一份响应体；成功时的字节就是 json.Encoder 的输出（HTML 转义、结尾换行）。
// 已经写出后才返回的错误只可能是连接写失败，此时客户端已不可达，与原先一样忽略。
func JSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	out := &statusOnFirstWrite{ResponseWriter: w, status: status}
	err := json.NewEncoder(out).Encode(value)
	if err == nil || out.wrote {
		return
	}
	slog.Error("http server: encode response", "status", status, "err", err)
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = io.WriteString(w, "{\"error\":\"encode response\",\"ok\":false}\n")
}

// statusOnFirstWrite 把 WriteHeader 推迟到第一次 Write，只给 JSON 使用。
type statusOnFirstWrite struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusOnFirstWrite) Write(p []byte) (int, error) {
	if !w.wrote {
		w.wrote = true
		w.ResponseWriter.WriteHeader(w.status)
	}
	return w.ResponseWriter.Write(p)
}

func HandleJSON[TReq any, TResp any](fn func(context.Context, TReq) (TResp, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, ok := BindJSON[TReq](w, r)
		if !ok {
			return
		}
		resp, err := fn(r.Context(), req)
		if err != nil {
			JSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		JSON(w, http.StatusOK, resp)
	}
}

func BindJSON[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var out T
	raw, ok := ReadBody(w, r)
	if !ok || len(raw) == 0 {
		return out, ok
	}
	// RR-20261003-NC-04：已读完整 body，必须整段为单个 JSON 值才能进入业务函数。
	if err := json.Unmarshal(raw, &out); err != nil {
		JSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "decode request"})
		return out, false
	}
	return out, true
}

func ReadBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r == nil || r.Body == nil {
		return nil, true
	}
	defer r.Body.Close()
	reader := http.MaxBytesReader(w, r.Body, maxBodyBytes(r.Context()))
	raw, err := io.ReadAll(reader)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			JSON(w, http.StatusRequestEntityTooLarge, map[string]any{"ok": false, "error": "request body too large"})
			return nil, false
		}
		JSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "read request"})
		return nil, false
	}
	return raw, true
}

func (e *Engine) requestContextMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := strings.TrimSpace(r.Header.Get(HeaderRequestID))
		if requestID == "" {
			requestID = fmt.Sprintf("%s-%d", defaultRequestIDPrefix, time.Now().UnixNano())
		}
		w.Header().Set(HeaderRequestID, requestID)
		ctx := context.WithValue(r.Context(), requestIDContextKey, requestID)
		ctx = context.WithValue(ctx, maxBodyContextKey, e.cfg.MaxBodyBytes)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// recoverMiddleware 把响应开始之前的 handler panic 转成 500 JSON。
//
// 响应已经开始（写过状态码/响应体、Flush 或 Hijack）后状态无法更改，再追加错误体
// 会让客户端收到一个“完整”的 2xx 拼接体；这时记录日志后以 http.ErrAbortHandler
// 重新 panic，由 net/http 中止连接，客户端看到传输失败。handler 自己用
// http.ErrAbortHandler 中止时保持标准库语义，原样传播（RR-20261005-NC-81）。
func (e *Engine) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tracked, state := trackResponse(w)
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			if recovered == http.ErrAbortHandler {
				panic(recovered)
			}
			slog.Error("http server panic",
				"request_id", RequestID(r.Context()),
				"path", r.URL.Path,
				"err", recovered,
				"response_started", state.started,
				"stack", string(debug.Stack()),
			)
			if state.started {
				panic(http.ErrAbortHandler)
			}
			JSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "internal server error"})
		}()
		next.ServeHTTP(tracked, r)
	})
}

// responseState 记录响应是否已经开始，只给 recoverMiddleware 判断能否改写为 500。
// 它保留 Flusher（含 FlushError）、io.ReaderFrom 和 Unwrap（http.ResponseController
// 经此取得原 writer）；仅当原 writer 支持时才暴露 Hijacker，类型断言结果与原 writer
// 一致。标准库没有“响应是否已开始”的查询（ResponseController 只提供 Flush/Hijack/
// 期限/全双工），所以只能由包装 writer 记录。http.Pusher 不透传：主流浏览器已移除
// HTTP/2 推送，Go 自带客户端也以 SETTINGS_ENABLE_PUSH=0 关闭，Engine 是 JSON 接口。
type responseState struct {
	http.ResponseWriter
	started bool
}

type hijackableResponseState struct{ *responseState }

func trackResponse(w http.ResponseWriter) (http.ResponseWriter, *responseState) {
	state := &responseState{ResponseWriter: w}
	if _, ok := w.(http.Hijacker); ok {
		return hijackableResponseState{state}, state
	}
	return state, state
}

func (w *responseState) WriteHeader(code int) {
	// 1xx 信息性响应（101 除外）不提交最终状态。
	if code < 100 || code > 199 || code == http.StatusSwitchingProtocols {
		w.started = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseState) Write(p []byte) (int, error) {
	w.started = true
	return w.ResponseWriter.Write(p)
}

func (w *responseState) ReadFrom(src io.Reader) (int64, error) {
	w.started = true
	return io.Copy(w.ResponseWriter, src)
}

// FlushError 让 http.ResponseController.Flush 拿到原 writer 的结果：不支持 Flush
// 时返回 http.ErrNotSupported 且不算响应已开始，连接写失败原样返回。
func (w *responseState) FlushError() error {
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if !errors.Is(err, http.ErrNotSupported) {
		w.started = true
	}
	return err
}

func (w *responseState) Flush() { _ = w.FlushError() }

func (w *responseState) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w hijackableResponseState) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffered, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err == nil {
		w.started = true
	}
	return conn, buffered, err
}

func maxBodyBytes(ctx context.Context) int64 {
	if ctx != nil {
		if v, ok := ctx.Value(maxBodyContextKey).(int64); ok && v > 0 {
			return v
		}
	}
	return DefaultMaxJSONBody
}

func normalizePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return path
}
