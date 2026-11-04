// 路由 RPC：缓存 → 查找接口，改投一次，no responders 后重新查找一次，四类答复的判定。

package servicerpc

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

type routedResponse struct {
	Code   int32
	Reason string
	Owner  int32
	Value  string
}

func (r *routedResponse) StatusCode() int32    { return r.Code }
func (r *routedResponse) StatusReason() string { return r.Reason }
func (r *routedResponse) RedirectSID() int32   { return r.Owner }

// routedBus 按 sid 给出固定答复，记下每次 CallTo 的目标。
type routedBus struct {
	bus.IBus
	answers map[int32]routedResponse
	errs    map[int32]error
	called  []int32
}

func (b *routedBus) CallTo(_ context.Context, _ string, sid int32, _ string, _ any, resp any) error {
	b.called = append(b.called, sid)
	if err := b.errs[sid]; err != nil {
		return err
	}
	answer, ok := b.answers[sid]
	if !ok {
		return fmt.Errorf("%w: sid %d", fnats.ErrNoResponders, sid)
	}
	out := resp.(*routedResponse)
	// 像 omitempty 编码那样只写非零字段：残留字段必须由调用方在改投前清掉。
	if answer.Code != 0 {
		out.Code = answer.Code
	}
	if answer.Reason != "" {
		out.Reason = answer.Reason
	}
	if answer.Owner != 0 {
		out.Owner = answer.Owner
	}
	if answer.Value != "" {
		out.Value = answer.Value
	}
	return nil
}

// mapResolver 是查找接口的替身：owners 里没有的实体答 EntityNotFound，err 非空时答查找失败；记下每次查找。
type mapResolver struct {
	owners  map[int64]int32
	err     error
	lookups int
}

func (r *mapResolver) LookupRoute(_ context.Context, id int64) (int32, error) {
	r.lookups++
	if r.err != nil {
		return 0, r.err
	}
	sid, ok := r.owners[id]
	if !ok {
		return 0, EntityNotFound(id)
	}
	return sid, nil
}

const routedAlliance = int64(5<<10 | 7) // 完整 Entity ID：kind 在低位

func TestCallRoutedCheckedRedirectsOnceAndCachesTheNamedOwner(t *testing.T) {
	ctx := context.Background()
	// 路由登记还指向旧 owner 3000，它已把联盟转给 3001：答复改投，改投一次成功，响应里没有第一次答复的残留。
	b := &routedBus{answers: map[int32]routedResponse{
		3000: {Code: CodeRedirect, Reason: "not owner", Owner: 3001},
		3001: {Value: "joined"},
	}}
	resolver := &mapResolver{owners: map[int64]int32{routedAlliance: 3000}}
	client := NewBusClient(b, "alliance", 0)
	var resp routedResponse
	if err := client.CallRoutedChecked(ctx, resolver, routedAlliance, "Join", struct{}{}, &resp, "join"); err != nil {
		t.Fatalf("redirected call: %v", err)
	}
	if fmt.Sprint(b.called) != "[3000 3001]" || resp.Value != "joined" || resp.Code != 0 || resp.Owner != 0 {
		t.Fatalf("called=%v resp=%+v, want [3000 3001] and a clean success", b.called, resp)
	}
	// 改投目标进了缓存：下一次直接发给 3001，不再查找。
	resp = routedResponse{}
	if err := client.CallRoutedChecked(ctx, resolver, routedAlliance, "Join", struct{}{}, &resp, "join"); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(b.called) != "[3000 3001 3001]" || resolver.lookups != 1 {
		t.Fatalf("called=%v lookups=%d; want the cached redirect target and a single lookup", b.called, resolver.lookups)
	}

	// 改投目标也说不是 owner（迁移中）：只改投一次，结论是可重试、未执行。
	b = &routedBus{answers: map[int32]routedResponse{
		3000: {Code: CodeRedirect, Owner: 3001},
		3001: {Code: CodeRedirect, Owner: 3000},
	}}
	client = NewBusClient(b, "alliance", 0)
	resp = routedResponse{}
	err := client.CallRoutedChecked(ctx, &mapResolver{owners: map[int64]int32{routedAlliance: 3000}}, routedAlliance, "Join", struct{}{}, &resp, "join")
	if Classify(err) != OutcomeRetryable || len(b.called) != 2 {
		t.Fatalf("redirect loop: err=%v outcome=%s called=%v, want retryable after exactly two calls", err, Classify(err), b.called)
	}

	// 业务拒绝不改投。
	b = &routedBus{answers: map[int32]routedResponse{3000: {Code: 100001, Reason: "alliance full"}}}
	client = NewBusClient(b, "alliance", 0)
	resp = routedResponse{}
	err = client.CallRoutedChecked(ctx, &mapResolver{owners: map[int64]int32{routedAlliance: 3000}}, routedAlliance, "Join", struct{}{}, &resp, "join")
	if Classify(err) != OutcomeRejected || fmt.Sprint(b.called) != "[3000]" {
		t.Fatalf("business rejection: err=%v outcome=%s called=%v", err, Classify(err), b.called)
	}
}

// 缩容 / owner 下线后缓存仍指向旧 sid：no responders 后清缓存、重新查找一次，查到新 owner 就在同一份预算里改投过去。
func TestCallRoutedCheckedRelooksUpAfterNoRespondersAndReachesTheNewOwner(t *testing.T) {
	ctx := context.Background()
	b := &routedBus{answers: map[int32]routedResponse{3000: {Value: "old"}, 3001: {Value: "new"}}}
	resolver := &mapResolver{owners: map[int64]int32{routedAlliance: 3000}}
	client := NewBusClient(b, "alliance", 0)
	var resp routedResponse
	if err := client.CallRoutedChecked(ctx, resolver, routedAlliance, "Join", struct{}{}, &resp, "join"); err != nil || resp.Value != "old" {
		t.Fatalf("first call resp=%+v err=%v", resp, err)
	}
	// 3000 下线（实体已转给 3001，登记也改了），本客户端的缓存还指向 3000。
	delete(b.answers, 3000)
	resolver.owners[routedAlliance] = 3001
	resp = routedResponse{}
	if err := client.CallRoutedChecked(ctx, resolver, routedAlliance, "Join", struct{}{}, &resp, "join"); err != nil || resp.Value != "new" {
		t.Fatalf("after the owner left: resp=%+v err=%v called=%v; want the call re-looked-up and sent to 3001", resp, err, b.called)
	}
	if fmt.Sprint(b.called) != "[3000 3000 3001]" || resolver.lookups != 2 {
		t.Fatalf("called=%v lookups=%d; want one stale attempt, one re-lookup and one send to the new owner", b.called, resolver.lookups)
	}

	// 重新查找仍是同一个下线的 sid（死掉的 owner 还没被拉起）：不再发第二次，按可重试、未执行返回。
	b = &routedBus{answers: map[int32]routedResponse{}}
	resolver = &mapResolver{owners: map[int64]int32{routedAlliance: 3000}}
	client = NewBusClient(b, "alliance", 0)
	err := client.CallRoutedChecked(ctx, resolver, routedAlliance, "Join", struct{}{}, &routedResponse{}, "join")
	if !errors.Is(err, fnats.ErrNoResponders) || Classify(err) != OutcomeRetryable || fmt.Sprint(b.called) != "[3000]" || resolver.lookups != 2 {
		t.Fatalf("dead owner: err=%v called=%v lookups=%d; want no responders after one send and one re-lookup", err, b.called, resolver.lookups)
	}
	if client.routes.len() != 0 {
		t.Fatalf("a route answered with no responders stays cached (%d entries)", client.routes.len())
	}
}

// 查找接口答“实体不存在”时请求不发出去（业务拒绝，不负缓存：之后建好的实体立刻可见）；查找失败按可重试返回。
func TestCallRoutedCheckedDoesNotSendWithoutAnOwner(t *testing.T) {
	ctx := context.Background()
	b := &routedBus{answers: map[int32]routedResponse{3000: {Value: "ok"}}}
	resolver := &mapResolver{owners: map[int64]int32{}}
	client := NewBusClient(b, "alliance", 0)
	err := client.CallRoutedChecked(ctx, resolver, routedAlliance, "Join", struct{}{}, &routedResponse{}, "join")
	if Classify(err) != OutcomeRejected || errcode.CodeOf(err) != errcode.CodeEntityNotFound || len(b.called) != 0 {
		t.Fatalf("missing entity: err=%v outcome=%s called=%v; want entity-not-found without a send", err, Classify(err), b.called)
	}
	resolver.owners[routedAlliance] = 3000
	if err := client.CallRoutedChecked(ctx, resolver, routedAlliance, "Join", struct{}{}, &routedResponse{}, "join"); err != nil {
		t.Fatalf("an entity created right after a miss must be found at once: %v", err)
	}

	resolver = &mapResolver{err: errors.New("redis and mongo unreachable")}
	err = NewBusClient(b, "alliance", 0).CallRoutedChecked(ctx, resolver, routedAlliance+1, "Join", struct{}{}, &routedResponse{}, "join")
	if Classify(err) != OutcomeRetryable {
		t.Fatalf("lookup failure: err=%v outcome=%s; want retryable (nothing was sent)", err, Classify(err))
	}
}

func TestRouteCacheExpiresAndEvictsTheLeastRecentlyUsed(t *testing.T) {
	now := time.Unix(1000, 0)
	cache := newRouteCache(30*time.Second, 2)
	cache.now = func() time.Time { return now }
	cache.put(1, 3000)
	cache.put(2, 3001)
	if _, ok := cache.get(1); !ok { // 1 变成最近用过
		t.Fatal("entry 1 missing")
	}
	cache.put(3, 3002) // 淘汰最久没用的 2
	if _, ok := cache.get(2); ok || cache.len() != 2 {
		t.Fatalf("entry 2 survived eviction (len=%d)", cache.len())
	}
	now = now.Add(30 * time.Second)
	if _, ok := cache.get(1); ok {
		t.Fatal("an entry is served at its expiry")
	}
	cache.put(4, 3003)
	cache.forget(4, 3000) // 已改写成别的 sid 的条目不动
	if sid, ok := cache.get(4); !ok || sid != 3003 {
		t.Fatalf("forget with a stale sid removed the entry: %d %v", sid, ok)
	}
}

// 服务端答复：Redirect 给出 CodeRedirect 与 owner，Retryable 给出 CodeRetryable；cause 仍可 errors.Is。
func TestServerAnswersCarryCodeAndOwner(t *testing.T) {
	cause := errors.New("writer fenced")
	answer := Redirect(3001, cause)
	if code := Code(answer); code != CodeRedirect {
		t.Fatalf("Redirect code = %d, want %d", code, CodeRedirect)
	}
	if owner, ok := RedirectSID(answer); !ok || owner != 3001 || !errors.Is(answer, cause) {
		t.Fatalf("RedirectSID = %d,%v; errors.Is(cause)=%v", owner, ok, errors.Is(answer, cause))
	}
	// 不知道 owner 时退化为可重试。
	if unknown := Redirect(0, cause); Code(unknown) != CodeRetryable {
		t.Fatalf("Redirect(0) code = %d, want retryable", Code(unknown))
	}
	if retry := Retryable(cause); Code(retry) != CodeRetryable || !errors.Is(retry, cause) {
		t.Fatalf("Retryable code = %d", Code(retry))
	}
}

func TestClassifySeparatesNotExecutedFromUnknown(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want Outcome
	}{
		{"success", nil, OutcomeOK},
		{"no responders after retries", fmt.Errorf("rpc: x failed after 3 attempts: %w", fnats.ErrNoResponders), OutcomeRetryable},
		{"server retryable", Check(CodeRetryable, "recovering", ""), OutcomeRetryable},
		{"redirect not followed", Check(CodeRedirect, "not owner", ""), OutcomeRetryable},
		{"business rejection", Check(100001, "alliance full", ""), OutcomeRejected},
		{"entity not found", errcode.Wrap(errcode.ErrEntityNotFound, nil), OutcomeRejected},
		{"timeout", fmt.Errorf("%w: %w", fnats.ErrTimeout, context.DeadlineExceeded), OutcomeUnknown},
		{"caller deadline", context.DeadlineExceeded, OutcomeUnknown},
		{"server internal", Check(errcode.CodeInternal, "server error", ""), OutcomeUnknown},
		{"transport decode", errors.New("bus: decode rpc response envelope: eof"), OutcomeUnknown},
	} {
		if got := Classify(tc.err); got != tc.want {
			t.Errorf("%s: Classify(%v) = %s, want %s", tc.name, tc.err, got, tc.want)
		}
	}
}
