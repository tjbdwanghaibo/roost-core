package lobby

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tjbdwanghaibo/roost-core/service/relay/token"
)

// CreateRequest 是 POST /rooms 的请求体。
type CreateRequest struct {
	Game string `json:"game"`
	Mode string `json:"mode"`
	// MaxMembers 含房主；0 取 lobby.games.<game>.max_members，不能超过它。
	MaxMembers int    `json:"max_members"`
	Name       string `json:"name"`
	Password   string `json:"password,omitempty"`
	// Attrs 是自定义属性，原样存、原样给，不超过 1KB，lobby 不解析。
	Attrs json.RawMessage `json:"attrs,omitempty"`
	// Transport 是客户端要连 relay 的方式：tcp（缺省）或 ws。
	Transport Transport `json:"transport,omitempty"`
}

// JoinRequest 是 POST /rooms/{id}/join 的请求体。
type JoinRequest struct {
	Game      string    `json:"game"`
	Password  string    `json:"password,omitempty"`
	Transport Transport `json:"transport,omitempty"`
}

// LeaveRequest 是 POST /rooms/{id}/leave 的请求体。
type LeaveRequest struct {
	Game string `json:"game"`
}

// StateRequest 是 POST /rooms/{id}/state 的请求体：state 与 attrs 至少写一个。
type StateRequest struct {
	Game  string          `json:"game"`
	State RoomState       `json:"state,omitempty"`
	Attrs json.RawMessage `json:"attrs,omitempty"`
}

// authenticate 核对游戏在本 lobby 的列表里，再把 token 交给 account 校验（token 的游戏必须等于 game）。
func (s *Service) authenticate(ctx context.Context, gameToken, game string) (string, GameRule, error) {
	rule, served := s.cfg.Games[game]
	if !served {
		return "", GameRule{}, fmt.Errorf("%w: %q", ErrGameNotServed, game)
	}
	if strings.TrimSpace(gameToken) == "" {
		return "", GameRule{}, fmt.Errorf("%w: no game token", ErrUnauthorized)
	}
	account, err := s.cfg.Tokens.ValidateGameToken(ctx, gameToken, game)
	if err != nil {
		return "", GameRule{}, err
	}
	if account == "" {
		return "", GameRule{}, fmt.Errorf("%w: the validator returned no account", ErrUnauthorized)
	}
	return account, rule, nil
}

// CreateRoom 建房，调用者是房主：选 relay、写房间记录、签房主令牌。
func (s *Service) CreateRoom(ctx context.Context, gameToken string, req CreateRequest) (Ticket, error) {
	account, rule, err := s.authenticate(ctx, gameToken, req.Game)
	if err != nil {
		return Ticket{}, err
	}
	transport, err := checkTransport(req.Transport)
	if err != nil {
		return Ticket{}, err
	}
	if !labelPattern.MatchString(req.Mode) {
		return Ticket{}, fmt.Errorf("%w: mode must match %s", ErrBadRequest, labelPattern)
	}
	if req.MaxMembers == 0 {
		req.MaxMembers = rule.MaxMembers
	}
	if req.MaxMembers < 1 || req.MaxMembers > rule.MaxMembers {
		return Ticket{}, fmt.Errorf("%w: max_members must be 1..%d for %s", ErrBadRequest, rule.MaxMembers, req.Game)
	}
	if utf8.RuneCountInString(req.Name) > MaxNameRunes || len(req.Password) > MaxPasswordBytes {
		return Ticket{}, fmt.Errorf("%w: name is limited to %d characters and password to %d bytes", ErrBadRequest, MaxNameRunes, MaxPasswordBytes)
	}
	if err := checkAttrs(req.Attrs); err != nil {
		return Ticket{}, err
	}
	relays, err := s.cfg.Relays.Relays(ctx)
	if err != nil {
		return Ticket{}, err
	}
	relay, ok := pickRelay(relays, transport)
	if !ok {
		return Ticket{}, fmt.Errorf("%w for transport %s", ErrNoRelay, transport)
	}

	now := s.cfg.Now()
	room := Room{
		Game: req.Game, Mode: req.Mode, Name: req.Name, MaxMembers: req.MaxMembers, State: StateOpen,
		Attrs: req.Attrs, Host: account, Relay: relay, CreatedAtUnix: now.Unix(),
		Members: []Member{{Account: account, ReservedUntilUnix: s.reservedUntil(now)}},
	}
	room.keepAlive(now, s.cfg.Rooms.TTL())
	// 房间号随机生成；撞上已有的（64 位随机数，实际不会发生）换一个再试。
	for attempt := 0; ; attempt++ {
		if room.ID, err = s.cfg.NewID(); err != nil {
			return Ticket{}, fmt.Errorf("lobby: room id: %w", err)
		}
		room.PasswordHash = passwordHash(room.ID, req.Password)
		created, err := s.cfg.Rooms.Create(ctx, room)
		if err != nil {
			return Ticket{}, err
		}
		if created {
			break
		}
		if attempt == 2 {
			return Ticket{}, fmt.Errorf("lobby: could not allocate a room id")
		}
	}
	return s.ticket(room, account, transport, now)
}

// ListRooms 列出 game/mode 下可加入的房间：未满、未开始、未关闭、relay 已报告房主连上、未过期。
func (s *Service) ListRooms(ctx context.Context, gameToken, game, mode string, limit int) ([]RoomView, error) {
	if _, _, err := s.authenticate(ctx, gameToken, game); err != nil {
		return nil, err
	}
	if !labelPattern.MatchString(mode) {
		return nil, fmt.Errorf("%w: mode must match %s", ErrBadRequest, labelPattern)
	}
	if limit <= 0 {
		limit = DefaultListLimit
	}
	limit = min(limit, MaxListLimit)
	rooms, err := s.cfg.Rooms.List(ctx, game, mode, limit)
	if err != nil {
		return nil, err
	}
	now := s.cfg.Now()
	views := make([]RoomView, 0, len(rooms))
	for _, room := range rooms {
		room.pruneReservations(now)
		if !room.gone(now) && room.listable() {
			views = append(views, room.View())
		}
	}
	return views, nil
}

// JoinRoom 加入房间并签令牌。
//
// 已经在房间里的人（含房主）再调是幂等的：不查密码、不查人数与“已开始”，不算新人（不再占一个席位），席位预留期
// 重新算，重新签一张令牌——断线重连就走这条路（§4.3）。已开始的房间里掉线的人席位一直留着（Room.holdsSeat），
// 所以他再 join 就是回到原座。角色按 Redis 里的当前房主填（§4.6.5）。
func (s *Service) JoinRoom(ctx context.Context, gameToken, roomID string, req JoinRequest) (Ticket, error) {
	account, _, err := s.authenticate(ctx, gameToken, req.Game)
	if err != nil {
		return Ticket{}, err
	}
	transport, err := checkTransport(req.Transport)
	if err != nil {
		return Ticket{}, err
	}
	now := s.cfg.Now()
	room, err := s.cfg.Rooms.Update(ctx, roomID, func(room *Room) (bool, error) {
		if room.Game != req.Game || room.gone(now) {
			return false, ErrRoomMissing
		}
		if room.Relay.Addr(transport) == "" {
			return false, fmt.Errorf("%w: %s", ErrTransportUnavailable, transport)
		}
		if room.kicked(account) {
			return false, ErrKicked
		}
		room.pruneReservations(now)
		if i := room.memberIndex(account); i >= 0 {
			room.Members[i].ReservedUntilUnix = s.reservedUntil(now)
			room.clearLeaving(account)
			return true, nil
		}
		switch {
		case room.State != StateOpen:
			return false, ErrRoomStarted
		case !passwordMatches(room.PasswordHash, room.ID, req.Password):
			return false, ErrWrongPassword
		case len(room.Members) >= room.MaxMembers:
			return false, ErrRoomFull
		}
		// 新席位的事实从房间当前水位起算（Member.PresenceSeq）：同一账号上一个席位留下的、还在路上的旧报告
		// 不能把这个还没连上 relay 的预留记成“连上过”。
		room.Members = append(room.Members, Member{Account: account, ReservedUntilUnix: s.reservedUntil(now), PresenceSeq: room.RelaySeq})
		room.clearLeaving(account)
		return true, nil
	})
	if err != nil {
		return Ticket{}, err
	}
	return s.ticket(room, account, transport, now)
}

// LeaveRoom 离开房间。
//
// 成员：立即让出席位，并让 relay 断开他（RelayCommand leave）。房主：席位不动，让 relay 走换 host（§4.6.1“房主离开”），
// 新房主由 relay 的 host_changed 报告回来；同时清掉他的 Connected，换完 host 后他按没开局的规则让出席位（已开始的
// 房间也不给主动离开的人留座）。relay 还没报告过这个房间（房主从没连上）时没有可换的人，直接关房。
// 不在房间里的人调用是空操作。
func (s *Service) LeaveRoom(ctx context.Context, gameToken, roomID string, req LeaveRequest) (Ticket, error) {
	account, _, err := s.authenticate(ctx, gameToken, req.Game)
	if err != nil {
		return Ticket{}, err
	}
	notifyRelay := false
	now := s.cfg.Now()
	room, err := s.cfg.Rooms.Update(ctx, roomID, func(room *Room) (bool, error) {
		notifyRelay = false
		if room.Game != req.Game || room.gone(now) {
			return false, ErrRoomMissing
		}
		if account == room.Host {
			if room.RelaySeq == 0 {
				room.Closed = true
				return true, nil
			}
			notifyRelay = true
			i := room.memberIndex(account)
			if i < 0 || !room.Members[i].Connected {
				return false, nil
			}
			room.Members[i].Connected = false
			room.markLeaving(account)
			return true, nil
		}
		i := room.memberIndex(account)
		if i < 0 {
			return false, nil
		}
		room.Members = append(room.Members[:i], room.Members[i+1:]...)
		notifyRelay = room.RelaySeq > 0
		if notifyRelay {
			room.markLeaving(account)
		}
		return true, nil
	})
	if err != nil {
		return Ticket{}, err
	}
	if notifyRelay {
		s.sendRelayCommand(room.Relay.Instance, RelayCommand{Type: CommandLeave, Room: room.ID, Account: account})
	}
	return Ticket{Room: room.View()}, nil
}

// SetRoomState 只有当前房主能调：改“可加入 / 已开始”，或替换自定义属性。
func (s *Service) SetRoomState(ctx context.Context, gameToken, roomID string, req StateRequest) (Ticket, error) {
	account, _, err := s.authenticate(ctx, gameToken, req.Game)
	if err != nil {
		return Ticket{}, err
	}
	if req.State == "" && req.Attrs == nil {
		return Ticket{}, fmt.Errorf("%w: state or attrs is required", ErrBadRequest)
	}
	if req.State != "" && req.State != StateOpen && req.State != StateStarted {
		return Ticket{}, fmt.Errorf("%w: state must be %s or %s", ErrBadRequest, StateOpen, StateStarted)
	}
	if err := checkAttrs(req.Attrs); err != nil {
		return Ticket{}, err
	}
	now := s.cfg.Now()
	room, err := s.cfg.Rooms.Update(ctx, roomID, func(room *Room) (bool, error) {
		if room.Game != req.Game || room.gone(now) {
			return false, ErrRoomMissing
		}
		if room.Host != account {
			return false, ErrNotHost
		}
		if req.State != "" {
			room.State = req.State
		}
		if req.Attrs != nil {
			room.Attrs = req.Attrs
		}
		return true, nil
	})
	if err != nil {
		return Ticket{}, err
	}
	return Ticket{Room: room.View()}, nil
}

// ticket 签 relay 令牌：角色按房间记录里的当前房主填。
func (s *Service) ticket(room Room, account string, transport Transport, now time.Time) (Ticket, error) {
	role := token.RoleMember
	if account == room.Host {
		role = token.RoleHost
	}
	expiresAt := now.Add(s.cfg.RelayTokenTTL)
	signed, err := token.Sign(s.cfg.RelayTokenKey, token.Claims{
		Relay: room.Relay.Instance, Room: room.ID, Account: account, Role: role,
		MaxMembers: room.MaxMembers, ExpiresAt: expiresAt,
	})
	if err != nil {
		return Ticket{}, err
	}
	return Ticket{Room: room.View(), RelayAddr: room.Relay.Addr(transport), Token: signed, ExpiresAt: expiresAt.Unix()}, nil
}

// reservedUntil 是新签令牌的席位留到的时刻：令牌寿命加上 relay 报告回来的余量。
func (s *Service) reservedUntil(now time.Time) int64 {
	return now.Add(s.cfg.RelayTokenTTL + reservationSlack).Unix()
}

func (s *Service) sendRelayCommand(instance string, command RelayCommand) {
	data, err := json.Marshal(command)
	if err == nil {
		err = s.cfg.Commands.Publish(RelayCommandSubject(s.cfg.Subject, instance), data)
	}
	if err != nil {
		// 命令是尽力而为：席位已经在目录里让出，客户端自己断开 relay 连接也是离开（§4.2）。
		slog.Warn("lobby: relay command not sent", "relay", instance, "room", command.Room, "type", command.Type, "err", err)
	}
}

func checkTransport(transport Transport) (Transport, error) {
	switch transport {
	case "":
		return TransportTCP, nil
	case TransportTCP, TransportWS:
		return transport, nil
	}
	return "", fmt.Errorf("%w: transport must be %s or %s", ErrBadRequest, TransportTCP, TransportWS)
}

func checkAttrs(attrs json.RawMessage) error {
	if len(attrs) > MaxAttrsBytes {
		return fmt.Errorf("%w: attrs is limited to %d bytes", ErrBadRequest, MaxAttrsBytes)
	}
	if attrs != nil && !json.Valid(attrs) {
		return fmt.Errorf("%w: attrs is not JSON", ErrBadRequest)
	}
	return nil
}

func passwordHash(roomID, password string) string {
	if password == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(roomID + "\n" + password))
	return hex.EncodeToString(sum[:])
}

func passwordMatches(stored, roomID, presented string) bool {
	if stored == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(stored), []byte(passwordHash(roomID, presented))) == 1
}

// errorStatus 是错误的 HTTP 状态与给客户端的错误名。没有归类的错误（存储、account、etcd 不可达）一律 503，
// 错误名 unavailable，细节只进日志。
func errorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, ErrBadRequest):
		return 400, "bad_request"
	case errors.Is(err, ErrUnauthorized):
		return 401, "unauthorized"
	case errors.Is(err, ErrGameNotServed):
		return 403, "game_not_served"
	case errors.Is(err, ErrNotHost):
		return 403, "not_host"
	case errors.Is(err, ErrWrongPassword):
		return 403, "wrong_password"
	case errors.Is(err, ErrKicked):
		return 403, "kicked"
	case errors.Is(err, ErrRoomMissing):
		return 404, "room_not_found"
	case errors.Is(err, ErrRoomFull):
		return 409, "room_full"
	case errors.Is(err, ErrRoomStarted):
		return 409, "room_started"
	case errors.Is(err, ErrTransportUnavailable):
		return 409, "transport_unavailable"
	case errors.Is(err, ErrNoRelay):
		return 503, "no_relay"
	}
	return 503, "unavailable"
}
