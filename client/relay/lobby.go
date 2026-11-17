package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// lobby 的 HTTP 接口（契约见 docs/framework/LOBBY.md §2）。全部是 JSON，每个请求带 Authorization: Bearer gt1...；
// 请求里的 game 必须等于 gt1 里的游戏。

// Transport 是向 lobby 要的 relay 接入方式：tcp 时 relay_addr 是 host:port，ws 时是完整的 ws:// / wss:// URL。
// Go SDK 只连 TCP；ws 留给浏览器（TS SDK）。
type Transport string

const (
	TransportTCP Transport = "tcp"
	TransportWS  Transport = "ws"
)

// RoomState 是房主设置的房间状态。
type RoomState string

const (
	StateOpen    RoomState = "open"    // 可加入，出现在列表里
	StateStarted RoomState = "started" // 已开始：不在列表里、新人不能加入，已在房间里的人照常可以重连
)

// Room 是 lobby 返回的房间（LOBBY.md §2 的 room）。
type Room struct {
	ID          string    `json:"id"`
	Game        string    `json:"game"`
	Mode        string    `json:"mode"`
	Name        string    `json:"name"`
	MaxMembers  int       `json:"max_members"` // 含房主
	Members     int       `json:"members"`     // 占着席位的人数（在线、预留期内与房主）
	HasPassword bool      `json:"has_password"`
	State       RoomState `json:"state"`
	Host        string    `json:"host"`
	HostEpoch   uint64    `json:"host_epoch"`
	// Attrs 是建房时写的自定义属性，lobby 原样存、原样给，不解析。
	Attrs         json.RawMessage `json:"attrs,omitempty"`
	CreatedAtUnix int64           `json:"created_at_unix"`
}

// Ticket 是建房、加入的答复：房间、relay 地址与 relay 令牌。令牌寿命很短（缺省 30s），只用来建立连接。
type Ticket struct {
	Room      Room   `json:"room"`
	RelayAddr string `json:"relay_addr,omitempty"`
	Token     string `json:"token,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"` // relay 令牌的过期时刻（Unix 秒）
}

// CreateRoomRequest 是 POST /rooms 的请求，调用者成为房主。
type CreateRoomRequest struct {
	Game       string          `json:"game"`
	Mode       string          `json:"mode"`
	MaxMembers int             `json:"max_members,omitempty"` // 含房主；0 取 lobby 对这个游戏的上限
	Name       string          `json:"name,omitempty"`
	Password   string          `json:"password,omitempty"`
	Attrs      json.RawMessage `json:"attrs,omitempty"` // 不超过 1KB
	Transport  Transport       `json:"transport,omitempty"`
}

// JoinRoomRequest 是 POST /rooms/{id}/join 的请求。对已在房间里的人是幂等的（不再查密码、人数和“已开始”），
// 断线重连就走这一条。
type JoinRoomRequest struct {
	Game      string    `json:"game"`
	Password  string    `json:"password,omitempty"`
	Transport Transport `json:"transport,omitempty"`
}

// SetRoomStateRequest 是 POST /rooms/{id}/state 的请求，只有当前房主能调；State 与 Attrs 至少写一个。
type SetRoomStateRequest struct {
	Game  string          `json:"game"`
	State RoomState       `json:"state,omitempty"`
	Attrs json.RawMessage `json:"attrs,omitempty"`
}

// lobby 的错误名（答复里的 error 字段）对应的哨兵错误，用 errors.Is 判断：
//
//	if errors.Is(err, relay.ErrRoomFull) { ... }
//
// 拿到的是 *LobbyError，Status 与 Message 用 errors.As 取。
var (
	ErrBadRequest           = errors.New("lobby: bad_request")           // 400 参数不合法
	ErrUnauthorized         = errors.New("lobby: unauthorized")          // 401 token 无效、过期、游戏不符、账号被封
	ErrGameNotServed        = errors.New("lobby: game_not_served")       // 403 游戏不在 lobby.games 里
	ErrNotHost              = errors.New("lobby: not_host")              // 403 只有房主能做
	ErrWrongPassword        = errors.New("lobby: wrong_password")        // 403 密码不对
	ErrRoomNotFound         = errors.New("lobby: room_not_found")        // 404 房间不存在、已过期、已关闭
	ErrRoomFull             = errors.New("lobby: room_full")             // 409 满员
	ErrRoomStarted          = errors.New("lobby: room_started")          // 409 已开始
	ErrTransportUnavailable = errors.New("lobby: transport_unavailable") // 409 房间所在的 relay 没有这种接入
	ErrTooLarge             = errors.New("lobby: too_large")             // 413 请求体太大
	ErrNoRelay              = errors.New("lobby: no_relay")              // 503 没有可用的 relay
	ErrUnavailable          = errors.New("lobby: unavailable")           // 503 lobby 的依赖不可达
)

var lobbyErrors = map[string]error{
	"bad_request": ErrBadRequest, "unauthorized": ErrUnauthorized, "game_not_served": ErrGameNotServed,
	"not_host": ErrNotHost, "wrong_password": ErrWrongPassword, "room_not_found": ErrRoomNotFound,
	"room_full": ErrRoomFull, "room_started": ErrRoomStarted, "transport_unavailable": ErrTransportUnavailable,
	"too_large": ErrTooLarge, "no_relay": ErrNoRelay, "unavailable": ErrUnavailable,
}

// LobbyError 是 lobby 答的非 200。Code 是答复里的错误名；答复不是 lobby 的 JSON（前置代理的错误页）时 Code 为空。
type LobbyError struct {
	Status  int
	Code    string
	Message string
}

func (e *LobbyError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("lobby: HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("lobby: %s (HTTP %d): %s", e.Code, e.Status, e.Message)
}

// Is 让 errors.Is(err, ErrRoomFull) 这类判断按错误名成立。
func (e *LobbyError) Is(target error) bool {
	known, ok := lobbyErrors[e.Code]
	return ok && known == target
}

// Temporary 报告这个错误值不值得稍后重试：503（依赖不可达、没有 relay）与没有 lobby 错误名的 5xx。
func (e *LobbyError) Temporary() bool { return e.Status >= 500 }

// maxLobbyReplyBytes 是读 lobby 答复的上限；最大的答复是 100 个房间的列表，远小于它。
const maxLobbyReplyBytes = 1 << 20

// GameTokenSource 返回当前的游戏账号 token（gt1）。gt1 有寿命（account.games 的 token_ttl），长时间运行的客户端
// 在这里换新的；每个请求调用一次。
type GameTokenSource func(ctx context.Context) (string, error)

// StaticGameToken 把一张固定的 gt1 当 GameTokenSource 用。
func StaticGameToken(token string) GameTokenSource {
	return func(context.Context) (string, error) { return token, nil }
}

// Lobby 是 lobby 的 HTTP 客户端。并发安全。
type Lobby struct {
	baseURL string
	token   GameTokenSource
	http    *http.Client
}

// NewLobby 建 lobby 客户端。baseURL 是 lobby 的根地址，例如 https://lobby.example.com；httpClient 为 nil 时
// 用 http.DefaultClient（请求的期限由调用方的 ctx 决定）。
func NewLobby(baseURL string, token GameTokenSource, httpClient *http.Client) *Lobby {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Lobby{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: httpClient}
}

// CreateRoom 建房，调用者是房主。拿到的 Ticket 交给 Client.Connect。
func (l *Lobby) CreateRoom(ctx context.Context, req CreateRoomRequest) (Ticket, error) {
	var ticket Ticket
	err := l.call(ctx, http.MethodPost, "/rooms", req, &ticket)
	return ticket, err
}

// ListRooms 列出 game / mode 下可加入的房间（未满、未开始、未过期、房主已连上 relay），新建的在前。
// limit 为 0 取 lobby 的缺省（50），上限 100。
func (l *Lobby) ListRooms(ctx context.Context, game, mode string, limit int) ([]Room, error) {
	query := url.Values{"game": {game}, "mode": {mode}}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	var reply struct {
		Rooms []Room `json:"rooms"`
	}
	err := l.call(ctx, http.MethodGet, "/rooms?"+query.Encode(), nil, &reply)
	return reply.Rooms, err
}

// JoinRoom 加入房间，或给已在房间里的人（含房主）重新签一张 relay 令牌。
func (l *Lobby) JoinRoom(ctx context.Context, roomID string, req JoinRoomRequest) (Ticket, error) {
	var ticket Ticket
	err := l.call(ctx, http.MethodPost, "/rooms/"+url.PathEscape(roomID)+"/join", req, &ticket)
	return ticket, err
}

// LeaveRoom 离开房间：让出席位，lobby 再让 relay 断开这个账号（Closed{left}）。房主离开由 relay 走换 host。
func (l *Lobby) LeaveRoom(ctx context.Context, roomID, game string) (Room, error) {
	var ticket Ticket
	err := l.call(ctx, http.MethodPost, "/rooms/"+url.PathEscape(roomID)+"/leave", map[string]string{"game": game}, &ticket)
	return ticket.Room, err
}

// SetRoomState 改房间状态或自定义属性，只有当前房主能调（否则 ErrNotHost）。
func (l *Lobby) SetRoomState(ctx context.Context, roomID string, req SetRoomStateRequest) (Room, error) {
	var ticket Ticket
	err := l.call(ctx, http.MethodPost, "/rooms/"+url.PathEscape(roomID)+"/state", req, &ticket)
	return ticket.Room, err
}

// call 发一个请求：带上 gt1，body 非 nil 时按 JSON 发；200 解进 out，其余答复换成 *LobbyError。
func (l *Lobby) call(ctx context.Context, method, path string, body, out any) error {
	gameToken, err := l.token(ctx)
	if err != nil {
		return fmt.Errorf("lobby: game token: %w", err)
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("lobby: encode request: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, l.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("lobby: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+gameToken)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := l.http.Do(request)
	if err != nil {
		return fmt.Errorf("lobby: %s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxLobbyReplyBytes))
	if err != nil {
		return fmt.Errorf("lobby: read reply: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		var reply struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &reply) != nil || reply.Error == "" {
			return &LobbyError{Status: response.StatusCode, Message: strings.TrimSpace(string(data))}
		}
		return &LobbyError{Status: response.StatusCode, Code: reply.Error, Message: reply.Message}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("lobby: decode reply: %w", err)
	}
	return nil
}
