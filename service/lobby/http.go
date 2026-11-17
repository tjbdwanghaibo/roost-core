package lobby

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/infra/network/httpserver"
)

// MaxRequestBytes 是请求体上限。最大的合法请求是带 1KB 属性的建房，16KiB 留足余量。
const MaxRequestBytes = 16 << 10

// Handler 返回大厅的 HTTP 接口（全部 JSON；除 OPTIONS 预检外都要 `Authorization: Bearer gt1...`）：
//
//	POST /rooms               建房（CreateRequest）               → Ticket
//	GET  /rooms?game=&mode=   列房（可选 limit，缺省 50、上限 100）→ {"rooms": [RoomView]}
//	POST /rooms/{id}/join     加入（JoinRequest）                  → Ticket
//	POST /rooms/{id}/leave    离开（LeaveRequest）                 → Ticket（只有 room）
//	POST /rooms/{id}/state    房主改状态 / 属性（StateRequest）    → Ticket（只有 room）
//
// 错误答 {"error": <错误名>, "message": <说明>}，状态码见 errorStatus；503 的说明不带内部细节（细节进日志）。
//
// corsOrigins 是允许跨域调用的游戏页面 Origin（§4.7.1），例如 https://play.example.com；"*" 表示任何 Origin
// （凭证是 Authorization 头而不是 cookie，放开不会让别的站点借用用户的登录）。不在列表里的 Origin 拿不到 CORS 头，
// 预检答 403。
func (s *Service) Handler(corsOrigins []string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /rooms", func(w http.ResponseWriter, r *http.Request) {
		var req CreateRequest
		if decode(w, r, &req) {
			reply(w, r, func() (any, error) { return s.CreateRoom(r.Context(), bearer(r), req) })
		}
	})
	mux.HandleFunc("GET /rooms", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		limit := 0
		if raw := query.Get("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 0 {
				fail(w, r, fmt.Errorf("%w: limit must be a non-negative integer", ErrBadRequest))
				return
			}
			limit = parsed
		}
		reply(w, r, func() (any, error) {
			rooms, err := s.ListRooms(r.Context(), bearer(r), query.Get("game"), query.Get("mode"), limit)
			return map[string][]RoomView{"rooms": rooms}, err
		})
	})
	mux.HandleFunc("POST /rooms/{id}/join", func(w http.ResponseWriter, r *http.Request) {
		var req JoinRequest
		if decode(w, r, &req) {
			reply(w, r, func() (any, error) { return s.JoinRoom(r.Context(), bearer(r), r.PathValue("id"), req) })
		}
	})
	mux.HandleFunc("POST /rooms/{id}/leave", func(w http.ResponseWriter, r *http.Request) {
		var req LeaveRequest
		if decode(w, r, &req) {
			reply(w, r, func() (any, error) { return s.LeaveRoom(r.Context(), bearer(r), r.PathValue("id"), req) })
		}
	})
	mux.HandleFunc("POST /rooms/{id}/state", func(w http.ResponseWriter, r *http.Request) {
		var req StateRequest
		if decode(w, r, &req) {
			reply(w, r, func() (any, error) { return s.SetRoomState(r.Context(), bearer(r), r.PathValue("id"), req) })
		}
	})
	return cors(corsOrigins, mux)
}

// cors 给允许的 Origin 加 CORS 头，并答预检。
func cors(origins []string, next http.Handler) http.Handler {
	anyOrigin := slices.Contains(origins, "*")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := origin != "" && (anyOrigin || slices.Contains(origins, origin))
		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		httpserver.AddVary(w.Header(), "Origin")
		if r.Method == http.MethodOptions {
			if !allowed {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearer 取 Authorization: Bearer 后面的 token；没有时为空（由 authenticate 拒绝）。
func bearer(r *http.Request) string {
	scheme, value, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(value)
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpserver.JSON(w, http.StatusRequestEntityTooLarge, errorBody{Error: "too_large", Message: "request body is too large"})
			return false
		}
		fail(w, r, fmt.Errorf("%w: request body is not valid JSON: %v", ErrBadRequest, err))
		return false
	}
	return true
}

type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func reply(w http.ResponseWriter, r *http.Request, call func() (any, error)) {
	result, err := call()
	if err != nil {
		fail(w, r, err)
		return
	}
	httpserver.JSON(w, http.StatusOK, result)
}

func fail(w http.ResponseWriter, r *http.Request, err error) {
	status, name := errorStatus(err)
	message := err.Error()
	if status >= 500 {
		slog.Warn("lobby: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		message = "the lobby cannot serve this request right now"
	}
	httpserver.JSON(w, status, errorBody{Error: name, Message: message})
}
