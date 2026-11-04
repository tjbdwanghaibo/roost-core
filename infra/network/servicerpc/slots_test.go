// 按槽位路由：区间表加载时校验一次（覆盖完整、不重叠、sid 不重复），改投一次，四类答复的判定。

package servicerpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
)

func TestSlotTableRejectsGapsOverlapsAndDuplicateSids(t *testing.T) {
	for _, tc := range []struct {
		name  string
		slots int
		specs []string
		want  string
	}{
		{"gap", 256, []string{"3000:0-127", "3001:129-255"}, "slots 128 are not assigned"},
		{"tail gap", 256, []string{"3000:0-200"}, "slots 201-255 are not assigned"},
		{"overlap", 256, []string{"3000:0-128", "3001:128-255"}, "overlaps sid 3000 at slot 128"},
		{"duplicate sid", 256, []string{"3000:0-127", "3000:128-255"}, "sid 3000 appears in two ranges"},
		{"outside", 256, []string{"3000:0-256"}, "must lie within 0-255"},
		{"reversed", 256, []string{"3000:10-0", "3001:0-255"}, "first <= last"},
		{"zero sid", 4, []string{"0:0-3"}, "sid must be positive"},
		{"no slots", 0, []string{"3000:0"}, "slot count 0"},
		{"malformed", 4, []string{"3000-0-3"}, "want sid:first-last"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseSlotTable(tc.slots, tc.specs); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseSlotTable(%d, %q) err = %v, want %q", tc.slots, tc.specs, err, tc.want)
			}
		})
	}

	table, err := ParseSlotTable(256, []string{"3001:128-255", "3000:0-127"})
	if err != nil {
		t.Fatal(err)
	}
	// 联盟 ID = 序号 × slots + 槽位：同一槽位的不同序号落在同一个 sid。
	for _, tc := range []struct {
		key  int64
		sid  int32
		slot int
	}{{0, 3000, 0}, {127, 3000, 127}, {128, 3001, 128}, {7*256 + 200, 3001, 200}, {7*256 + 5, 3000, 5}, {-1, 3001, 255}} {
		if slot, sid := table.SlotOf(tc.key), table.OwnerOf(tc.key); slot != tc.slot || sid != tc.sid {
			t.Errorf("key %d: slot=%d sid=%d, want slot=%d sid=%d", tc.key, slot, sid, tc.slot, tc.sid)
		}
	}
}

// 声明一次、加载时校验一次：嵌进配置结构体的 SlotTableConfig 在解码时经 ValidateConfig 拒绝坏表，必填键缺失也拒绝。
func TestSlotTableConfigIsValidatedOnLoad(t *testing.T) {
	type routing struct {
		Alliance SlotTableConfig `config:"alliance"`
	}
	load := func(doc map[string]any) (routing, error) {
		var out routing
		return out, configschema.Decode(configschema.NewMapSource(doc), &out, false)
	}
	if _, err := load(map[string]any{"alliance": map[string]any{"slots": 256, "shards": []any{"3000:0-127"}}}); err == nil || !strings.Contains(err.Error(), "128-255 are not assigned") {
		t.Fatalf("incomplete table loaded: err = %v", err)
	}
	if _, err := load(map[string]any{}); err == nil || !strings.Contains(err.Error(), "alliance.slots") {
		t.Fatalf("missing slot table loaded: err = %v", err)
	}
	cfg, err := load(map[string]any{"alliance": map[string]any{"slots": 256, "shards": []any{"3000:0-255"}}})
	if err != nil {
		t.Fatal(err)
	}
	table, err := cfg.Alliance.Table()
	if err != nil || table.OwnerOf(12345) != 3000 {
		t.Fatalf("table=%v err=%v", table, err)
	}
}

type shardResponse struct {
	Code   int32
	Reason string
	Owner  int32
	Value  string
}

func (r *shardResponse) StatusCode() int32    { return r.Code }
func (r *shardResponse) StatusReason() string { return r.Reason }
func (r *shardResponse) RedirectSID() int32   { return r.Owner }

// shardBus 按 sid 给出固定答复，记下每次 CallTo 的目标。
type shardBus struct {
	bus.IBus
	answers map[int32]shardResponse
	errs    map[int32]error
	called  []int32
}

func (b *shardBus) CallTo(_ context.Context, _ string, sid int32, _ string, _ any, resp any) error {
	b.called = append(b.called, sid)
	if err := b.errs[sid]; err != nil {
		return err
	}
	answer, ok := b.answers[sid]
	if !ok {
		return fmt.Errorf("%w: sid %d", fnats.ErrNoResponders, sid)
	}
	out := resp.(*shardResponse)
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

func TestCallSlotCheckedRedirectsOnceToTheOwnerTheServerNames(t *testing.T) {
	table, err := ParseSlotTable(256, []string{"3000:0-127", "3001:128-255"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := int64(3*256 + 10) // 槽位 10，表上是 3000

	// 表过期：3000 已把这个联盟转给 3001，答复改投；改投一次成功，响应里没有第一次答复的残留。
	b := &shardBus{answers: map[int32]shardResponse{
		3000: {Code: CodeRedirect, Reason: "not owner", Owner: 3001},
		3001: {Value: "joined"},
	}}
	client := NewBusClient(b, "alliance", 0)
	var resp shardResponse
	if err := client.CallSlotChecked(ctx, table, key, "Join", struct{}{}, &resp, "join"); err != nil {
		t.Fatalf("redirected call: %v", err)
	}
	if fmt.Sprint(b.called) != "[3000 3001]" || resp.Value != "joined" || resp.Code != 0 || resp.Owner != 0 {
		t.Fatalf("called=%v resp=%+v, want [3000 3001] and a clean success", b.called, resp)
	}

	// 改投目标也说不是 owner（迁移中）：只改投一次，结论是可重试、未执行。
	b = &shardBus{answers: map[int32]shardResponse{
		3000: {Code: CodeRedirect, Owner: 3001},
		3001: {Code: CodeRedirect, Owner: 3000},
	}}
	client = NewBusClient(b, "alliance", 0)
	resp = shardResponse{}
	err = client.CallSlotChecked(ctx, table, key, "Join", struct{}{}, &resp, "join")
	if Classify(err) != OutcomeRetryable || len(b.called) != 2 {
		t.Fatalf("redirect loop: err=%v outcome=%s called=%v, want retryable after exactly two calls", err, Classify(err), b.called)
	}

	// 表正确时一次到位；业务拒绝不改投。
	b = &shardBus{answers: map[int32]shardResponse{3000: {Code: 100001, Reason: "alliance full"}}}
	client = NewBusClient(b, "alliance", 0)
	resp = shardResponse{}
	err = client.CallSlotChecked(ctx, table, key, "Join", struct{}{}, &resp, "join")
	if Classify(err) != OutcomeRejected || fmt.Sprint(b.called) != "[3000]" {
		t.Fatalf("business rejection: err=%v outcome=%s called=%v", err, Classify(err), b.called)
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
