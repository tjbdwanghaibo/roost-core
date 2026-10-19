package web

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/network/httpserver"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// 路由级指标（docs/framework/OBSERVABILITY.md“业务 HTTP 路由”）。标签只取有界的值：
//
//   - route：chi 匹配到的路由模板（例如 /items/{id}），不是原始路径；没有匹配到路由的请求（404 / 405）记 _unmatched；
//   - method：标准方法原样，其余记 _other（没匹配到路由的请求可以带任意方法名）；
//   - class：状态码类别 1xx～5xx。
const (
	metricRequests        = "web.request.total"
	metricRequestDuration = "web.request.duration"

	unmatchedRoute = "_unmatched"
	otherMethod    = "_other"
)

// observeRoutes 是 Engine 上的中间件（在 httpserver 的 panic 恢复之内）：请求结束后按路由模板计数、记延迟；
// 5xx 另记一条带路由模板与请求 ID 的 Warn，和 webroute 记录内部错误文本的那条日志按 request_id 对上。
// panic 时响应由外层的恢复中间件写 500，这里在 defer 里按 500 计，不 recover、不改变 panic 的传播。
func observeRoutes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}
		finished := false
		defer func() {
			status := recorder.status
			switch {
			case status == 0 && finished:
				status = http.StatusOK // 什么也没写就返回：net/http 回 200
			case status == 0:
				status = http.StatusInternalServerError // panic 且还没写响应：外层恢复中间件回 500
			}
			route := httpserver.RoutePattern(r)
			if route == "" {
				route = unmatchedRoute
			}
			method := boundedMethod(r.Method)
			elapsed := time.Since(started)
			metrics.IncCounter(metricRequests, metrics.Labels{"route": route, "method": method, "class": statusClass(status)}, 1)
			metrics.ObserveHistogram(metricRequestDuration, metrics.Labels{"route": route, "method": method}, elapsed)
			if status >= http.StatusInternalServerError {
				slog.Warn("web: request failed", "route", route, "method", method, "status", status,
					"request_id", httpserver.RequestID(r.Context()), "duration", elapsed)
			}
		}()
		next.ServeHTTP(recorder, r)
		finished = true
	})
}

func boundedMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return method
	}
	return otherMethod
}

func statusClass(status int) string {
	switch {
	case status < 200:
		return "1xx"
	case status < 300:
		return "2xx"
	case status < 400:
		return "3xx"
	case status < 500:
		return "4xx"
	}
	return "5xx"
}

// statusRecorder 记下发出的最终状态码（0 表示还没写）。Unwrap 让 http.ResponseController 的 Flush / Hijack /
// 期限设置透传到原 writer；生成的路由只经 webroute 写 JSON，不直接做类型断言。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.status == 0 && code >= 200 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }
