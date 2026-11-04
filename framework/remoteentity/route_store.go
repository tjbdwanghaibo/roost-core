package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

// Redis 路由登记（联盟服务方案 §2.7）：owner 在实体加载完成后把“实体 ID → owner sid + 所有权代际”写进 Redis，带 TTL，
// 由 owner 批量续期，转移出去、失去所有权与正常停机时按值删除，kill -9 靠过期。它只是路由提示：调用方查不到或查到旧
// owner 时回退读持久所有权记录、由服务端改投纠正；写准入永远只看所有权记录与 fence，不读这里。
//
// 键是 "<部署前缀>:remote_entity:route:<完整 Entity ID>"，部署前缀沿用 remote_entity.snapshot_l2_key_prefix（为空时
// 没有前缀与冒号，和 L2 快照键同一规则），不带 hash tag：每个脚本只碰一个键，Redis Cluster 下按键自然分散。值是
// "<owner sid>:<MarkerEpoch>"；MarkerEpoch 是所有权记录每次所有权变更加一的代际，比较它就知道两条登记谁新，同 sid
// 崩溃重启时代际不变、登记值也不变。

const (
	// DefaultRouteTTL 是路由登记的 TTL。kill -9 后过期前的登记仍指向同一个 sid，同 sid 拉起后它本来就是对的；真正错的
	// 登记只来自“迁走后旧 owner 没删掉”，那时新 owner 带更新代际的登记会覆盖它。所以 TTL 主要用来回收没人续的键。
	DefaultRouteTTL = 30 * time.Second
	// DefaultRouteRenewInterval 是 owner 续期一遍的间隔：TTL 内能续 3 次，偶发一次失败不会掉登记。
	DefaultRouteRenewInterval = 10 * time.Second
	// DefaultRouteRenewBatch 是一次 pipeline 续期（或停机删除）的键数上限。
	DefaultRouteRenewBatch = 1000
)

// routeRegisterLua：键不存在，或已存的代际不比本次新，才 SET … PX；已存的更新就不写、返回 0（迟到的旧 owner 登记
// 盖不掉新 owner）。代际按十进制字符串比较（先比长度再比字典序），不经 Lua number，避免大于 2^53 时丢精度；已存值
// 解析不出代际时按更旧处理、覆盖它。
//
// KEYS[1] 路由键；ARGV[1] 登记值 "<sid>:<epoch>"；ARGV[2] 本次代际（十进制）；ARGV[3] TTL 毫秒。
const routeRegisterLua = `
local cur = redis.call("GET", KEYS[1])
if cur then
    local sep = string.find(cur, ":", 1, true)
    if sep then
        local stored = string.sub(cur, sep + 1)
        local mine = ARGV[2]
        if string.match(stored, "^%d+$") and (#stored > #mine or (#stored == #mine and stored > mine)) then
            return 0
        end
    end
end
redis.call("SET", KEYS[1], ARGV[1], "PX", ARGV[3])
return 1
`

// routeRenewLua：值与本次完全相同才 PEXPIRE，返回 1；键不存在返回 0；值不同返回 -1。续期不重建键：续期批次在转移
// 之前拼好、转移之后才执行时，不会把已删掉的旧登记写回去。
//
// KEYS[1] 路由键；ARGV[1] 登记值；ARGV[2] TTL 毫秒。
const routeRenewLua = `
local cur = redis.call("GET", KEYS[1])
if cur == false then
    return 0
end
if cur ~= ARGV[1] then
    return -1
end
redis.call("PEXPIRE", KEYS[1], ARGV[2])
return 1
`

// routeDeleteLua：值相同才 DEL，返回 1，否则返回 0。只删自己写的，旧进程删不掉新 owner 的登记。
//
// KEYS[1] 路由键；ARGV[1] 登记值。
const routeDeleteLua = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
    redis.call("DEL", KEYS[1])
    return 1
end
return 0
`

// RouteRenewResult 是一个键续期的结果。
type RouteRenewResult int8

const (
	// RouteRenewed：值相同，已续期。
	RouteRenewed RouteRenewResult = 1
	// RouteMissing：键不存在（过期、Redis 丢键，或曾长时间连不上）。调用方先确认本地所有权仍是本 sid，再走登记。
	RouteMissing RouteRenewResult = 0
	// RouteReplaced：值已不是本次的（新 owner 登记了更新的代际，或转移后删掉又被别人登记）。不再续这个键。
	RouteReplaced RouteRenewResult = -1
)

// RouteEntry 是一条路由登记：实体与它的登记值。
type RouteEntry struct {
	ID    int64
	Value string
}

// routeStore 是 Manager 用的路由登记存储（Redis 实现见 RouteStore；单元测试用 Go 替身）。批量方法的结果与 entries
// 一一对应；err 非空时 results 可能为空或部分有效（pipeline 的单条错误记在 results 外，由调用方当作“没续上”）。
type routeStore interface {
	Register(ctx context.Context, id int64, owner int32, epoch uint64) (written bool, err error)
	Renew(ctx context.Context, entries []RouteEntry) ([]RouteRenewResult, error)
	Delete(ctx context.Context, entries []RouteEntry) error
	Lookup(ctx context.Context, id int64) (owner int32, epoch uint64, found bool, err error)
}

// RouteStore 是 Redis 上的路由登记：登记 / 续期 / 删除三个按值比较的脚本与查找。只读进程（Mirror Mod）只用它查找。
type RouteStore struct {
	redis  fredis.IRedis
	prefix string
	ttl    time.Duration
}

// NewRouteStore 用部署前缀（remote_entity.snapshot_l2_key_prefix，可以为空）建路由登记存储。ttl ≤ 0 取 DefaultRouteTTL。
func NewRouteStore(redis fredis.IRedis, prefix string, ttl time.Duration) (*RouteStore, error) {
	if redis == nil {
		return nil, errors.New("remote_entity: route store requires a redis client")
	}
	if err := ValidateSnapshotL2KeyPrefix(prefix); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		ttl = DefaultRouteTTL
	}
	return &RouteStore{redis: redis, prefix: prefix, ttl: ttl}, nil
}

// RouteKey 返回实体的路由登记键。prefix 为空时是 "remote_entity:route:<id>"。
func RouteKey(prefix string, id int64) string {
	key := "remote_entity:route:" + strconv.FormatInt(id, 10)
	if prefix == "" {
		return key
	}
	return prefix + ":" + key
}

// RouteValue 返回登记值 "<owner sid>:<MarkerEpoch>"。
func RouteValue(owner int32, epoch uint64) string {
	return strconv.FormatInt(int64(owner), 10) + ":" + strconv.FormatUint(epoch, 10)
}

// ParseRouteValue 解析登记值；格式不对返回错误（查找方按没有登记处理，回退读所有权记录）。
func ParseRouteValue(raw string) (int32, uint64, error) {
	sidText, epochText, ok := strings.Cut(raw, ":")
	if !ok {
		return 0, 0, fmt.Errorf("remote_entity: route value %q: want <sid>:<epoch>", raw)
	}
	sid, err := strconv.ParseInt(sidText, 10, 32)
	if err != nil || sid <= 0 {
		return 0, 0, fmt.Errorf("remote_entity: route value %q: invalid sid", raw)
	}
	epoch, err := strconv.ParseUint(epochText, 10, 64)
	if err != nil || epoch == 0 {
		return 0, 0, fmt.Errorf("remote_entity: route value %q: invalid epoch", raw)
	}
	return int32(sid), epoch, nil
}

func (s *RouteStore) key(id int64) string { return RouteKey(s.prefix, id) }

// Register 执行登记脚本；written=false 表示 Redis 里已有更新代际的登记（本次是迟到的旧 owner）。
func (s *RouteStore) Register(ctx context.Context, id int64, owner int32, epoch uint64) (bool, error) {
	reply, err := s.redis.Eval(ctx, routeRegisterLua, []string{s.key(id)}, RouteValue(owner, epoch), strconv.FormatUint(epoch, 10), s.ttl.Milliseconds())
	if err != nil {
		return false, err
	}
	written, err := scriptInt(reply)
	return written == 1, err
}

// Renew 用 pipeline 一次续期 entries（调用方按 DefaultRouteRenewBatch 分批）。单条脚本出错时那一条的结果记为
// RouteMissing 之外的值无法判断，整体返回 Exec 的错误、results 为 nil。
func (s *RouteStore) Renew(ctx context.Context, entries []RouteEntry) ([]RouteRenewResult, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	pipe := s.redis.Pipeline()
	futures := make([]*fredis.FutureAny, len(entries))
	for i, entry := range entries {
		futures[i] = pipe.Eval(ctx, routeRenewLua, []string{s.key(entry.ID)}, entry.Value, s.ttl.Milliseconds())
	}
	if err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	results := make([]RouteRenewResult, len(entries))
	for i, future := range futures {
		reply, err := future.Result()
		if err != nil {
			return nil, err
		}
		code, err := scriptInt(reply)
		if err != nil {
			return nil, err
		}
		results[i] = RouteRenewResult(code)
	}
	return results, nil
}

// Delete 用 pipeline 按值删除 entries。
func (s *RouteStore) Delete(ctx context.Context, entries []RouteEntry) error {
	if len(entries) == 0 {
		return nil
	}
	pipe := s.redis.Pipeline()
	for _, entry := range entries {
		pipe.Eval(ctx, routeDeleteLua, []string{s.key(entry.ID)}, entry.Value)
	}
	return pipe.Exec(ctx)
}

// Lookup 读实体的登记。没有登记 found=false；值格式不对返回错误。
func (s *RouteStore) Lookup(ctx context.Context, id int64) (int32, uint64, bool, error) {
	raw, err := s.redis.Get(ctx, s.key(id))
	if errors.Is(err, fredis.ErrNil) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	owner, epoch, err := ParseRouteValue(string(raw))
	if err != nil {
		return 0, 0, false, err
	}
	return owner, epoch, true, nil
}

func scriptInt(reply any) (int64, error) {
	switch v := reply.(type) {
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	default:
		return 0, fmt.Errorf("remote_entity: unexpected route script reply %T", reply)
	}
}

// OwnershipReader 只读持久所有权记录。写 Manager 与 MongoOwnershipReader 都实现它；路由查找在 Redis 没有登记时用它。
type OwnershipReader interface {
	GetRemoteOwnership(ctx context.Context, id int64) (entity.RemoteEntityMarkerLease, bool, error)
}

// RouteLookup 是路由查找的 Remote 一侧：Redis 登记 → 持久所有权记录。wiring 把它接成 servicerpc.RouteResolver
// （wiring/remoteentity.RouteSource）。只读，不登记、不加载：读到 owner 就交给调用方发过去，owner 收到后冷加载并登记。
type RouteLookup struct {
	routes *RouteStore
	owners OwnershipReader
}

// NewRouteLookup 组合两层查找；owners 必需，routes 可以为 nil（只读所有权记录）。
func NewRouteLookup(routes *RouteStore, owners OwnershipReader) (*RouteLookup, error) {
	if owners == nil {
		return nil, errors.New("remote_entity: route lookup requires an ownership reader")
	}
	return &RouteLookup{routes: routes, owners: owners}, nil
}

// LookupOwner 返回实体当前 owner 的 sid（路由提示）。Redis 有登记就用它；没有、读不了或值坏了，就读持久所有权记录。
// 都没有时 found=false（实体不存在）。只有所有权记录也读不了才返回错误：Redis 不可用时路由退化为每次一次 Mongo 点读，
// 写准入不受影响。
func (l *RouteLookup) LookupOwner(ctx context.Context, id int64) (owner int32, found bool, err error) {
	if l.routes != nil {
		sid, _, ok, routeErr := l.routes.Lookup(ctx, id)
		switch {
		case routeErr != nil:
			metrics.IncCounter("remote_entity.route_lookup_fallback_total", metrics.Labels{"reason": "redis_error"}, 1)
		case ok:
			metrics.IncCounter("remote_entity.route_lookup_total", metrics.Labels{"source": "redis"}, 1)
			return sid, true, nil
		default:
			metrics.IncCounter("remote_entity.route_lookup_fallback_total", metrics.Labels{"reason": "redis_missing"}, 1)
		}
	}
	lease, ok, err := l.owners.GetRemoteOwnership(ctx, id)
	switch {
	case err != nil:
		metrics.IncCounter("remote_entity.route_lookup_total", metrics.Labels{"source": "error"}, 1)
		return 0, false, err
	case !ok || lease.OwnerSid == 0:
		metrics.IncCounter("remote_entity.route_lookup_total", metrics.Labels{"source": "not_found"}, 1)
		return 0, false, nil
	default:
		metrics.IncCounter("remote_entity.route_lookup_total", metrics.Labels{"source": "authority"}, 1)
		return lease.OwnerSid, true, nil
	}
}
