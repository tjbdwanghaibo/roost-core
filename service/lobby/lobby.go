// Package lobby 是开房间场景的大厅服（方案 FEATURE-2026-11-16-lobby-relay §4.2、§4.6.4 第 7 步、§4.6.5、§4.7.1）。
//
// 职责只有三件：让玩家找到房间（建房、列房）、把玩家连到同一个 relay 实例（签 relay 令牌、给地址）、跟着 relay 的
// 报告维护房间目录（人数、房主、关房）。不做玩法校验：只看人数、密码和房主身份。
//
// 结构：
//   - 房间目录在 Redis（RoomStore），每条记录带 TTL，只由 relay 的报告续命（Room.AliveUntilUnixMilli）；relay 不再报告
//     （崩溃、房间关闭）时房间在 room TTL 后不在了，客户端反复 join 也不续命。
//     lobby 进程无状态，可以多开；同一个房间的并发修改（加入、relay 报告、房主改状态）都经 RoomStore.Update 的
//     compare-and-set，不存在“读、判断、写”的窗口。
//   - 身份只认 account 签发的游戏账号 token（gt2），校验交给 account 的 ValidateGameToken（GameTokenValidator），
//     lobby 不持有 account 的密钥。
//   - relay 实例从 etcd 发现（RelayDirectory），建房时选房间数最少的那个；房间固定在这个实例上，不迁移。
//   - relay 经 NATS 报告房间事件（RoomEvent，HandleRelayEvent 处理）；lobby 经 NATS 让 relay 执行 leave（RelayCommand）。
//   - relay 令牌由 service/relay/token 签发：角色按 Redis 里的当前房主填（§4.6.5），lobby 与 relay 短暂不一致时以 relay 为准。
package lobby

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/service/relay/token"
)

// RoomState 是房主设置的房间状态。
type RoomState string

const (
	// StateOpen：可加入，出现在列表里（还要未满、relay 已报告房主连上）。
	StateOpen RoomState = "open"
	// StateStarted：已开始，不出现在列表里，新人不能加入；已在房间里的人照常可以重连（join 幂等）。
	// 已开始的房间里，连上过 relay 的成员掉线后席位一直留着，直到离开、被踢或关房（Room.holdsSeat，方案 §6 第 12 条）。
	StateStarted RoomState = "started"
)

// Transport 是客户端连 relay 的方式（§4.7.1）。
type Transport string

const (
	TransportTCP Transport = "tcp"
	TransportWS  Transport = "ws"
)

// 输入的上限。
const (
	MaxAttrsBytes    = 1024
	MaxNameRunes     = 64
	MaxPasswordBytes = 64
	MaxListLimit     = 100
	DefaultListLimit = 50
	// reservationSlack 是成员席位预留比令牌寿命多出来的时间：令牌在 TokenTTL 内用掉，relay 再把“已连上”报告回来。
	reservationSlack = 15 * time.Second
)

// labelPattern 是 game、mode 的写法：它们进 Redis 索引键，只许字母数字、下划线、连字符。
var labelPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

var (
	// ErrUnauthorized：没带游戏账号 token、token 无效、过期、被封，或 token 的游戏与请求的不一致。HTTP 401。
	ErrUnauthorized = errors.New("lobby: unauthorized")
	// ErrBadRequest：参数不合法。HTTP 400。
	ErrBadRequest = errors.New("lobby: bad request")
	// ErrGameNotServed：请求的游戏不在 lobby.games 里。HTTP 403。
	ErrGameNotServed = errors.New("lobby: game is not served by this lobby")
	// ErrNotHost：只有房主能做的操作。HTTP 403。
	ErrNotHost = errors.New("lobby: only the host may do this")
	// ErrWrongPassword：密码不对。HTTP 403。
	ErrWrongPassword = errors.New("lobby: wrong room password")
	// ErrKicked：这个账号被房主踢出过这个房间（被踢名单随房间记录一起过期）。HTTP 403。
	ErrKicked = errors.New("lobby: kicked from this room")
	// ErrRoomMissing：房间不存在、已过期或已关闭。HTTP 404。
	ErrRoomMissing = errors.New("lobby: room not found")
	// ErrRoomFull：满员。HTTP 409。
	ErrRoomFull = errors.New("lobby: room is full")
	// ErrRoomStarted：已开始，新人不能加入。HTTP 409。
	ErrRoomStarted = errors.New("lobby: room has started")
	// ErrTransportUnavailable：房间所在的 relay 没有客户端要的接入方式（tcp / ws）。HTTP 409。
	ErrTransportUnavailable = errors.New("lobby: the room's relay does not offer this transport")
	// ErrNoRelay：etcd 里没有可用的 relay 实例。HTTP 503。
	ErrNoRelay = errors.New("lobby: no relay instance is available")
)

// GameTokenValidator 校验游戏账号 token（gt2），返回账号。生产实现是 account 的 ValidateGameToken RPC
// （wiring/lobby 接线），token 里的游戏必须等于 game。token 无效、过期、游戏不符、账号被封都返回包着
// ErrUnauthorized 的错误；其他错误（account 不可达）按服务不可用处理。
type GameTokenValidator interface {
	ValidateGameToken(ctx context.Context, gameToken, game string) (account string, err error)
}

// Publisher 是发给 relay 的命令通道（NATS 的 Publish）。
type Publisher interface {
	Publish(subject string, data []byte) error
}

// GameRule 是 lobby.games 里一个游戏的规则。
type GameRule struct {
	// MaxMembers 是这个游戏的房间人数上限（含房主）。建房不写 max_members 时取它。
	MaxMembers int
}

// Config 是 Service 的依赖与参数。
type Config struct {
	// Games 是本 lobby 服务的游戏；不在里面的游戏一律拒绝。必填。
	Games map[string]GameRule
	// Rooms 是房间目录。必填（NewRedisRooms；单测用 NewMemoryRooms）。
	Rooms RoomStore
	// Tokens 校验游戏账号 token。必填。
	Tokens GameTokenValidator
	// Relays 列出 relay 实例。必填。
	Relays RelayDirectory
	// Commands 发 relay 命令；Subject 是命令主题的前缀（RelayCommandSubject）。都必填。
	Commands Publisher
	Subject  string
	// RelayTokenKey 是 relay 令牌的 HMAC 密钥，与 relay 同一把。必填，至少 token.MinKeyBytes 字节。
	RelayTokenKey []byte
	// RelayTokenTTL 是 relay 令牌寿命；0 取 token.DefaultTTL。
	RelayTokenTTL time.Duration
	// Now 是系统时间；nil 取 time.Now。令牌、预留席位都按系统时间。
	Now func() time.Time
	// NewID 生成房间号；nil 取 64 位随机数的十六进制。测试缝。
	NewID func() (string, error)
}

// Service 是大厅的全部操作。并发安全：自己不持有可变状态，房间状态都在 RoomStore 里。
type Service struct {
	cfg Config
}

// New 检查 cfg 并返回 Service。
func New(cfg Config) (*Service, error) {
	var missing []string
	if len(cfg.Games) == 0 {
		missing = append(missing, "Games")
	}
	if cfg.Rooms == nil {
		missing = append(missing, "Rooms")
	}
	if cfg.Tokens == nil {
		missing = append(missing, "Tokens")
	}
	if cfg.Relays == nil {
		missing = append(missing, "Relays")
	}
	if cfg.Commands == nil {
		missing = append(missing, "Commands")
	}
	if strings.TrimSpace(cfg.Subject) == "" {
		missing = append(missing, "Subject")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("lobby: %s required", strings.Join(missing, ", "))
	}
	if len(cfg.RelayTokenKey) < token.MinKeyBytes {
		return nil, fmt.Errorf("lobby: relay token key has %d bytes, need at least %d", len(cfg.RelayTokenKey), token.MinKeyBytes)
	}
	for name, rule := range cfg.Games {
		if !labelPattern.MatchString(name) {
			return nil, fmt.Errorf("lobby: game name %q must match %s", name, labelPattern)
		}
		if rule.MaxMembers < 1 || rule.MaxMembers > token.MaxMembersLimit {
			return nil, fmt.Errorf("lobby: game %s max members %d is outside 1..%d", name, rule.MaxMembers, token.MaxMembersLimit)
		}
	}
	if cfg.RelayTokenTTL <= 0 {
		cfg.RelayTokenTTL = token.DefaultTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NewID == nil {
		cfg.NewID = randomID
	}
	return &Service{cfg: cfg}, nil
}

// Room 是 Redis 里的一条房间记录。只在 lobby 内部与存储之间流转，客户端看到的是 RoomView。
type Room struct {
	ID         string    `json:"id"`
	Game       string    `json:"game"`
	Mode       string    `json:"mode"`
	Name       string    `json:"name"`
	MaxMembers int       `json:"max_members"`
	State      RoomState `json:"state"`
	// Closed：relay 报告房间已关闭（或房主在 relay 建房前离开）。不再出现在列表里、不能加入，记录随 TTL 消失。
	Closed bool `json:"closed,omitempty"`
	// PasswordHash 是 sha256(房间号 + "\n" + 密码) 的十六进制；空表示没有密码。
	PasswordHash string          `json:"password_hash,omitempty"`
	Attrs        json.RawMessage `json:"attrs,omitempty"`
	// Host 是当前房主账号，HostEpoch 是 relay 的 host_epoch：只接受更大的 epoch 改写（§4.6.4 第 7 步）。
	Host      string `json:"host"`
	HostEpoch uint64 `json:"host_epoch"`
	// Relay 是建房时选中的 relay 实例与它的对外地址；房间固定在它上面。
	Relay Relay `json:"relay"`
	// Members 是占着席位的人（含房主），规则见 holdsSeat。
	Members []Member `json:"members"`
	// Kicked 是被房主踢出过的账号（relay 报告 member_left、原因 kicked 时记下）：之后 join 与 relay 的验权都拒绝它，
	// 名单随房间记录一起过期（方案 §4.4）。
	Kicked []string `json:"kicked,omitempty"`
	// Leaving 是经 lobby 离开、relay 还没确认的账号（2026-11-17，交接 A1）。relay 的在线名单是带延迟的快照：
	// 离开之前发出、离开之后才处理到的报告里还有他。名单里的人不会被这种报告补回席位，也不会被重新记成 Connected；
	// relay 报告他 member_left（序号更大）或者他重新 join 成功时清掉；更新的报告仍说他在线时，lobby 再发一次 leave 命令。
	Leaving []string `json:"leaving,omitempty"`
	// RelaySeq 是已应用的 relay 报告的最大序号；0 表示 relay 还没报告过（房主还没连上）。
	RelaySeq      uint64 `json:"relay_seq,omitempty"`
	CreatedAtUnix int64  `json:"created_at_unix"`
	// AliveUntilUnixMilli 是房间的寿命：建房时与每条被接受的 relay 报告时设为此刻加 room TTL（RoomStore.TTL），
	// 过了这一刻房间按不存在处理（gone）。只有 relay 的报告续命：加入、离开、改状态也写记录（存储的键 TTL 随之重置），
	// 但不改它。否则 relay 崩溃后，按 SDK 重连策略反复 join 的客户端会让房间一直留在目录里、一直列在大厅里
	// （2026-11-17 多进程演练，evidence/lobby-relay-multiprocess-20261117）。按 lobby 的系统时间比较，多个 lobby
	// 实例之间的时钟差直接算进寿命。
	AliveUntilUnixMilli int64 `json:"alive_until_unix_ms"`
}

// Member 是一个席位。
type Member struct {
	Account string `json:"account"`
	// Online：relay 最近一次报告里这个人连着。
	Online bool `json:"online,omitempty"`
	// ReservedUntilUnix：lobby 给这个人签了令牌，席位留到这一刻；连上后清零。
	ReservedUntilUnix int64 `json:"reserved_until_unix,omitempty"`
	// Connected：relay 报告过他连上（之后掉线也不清）。房间已开始时，靠它把掉线的人的席位一直留着；
	// 主动离开（lobby 的 leave、relay 报告 member_left 原因 left）清掉它，席位按没开局的规则让出。
	Connected bool `json:"connected,omitempty"`
	// PresenceSeq 是 Connected 依据的那条 relay 报告的序号：报告里他在线，或 member_left 原因 left 说他主动离开。
	// relay 的报告会乱序到达，Connected 只被比它更新的事实改写（Room.notePresence），之后不再跟着房间水位 RelaySeq 走。
	// 席位新建时它是下界：JoinRoom 给新加入的人建预留时取当时的 RelaySeq——同一账号上一个席位被删后还在路上的旧
	// 报告（序号不超过这个水位）说的是上一个席位，不能把还没连上的新预留记成“连上过”；新连接一定发生在签出令牌
	// 之后，relay 在锁内给快照占序号，含这条新连接的报告序号必然大于它。relay 名单补进来的席位取那条报告的序号
	// （Room.applyOnline）。经 lobby 离开、由 relay 确认的人，按确认那条报告的序号记成主动离开（HandleRelayEvent）。
	PresenceSeq uint64 `json:"presence_seq,omitempty"`
}

// RoomView 是客户端看到的房间。
type RoomView struct {
	ID            string          `json:"id"`
	Game          string          `json:"game"`
	Mode          string          `json:"mode"`
	Name          string          `json:"name"`
	MaxMembers    int             `json:"max_members"`
	Members       int             `json:"members"`
	HasPassword   bool            `json:"has_password"`
	State         RoomState       `json:"state"`
	Host          string          `json:"host"`
	HostEpoch     uint64          `json:"host_epoch"`
	Attrs         json.RawMessage `json:"attrs,omitempty"`
	CreatedAtUnix int64           `json:"created_at_unix"`
}

// Ticket 是建房、加入、离开、改状态的统一答复 {room, relay_addr, token, expires_at}。离开与改状态不签令牌，
// 后三个字段为空。
type Ticket struct {
	Room      RoomView `json:"room"`
	RelayAddr string   `json:"relay_addr,omitempty"`
	Token     string   `json:"token,omitempty"`
	// ExpiresAt 是 relay 令牌的过期时刻（Unix 秒）。
	ExpiresAt int64 `json:"expires_at,omitempty"`
}

// View 返回客户端可见的部分。
func (r Room) View() RoomView {
	return RoomView{
		ID: r.ID, Game: r.Game, Mode: r.Mode, Name: r.Name, MaxMembers: r.MaxMembers, Members: len(r.Members),
		HasPassword: r.PasswordHash != "", State: r.State, Host: r.Host, HostEpoch: r.HostEpoch, Attrs: r.Attrs,
		CreatedAtUnix: r.CreatedAtUnix,
	}
}

// listable 报告房间能不能出现在列表里：可加入、未满、relay 已报告房主连上、没关。
func (r Room) listable() bool {
	return !r.Closed && r.State == StateOpen && r.RelaySeq > 0 && len(r.Members) < r.MaxMembers
}

// gone 报告房间对客户端与 relay 来说已经不在：已关闭，或最后一条 relay 报告（没有报告过就是建房）之后过了 room TTL。
// 存储里的记录可能还在（加入、离开等写入重置了键的 TTL），等键自己过期。
func (r Room) gone(now time.Time) bool {
	return r.Closed || now.UnixMilli() >= r.AliveUntilUnixMilli
}

// keepAlive 把寿命延长到 now 起一个 room TTL：只在建房与接受 relay 报告时调用。
func (r *Room) keepAlive(now time.Time, ttl time.Duration) {
	r.AliveUntilUnixMilli = now.Add(ttl).UnixMilli()
}

func (r Room) kicked(account string) bool { return slices.Contains(r.Kicked, account) }

func (r Room) leaving(account string) bool { return slices.Contains(r.Leaving, account) }

func (r *Room) markLeaving(account string) {
	if !r.leaving(account) {
		r.Leaving = append(r.Leaving, account)
	}
}

func (r *Room) clearLeaving(account string) {
	r.Leaving = slices.DeleteFunc(r.Leaving, func(a string) bool { return a == account })
	if len(r.Leaving) == 0 {
		r.Leaving = nil
	}
}

func (r Room) memberIndex(account string) int {
	for i, member := range r.Members {
		if member.Account == account {
			return i
		}
	}
	return -1
}

// holdsSeat 是席位规则，加入、relay 报告、验权都按它算（被踢、离开、关房另有入口直接移除）：
//   - 在线的、房主（房主断线等 relay 换 host，§4.6.1）、预留期内的（拿了令牌还没连上）占着席位；
//   - 房间已开始时，relay 报告过连上的人（Connected）掉线后也一直占着，直到主动离开、被踢或关房；他再 join 就回到
//     原座（方案 §6 第 12 条）。拿了令牌却从没连上的人不算：预留过期照样让出，因为 relay 上没有他的座位，房主也踢不到他。
//
// 没开局的房间（以及改回 open 的房间）：不在线、过了预留期就让出。
func (r *Room) holdsSeat(member Member, now time.Time) bool {
	return member.Online || member.Account == r.Host || member.ReservedUntilUnix > now.Unix() ||
		(r.State == StateStarted && member.Connected)
}

// pruneReservations 按 holdsSeat 去掉不再占席位的人。
func (r *Room) pruneReservations(now time.Time) {
	kept := r.Members[:0]
	for _, member := range r.Members {
		if r.holdsSeat(member, now) {
			kept = append(kept, member)
		}
	}
	r.Members = kept
}

func randomID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
