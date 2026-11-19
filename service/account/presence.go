package account

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/tjbdwanghaibo/roost-core/infra/network/httpserver"
)

// 区服入口登记（维护者 D23，2026-11-14）：game 进程经总线定期向 account 上报“本服现在连这个地址、这么忙”，
// account 把它放进一张**带过期**的登记表；登录入口（portal 模块，manifest 那一层）给客户端的区服清单从这里取地址。
// 超过期限没有再报的条目不再给出——进程死了、网络断了，它的地址自己从清单里消失，不用谁去删。
//
// 为什么放在 account：入口（manifest）可以有多个副本，每个副本各存一张进程内的表时上报要广播到每个副本，否则
// 两次请求打到两个副本看到的清单不一样。account 的状态本来就在共享存储（Redis）里，登记表也放在那里：上报经
// 总线投给任意一个 account 副本，任何副本读到的都是同一张表。
//
// 契约：
//
//   - 登记是**易失**的事实，不是区服记录：区服有没有、叫什么、开没开、并没并仍是 UpsertServer / MergeServer 的
//     控制面数据（ListServers）。登记表里有、account 没登记的服，入口不列出；account 登记了而登记表里没有的服，
//     入口照常列出，只是没有动态地址（回落到入口自己的静态配置）。
//   - 过期由存储判断（Redis 用服务器自己的时钟，见 redis_presence.go），所以多个 account 副本之间不比较各自的时钟。
//     到期那一刻起不再给出。
//   - 至多 MaxServers 个没过期的服；满了之后新的服号被拒绝（ErrRangeInvalid），已登记的照常续期，过期的不占名额。
//   - 信任模型与本接口其余方法相同：总线只在内网，调用方可信（NATS 内部信任）。上报者自报服号，account 不核对
//     “这个进程真的是那个服”。
//   - 上报的间隔应明显短于期限（缺省每 10 秒报一次、期限 30 秒）：期限就是“进程死了之后它的地址还会被发出去多久”。

// MaxPresenceTTLSeconds 是一次登记的最长期限（秒）。期限按秒过总线（servicerpc 不让 time.Duration 上总线）。
const MaxPresenceTTLSeconds = 600

// 登记条目各字段的上限，与登录入口目录条目的上限一致。MaxPresenceAddrBytes 同时是 Addr 与 WSAddr 的上限。
const (
	MaxPresenceAddrBytes = 255
	MaxPresenceTags      = 8
	MaxPresenceTagBytes  = 32
)

// ServerPresence 是一个区服在登记表里的条目：客户端该连的地址与负载。
type ServerPresence struct {
	// ServerID 是区服号（上报的 game 进程的 sid），正数。
	ServerID int32 `json:"server_id"`
	// Addr 是客户端经 TCP 该连的地址，host:port，主机不能为空——通常是这个服前面的 Gate，直连部署时是 game 的玩家 TCP 地址。
	Addr string `json:"addr"`
	// WSAddr 是客户端经 WebSocket 该连的完整 URL（ws:// 或 wss://，见 CheckWebSocketAddr），浏览器（Unity WebGL）用它；
	// 空表示这个服不给 WebSocket 地址。它是 URL 而不是 host:port：前面常有终结 TLS 的负载均衡（wss），路径也由部署决定。
	// 不带这个字段的旧条目（Redis 里的、旧进程报的）等同只有 TCP（维护者 2026-11-18）。
	WSAddr string `json:"ws_addr,omitempty"`
	// Load 是这个服现在多忙（缺省上报在线连接数），不小于 0。account 与入口都不解释它。
	Load int `json:"load"`
	// Tags 是给客户端与推荐规则看的标签，至多 MaxPresenceTags 个、每个至多 MaxPresenceTagBytes 字节。
	Tags []string `json:"tags,omitempty"`
}

func (presence ServerPresence) clone() ServerPresence {
	presence.Tags = slices.Clone(presence.Tags)
	return presence
}

// validate 检查条目成不成样子，返回整理过的条目（地址去掉两端空白、标签另拷一份）。
func (presence ServerPresence) validate() (ServerPresence, error) {
	if presence.ServerID <= 0 {
		return ServerPresence{}, fmt.Errorf("%w: server presence has server id %d; it must be positive", ErrRangeInvalid, presence.ServerID)
	}
	addr := strings.TrimSpace(presence.Addr)
	if addr == "" || len(addr) > MaxPresenceAddrBytes {
		return ServerPresence{}, fmt.Errorf("%w: server %d presence address must be 1..%d bytes", ErrRangeInvalid, presence.ServerID, MaxPresenceAddrBytes)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || strings.TrimSpace(host) == "" {
		return ServerPresence{}, fmt.Errorf("%w: server %d presence address %q is not host:port with a host", ErrRangeInvalid, presence.ServerID, addr)
	}
	if number, err := strconv.Atoi(port); err != nil || number <= 0 || number > 65535 {
		return ServerPresence{}, fmt.Errorf("%w: server %d presence address %q has no usable port", ErrRangeInvalid, presence.ServerID, addr)
	}
	if presence.WSAddr != "" {
		wsAddr, err := CheckWebSocketAddr(presence.WSAddr)
		if err != nil {
			return ServerPresence{}, fmt.Errorf("%w: server %d presence websocket address: %v", ErrRangeInvalid, presence.ServerID, err)
		}
		presence.WSAddr = wsAddr
	}
	if presence.Load < 0 {
		return ServerPresence{}, fmt.Errorf("%w: server %d presence has a negative load", ErrRangeInvalid, presence.ServerID)
	}
	if len(presence.Tags) > MaxPresenceTags {
		return ServerPresence{}, fmt.Errorf("%w: server %d presence has %d tags; at most %d", ErrRangeInvalid, presence.ServerID, len(presence.Tags), MaxPresenceTags)
	}
	for _, tag := range presence.Tags {
		if tag == "" || len(tag) > MaxPresenceTagBytes {
			return ServerPresence{}, fmt.Errorf("%w: server %d presence tag must be 1..%d bytes", ErrRangeInvalid, presence.ServerID, MaxPresenceTagBytes)
		}
	}
	presence.Addr = addr
	return presence.clone(), nil
}

// CheckWebSocketAddr 检查一个给客户端的 WebSocket 地址，返回去掉两端空白的写法。规则只在这里一处（account 收上报、
// 登录入口的配置与目录条目、login 模块的上报配置都调它）：
//
//   - 完整的 URL，scheme 是小写的 ws 或 wss（TLS 由前置负载均衡终结时是 wss）；
//   - 有主机（不能是 "ws://:7080/ws" 或 "ws:///ws"）；写了端口时在 1～65535，不写取 scheme 的缺省端口；
//   - 主机是浏览器能用的（httpserver.BrowserHost）：末段是数字的主机必须是点分十进制 IPv4（192.0.2.300、game.1 浏览器
//     解析不了，127.1、010.0.0.1 浏览器会改写），IPv6 必须写在方括号里（ws://2001:db8::1:7080/ws 浏览器解析不了，
//     RV-20261119-02）且不带 %区域号；DNS 名不限；
//   - 不带用户名密码、不带片段（#…），不含空白；路径与查询串原样保留（路径由部署决定，缺省的玩家接入是 /ws）；
//   - 至多 MaxPresenceAddrBytes（255）字节。
func CheckWebSocketAddr(raw string) (string, error) {
	addr := strings.TrimSpace(raw)
	if addr == "" || len(addr) > MaxPresenceAddrBytes || strings.ContainsFunc(addr, unicode.IsSpace) {
		return "", fmt.Errorf("the websocket address must be a ws:// or wss:// URL of 1..%d bytes without spaces", MaxPresenceAddrBytes)
	}
	parsed, err := url.Parse(addr)
	if err != nil || (parsed.Scheme != "ws" && parsed.Scheme != "wss") || parsed.Opaque != "" {
		return "", errors.New("the websocket address must be a ws:// or wss:// URL")
	}
	if parsed.User != nil || parsed.Fragment != "" || strings.Contains(addr, "#") {
		return "", errors.New("the websocket address must not carry a user, a password or a #fragment")
	}
	if parsed.Hostname() == "" {
		return "", errors.New("the websocket address must name the host clients connect to")
	}
	// 浏览器（Unity WebGL、TS SDK 的缺省传输）按 WHATWG URL 解析：它解析不了的数字主机在这里拒绝，不让连不上的地址
	// 进区服目录（RV-20261118-13）；它会改写的 IPv4 写法（127.1、010.0.0.1）也拒绝并给出该写的值，别的客户端未必同样读。
	browserHost, err := httpserver.BrowserHost(parsed)
	if err != nil {
		return "", fmt.Errorf("the websocket address: %w", err)
	}
	if !strings.HasPrefix(parsed.Host, "[") && browserHost != parsed.Hostname() {
		return "", fmt.Errorf("the websocket address host %q is an IPv4 address browsers rewrite to %q and other clients may read differently; write %q", parsed.Hostname(), browserHost, browserHost)
	}
	if port := parsed.Port(); port != "" || strings.HasSuffix(parsed.Host, ":") {
		if number, err := strconv.Atoi(port); err != nil || number <= 0 || number > 65535 {
			return "", errors.New("the websocket address has no usable port")
		}
	}
	return addr, nil
}

// PresenceStore 存登记表。实现要并发安全，并且由存储自己判断过期（多个 account 副本共用一份时不比较各自的时钟）。
type PresenceStore interface {
	// Put 登记或续期：从现在起 ttl 之内 List 给出它；同一个服再登记是覆盖并重新计时。已有 MaxServers 个
	// 没过期的服而这是一个新的服号时返回 ErrRangeInvalid、什么都不改。
	Put(ctx context.Context, presence ServerPresence, ttl time.Duration) error
	// Delete 立刻撤掉一个服的登记；没有登记过不是错误。
	Delete(ctx context.Context, serverID int32) error
	// List 返回没过期的条目（顺序不限，Service 排序）。
	List(ctx context.Context) ([]ServerPresence, error)
}

// ReportServerPresence 登记或续期一个服的入口条目，ttlSeconds 秒之内有效（1..MaxPresenceTTLSeconds）。
// 不成样子的条目、超范围的 ttl、表满时的新服号都以 ErrRangeInvalid 拒绝且不改动。不核对服是否登记过：
// 开发环境常常先起 game、后 upsert-server，登记表里多一条没登记的服无害（入口只列 account 登记过的服）。
func (s *Service) ReportServerPresence(ctx context.Context, presence ServerPresence, ttlSeconds int64) error {
	checked, err := presence.validate()
	if err != nil {
		return err
	}
	if ttlSeconds <= 0 || ttlSeconds > MaxPresenceTTLSeconds {
		return fmt.Errorf("%w: server %d presence ttl %ds is outside 1..%ds", ErrRangeInvalid, presence.ServerID, ttlSeconds, MaxPresenceTTLSeconds)
	}
	return s.presence.Put(ctx, checked, time.Duration(ttlSeconds)*time.Second)
}

// WithdrawServerPresence 立刻撤掉一个服的入口条目（game 正常停机时调，不用等过期）。没有登记过不是错误。
func (s *Service) WithdrawServerPresence(ctx context.Context, serverID int32) error {
	if serverID <= 0 {
		return fmt.Errorf("%w: server id %d must be positive", ErrRangeInvalid, serverID)
	}
	return s.presence.Delete(ctx, serverID)
}

// ListServerPresence 返回没过期的入口条目，按服号升序，至多 MaxServers 条。只读；与 ListServers 一样是
// 登录前公开的列表信息，不需要调用方身份。存储里坏掉的条目（解不开、不成样子）跳过，不让整张清单失败。
func (s *Service) ListServerPresence(ctx context.Context) ([]ServerPresence, error) {
	raw, err := s.presence.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ServerPresence, 0, len(raw))
	seen := map[int32]bool{}
	for _, presence := range raw {
		checked, err := presence.validate()
		if err != nil || seen[checked.ServerID] {
			continue
		}
		seen[checked.ServerID] = true
		out = append(out, checked)
	}
	slices.SortFunc(out, func(a, b ServerPresence) int { return int(a.ServerID) - int(b.ServerID) })
	if len(out) > MaxServers {
		out = out[:MaxServers]
	}
	return out, nil
}

// MemoryPresenceStore 是进程内的登记表：单个 account 进程、测试用。多个 account 副本时每个副本各有一张，
// 上报只投给其中一个，所以生产装配（wiring/account 的 Mod）用 Redis 的那一种（RedisStores.Presence）。
type MemoryPresenceStore struct {
	now func() time.Time

	mu      sync.Mutex
	entries map[int32]presenceRecord
}

type presenceRecord struct {
	presence  ServerPresence
	expiresAt time.Time
}

// NewMemoryPresenceStore 返回空的进程内登记表。now 是判断过期用的时钟，nil 取 time.Now（系统时间）。
func NewMemoryPresenceStore(now func() time.Time) *MemoryPresenceStore {
	if now == nil {
		now = time.Now
	}
	return &MemoryPresenceStore{now: now, entries: map[int32]presenceRecord{}}
}

// Put 实现 PresenceStore。
func (store *MemoryPresenceStore) Put(_ context.Context, presence ServerPresence, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("%w: presence ttl must be positive", ErrRangeInvalid)
	}
	now := store.now()
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, known := store.entries[presence.ServerID]; !known {
		store.dropExpired(now)
		if len(store.entries) >= MaxServers {
			return fmt.Errorf("%w: %d servers are present, which is the limit; server %d was not added", ErrRangeInvalid, MaxServers, presence.ServerID)
		}
	}
	store.entries[presence.ServerID] = presenceRecord{presence: presence.clone(), expiresAt: now.Add(ttl)}
	return nil
}

// Delete 实现 PresenceStore。
func (store *MemoryPresenceStore) Delete(_ context.Context, serverID int32) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	delete(store.entries, serverID)
	return nil
}

// List 实现 PresenceStore。到期的那一刻起不再给出。
func (store *MemoryPresenceStore) List(context.Context) ([]ServerPresence, error) {
	now := store.now()
	store.mu.Lock()
	defer store.mu.Unlock()
	store.dropExpired(now)
	out := make([]ServerPresence, 0, len(store.entries))
	for _, record := range store.entries {
		out = append(out, record.presence.clone())
	}
	return out, nil
}

// dropExpired 删掉到期的条目。调用方持有锁。
func (store *MemoryPresenceStore) dropExpired(now time.Time) {
	for id, record := range store.entries {
		if !now.Before(record.expiresAt) {
			delete(store.entries, id)
		}
	}
}

var errPresenceStoreBroken = errors.New("account: presence store returned an unreadable entry")

var _ PresenceStore = (*MemoryPresenceStore)(nil)
