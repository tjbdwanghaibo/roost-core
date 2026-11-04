// 路由 RPC（联盟服务方案 §2.3、§2.7）：目标实体的 owner 只由 Remote 的持久所有权记录决定；owner 加载实体时在 Redis
// 登记一份带过期的“实体 ID → sid + 所有权代际”，调用方按本地缓存 → 查找接口（RouteResolver：Redis 登记 → 所有权
// 记录）找到 owner 再 CallTo。查找结果只是路由提示：服务端发现自己不是 owner 时答复“改投”并带它所知的 owner sid，
// 客户端改投一次；no responders（owner 下线、缩容后的旧缓存）时清掉缓存重新查找一次。四类答复（成功 / 业务拒绝 /
// 可重试未执行 / 结果未知）由 Classify 判定。
//
// 现有 NATS 轻量 RPC 客户端的自动重试（infra/network/nats/driver/rpc.go 的 RPCClient.Call）：
//   - no responders（目标 sid 没有订阅这个方法：进程不在、启动恢复中 handler 还没注册）按 nats.rpc 的重试策略
//     退避重试，直到 MaxAttempts 或调用期限；全部落空时错误 errors.Is 到 nats.ErrNoResponders——这个请求一次也没送到，
//     按“可重试，未执行”。重试期间目标恢复，请求可能就在这次调用里被执行。
//   - 超时：BusClient 总给调用带期限（NewBusClient 的 timeout），单次尝试用满整个期限，期限到就不再重试，所以同一次
//     调用不会因为超时被重发；但调用方自己的重试、JetStream RPC 的重投仍可能让同一请求送达多次。
//
// 所以写 RPC 必须按 RequestID 幂等，超时一律按“结果未知”处理：用同一个 RequestID 查结果或原样重发，直到得到
// 成功或业务拒绝。
package servicerpc

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

const (
	// CodeRetryable：请求没有执行，用同一 RequestID 重试（见 errcode.CodeRetryable）。
	CodeRetryable = errcode.CodeRetryable
	// CodeRedirect：本实例不是 owner，响应的 RedirectSID 是改投目标（见 errcode.CodeRedirect）。
	CodeRedirect = errcode.CodeRedirect
)

// RedirectProvider 是“改投”答复携带 owner sid 的方式：响应结构体加一个字段并实现它。
type RedirectProvider interface {
	RedirectSID() int32
}

// redirectError 是服务端的“改投”答复：CodeRedirect 加它所知的 owner sid。
type redirectError struct {
	owner int32
	coded error // errcode.Wrap(ErrRedirect, cause)：ClientError 据它给出 CodeRedirect，errors.Is 仍能判别 cause
}

func (e *redirectError) Error() string {
	return fmt.Sprintf("servicerpc: not the owner, redirect to sid %d: %v", e.owner, e.coded)
}

func (e *redirectError) Unwrap() error { return e.coded }

// Redirect 是服务端发现自己不是目标实体 owner 时的答复：Error(err) 给出 CodeRedirect，RedirectSID(err) 给出
// 写进响应 RedirectSID 字段的 owner。ownerSID 非正（本实例也不知道 owner）时退化为 Retryable。
func Redirect(ownerSID int32, cause error) error {
	if ownerSID <= 0 {
		return Retryable(cause)
	}
	return &redirectError{owner: ownerSID, coded: errcode.Wrap(errcode.ErrRedirect, cause)}
}

// RedirectSID 取出 Redirect 答复里的 owner sid。
func RedirectSID(err error) (int32, bool) {
	var redirect *redirectError
	if errors.As(err, &redirect) && redirect.owner > 0 {
		return redirect.owner, true
	}
	return 0, false
}

// Retryable 是服务端确认请求没有执行时的答复（CodeRetryable）：恢复中、所有权转移中、实体重载中。
func Retryable(cause error) error { return errcode.Wrap(errcode.ErrRetryable, cause) }

// Outcome 是一次写 RPC 的结论类别，决定调用方怎么显示、要不要重试。
type Outcome uint8

const (
	// OutcomeOK：成功，响应里是新状态。
	OutcomeOK Outcome = iota
	// OutcomeRejected：服务端执行了判定并拒绝（业务错误码），不重试。
	OutcomeRejected
	// OutcomeRetryable：确定没有执行——no responders、CodeRetryable、改投后仍不是 owner。用同一 RequestID 重试。
	OutcomeRetryable
	// OutcomeUnknown：可能执行了也可能没有——超时、取消、传输或解码错误、CodeInternal。用同一 RequestID
	// 查结果或原样重发，直到得到成功或拒绝。拿不准的错误都归这一类。
	OutcomeUnknown
)

func (o Outcome) String() string {
	switch o {
	case OutcomeOK:
		return "ok"
	case OutcomeRejected:
		return "rejected"
	case OutcomeRetryable:
		return "retryable"
	default:
		return "unknown"
	}
}

// Classify 把 Call / CallChecked / CallRoutedChecked 的错误归到四类答复之一。
func Classify(err error) Outcome {
	if err == nil {
		return OutcomeOK
	}
	if errors.Is(err, fnats.ErrNoResponders) {
		return OutcomeRetryable
	}
	if errors.Is(err, fnats.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return OutcomeUnknown
	}
	var coded *errcode.IntError
	if !errors.As(err, &coded) {
		return OutcomeUnknown
	}
	switch code := errcode.CodeOf(err); {
	case code == CodeRetryable || code == CodeRedirect:
		return OutcomeRetryable
	case code == errcode.CodeInternal:
		return OutcomeUnknown
	default:
		return OutcomeRejected
	}
}

// redirectTarget 判断响应是不是带了 sid 的改投答复。
func redirectTarget(err error, resp any) (int32, bool) {
	if err == nil || errcode.CodeOf(err) != CodeRedirect {
		return 0, false
	}
	provider, ok := resp.(RedirectProvider)
	if !ok {
		return 0, false
	}
	sid := provider.RedirectSID()
	return sid, sid > 0
}

// resetResponse 把 *T 的响应清零。
func resetResponse(resp any) error {
	value := reflect.ValueOf(resp)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return fmt.Errorf("servicerpc: routed response must be a non-nil pointer, got %T", resp)
	}
	value.Elem().SetZero()
	return nil
}

// RouteResolver 是路由 RPC 的查找接口：给出路由键（完整 Entity ID，kind 在 ID 里）当前 owner 的 sid。实现在 Remote
// 一侧（wiring/remoteentity.RouteSource）：先读 Redis 路由登记，没有就读持久所有权记录；都没有时返回
// EntityNotFound(entityID)（errcode.CodeEntityNotFound，Classify 为业务拒绝），请求不发出去。其他错误表示没查到
// （Redis 与所有权记录都读不了），CallRoutedChecked 按“可重试，未执行”返回。
//
// 结果只是路由提示：迁移、登记过期时它可能指向旧 owner，由服务端的改投答复纠正。实现不做缓存——缓存在
// BusClient 里，改投与 no responders 要更新它。
type RouteResolver interface {
	LookupRoute(ctx context.Context, entityID int64) (int32, error)
}

// EntityNotFound 是“这个实体没有所有权记录”的答复：查找接口找不到 owner、服务端前置检查读不到所有权记录时都用它。
// 非创建类请求不得认领不存在的实体，所以调用方得到业务拒绝（errcode.CodeEntityNotFound），不重试。
func EntityNotFound(entityID int64) error {
	return errcode.Wrap(errcode.ErrEntityNotFound, fmt.Errorf("servicerpc: entity %d has no ownership record", entityID))
}

const (
	// RouteCacheTTL 是路由缓存条目的有效期，与 Redis 路由登记的 TTL 相同。缓存只是提示，正确性靠改投，所以有效期长
	// 一点只是偶尔多一跳。
	RouteCacheTTL = 30 * time.Second
	// RouteCacheCapacity 是每个 BusClient 路由缓存的条数上限，满了按最近最少使用淘汰。
	RouteCacheCapacity = 65536
)

// routeCache 是 BusClient 的路由缓存：实体 ID → owner sid，带有效期，LRU 限条数。不做“实体不存在”的负缓存：刚建好的
// 实体会被别的进程立刻访问，负缓存会让它们在有效期内看不见它；查一个不存在的实体本来只多一次所有权记录点读。
type routeCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	capacity int
	entries  map[int64]*list.Element
	order    list.List // 前面是最近用过的
	now      func() time.Time
}

type routeCacheEntry struct {
	id      int64
	sid     int32
	expires time.Time
}

func newRouteCache(ttl time.Duration, capacity int) *routeCache {
	return &routeCache{ttl: ttl, capacity: capacity, entries: make(map[int64]*list.Element), now: time.Now}
}

// get 返回未过期的缓存 sid；过期条目顺手删掉。
func (c *routeCache) get(id int64) (int32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[id]
	if !ok {
		return 0, false
	}
	entry := element.Value.(*routeCacheEntry)
	if !c.now().Before(entry.expires) {
		c.order.Remove(element)
		delete(c.entries, id)
		return 0, false
	}
	c.order.MoveToFront(element)
	return entry.sid, true
}

// put 记下（或改写）id 的 owner，有效期从现在算。
func (c *routeCache) put(id int64, sid int32) {
	if sid <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	expires := c.now().Add(c.ttl)
	if element, ok := c.entries[id]; ok {
		entry := element.Value.(*routeCacheEntry)
		entry.sid, entry.expires = sid, expires
		c.order.MoveToFront(element)
		return
	}
	c.entries[id] = c.order.PushFront(&routeCacheEntry{id: id, sid: sid, expires: expires})
	for c.capacity > 0 && len(c.entries) > c.capacity {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*routeCacheEntry).id)
	}
}

// forget 删掉 id 的条目；sid 非零时只在条目仍指向 sid 时删（并发调用已经改写成新 owner 的不动）。
func (c *routeCache) forget(id int64, sid int32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[id]
	if !ok || (sid != 0 && element.Value.(*routeCacheEntry).sid != sid) {
		return
	}
	c.order.Remove(element)
	delete(c.entries, id)
}

// len 返回条目数（含未清理的过期条目），测试用。
func (c *routeCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// CallRoutedChecked 是路由 RPC 的客户端（联盟服务方案 §2.7）：按 entityID（完整 Entity ID）找到 owner，CallTo 它并
// 检查响应状态。查找顺序是本客户端的路由缓存 → resolver；都只是路由提示，由服务端裁决：
//
//   - 服务端答复改投（CodeRedirect，响应实现 RedirectProvider 给出另一个 sid）：缓存改成改投目标，改投一次。
//     改投后仍不对，或改投答复没带 sid，返回 CodeRedirect 错误（Classify 为可重试）。
//   - no responders（目标 sid 没有这个方法的订阅：owner 下线、缩容后的旧缓存、恢复中；NATS 客户端已按策略重试过）：
//     删掉这条缓存，经 resolver 重新查一次，查到不同的 sid 才再发一次，否则原样返回（可重试，未执行）。
//   - resolver 答复 EntityNotFound：原样返回（业务拒绝），请求不发出去；resolver 的其他错误按 Retryable 返回。
//
// 请求带实体 ID 作顺序键（bus.WithOrderKey）：owner 上同一实体的请求按到达顺序逐个进 handler，不同实体并行。
//
// 全部尝试共用 BusClient 的一份 timeout 预算。必须在 I/O 段调用（与 Call 相同，快池上返回 fctx 的阻塞错误）。
// resp 必须是指向响应结构体的指针：再次发送前清零，避免上一次响应的字段（改投码、owner sid）残留到结果里。
func (c *BusClient) CallRoutedChecked(ctx context.Context, resolver RouteResolver, entityID int64, method string, req any, resp any, fallback string) error {
	if err := fctx.BlockingError("servicerpc.CallRoutedChecked"); err != nil {
		return err
	}
	if c == nil || c.bus == nil {
		return ErrBusNil
	}
	if resolver == nil {
		return fmt.Errorf("servicerpc: route resolver is nil for %s", c.serviceType)
	}
	if entityID == 0 {
		return errcode.Wrap(errcode.ErrEntityNotFound, errors.New("servicerpc: routed call without an entity id"))
	}
	// 顺序键是实体 ID：owner 上同一实体的请求按到达顺序逐个处理，不同实体并行（bus.WithOrderKey）。
	ctx = bus.WithOrderKey(ctx, strconv.FormatInt(entityID, 10))
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	target, err := c.lookupRoute(callCtx, resolver, entityID)
	if err != nil {
		return err
	}
	err = c.CallChecked(callCtx, target, method, req, resp, fallback)
	next := int32(0)
	switch {
	case err == nil:
		return nil
	case errcode.CodeOf(err) == errcode.CodeEntityNotFound:
		// 服务端前置检查读不到所有权记录：实体不存在（或已删除），缓存不再指向任何人。
		c.routes.forget(entityID, target)
		return err
	case errors.Is(err, fnats.ErrNoResponders):
		// 目标 sid 没有这个方法的订阅：缓存过期指向的旧 owner、缩容下线的实例或还在恢复的 owner。重新查一次
		// （缓存已删，直接问 resolver），查到别的 sid 才再发，否则按可重试、未执行返回。
		c.routes.forget(entityID, target)
		metrics.IncCounter("servicerpc_route_relookup_total", metrics.Labels{"service": c.serviceType}, 1)
		fresh, lookupErr := resolver.LookupRoute(callCtx, entityID)
		if lookupErr != nil || fresh == target {
			return err
		}
		c.routes.put(entityID, fresh)
		next = fresh
	default:
		redirect, ok := redirectTarget(err, resp)
		if !ok || redirect == target {
			return err
		}
		// 服务端读了持久所有权记录，它说的 owner 比缓存与 Redis 登记新。
		c.routes.put(entityID, redirect)
		metrics.IncCounter("servicerpc_route_redirect_total", metrics.Labels{"service": c.serviceType}, 1)
		next = redirect
	}
	if resetErr := resetResponse(resp); resetErr != nil {
		return errors.Join(err, resetErr)
	}
	return c.CallChecked(callCtx, next, method, req, resp, fallback)
}

// lookupRoute 先查缓存，没有再问 resolver 并写进缓存。resolver 的“实体不存在”原样返回；其他查找错误表示请求
// 还没发出去，按 Retryable 返回。
func (c *BusClient) lookupRoute(ctx context.Context, resolver RouteResolver, entityID int64) (int32, error) {
	if sid, ok := c.routes.get(entityID); ok {
		metrics.IncCounter("servicerpc_route_lookup_total", metrics.Labels{"service": c.serviceType, "source": "cache"}, 1)
		return sid, nil
	}
	metrics.IncCounter("servicerpc_route_lookup_total", metrics.Labels{"service": c.serviceType, "source": "resolver"}, 1)
	sid, err := resolver.LookupRoute(ctx, entityID)
	switch {
	case errcode.CodeOf(err) == errcode.CodeEntityNotFound:
		return 0, err
	case err != nil:
		return 0, Retryable(fmt.Errorf("servicerpc: look up the owner of entity %d: %w", entityID, err))
	case sid <= 0:
		return 0, Retryable(fmt.Errorf("servicerpc: route resolver returned sid %d for entity %d", sid, entityID))
	}
	c.routes.put(entityID, sid)
	return sid, nil
}
