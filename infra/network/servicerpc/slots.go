// 按槽位路由的服务 RPC（联盟服务方案 §2.3、§2.7）：实体 ID 里编码槽位，槽位区间 → sid 的表只是路由提示，
// 真正的 owner 是 Remote 的持久所有权记录；服务端发现自己不是 owner 时答复“改投”并带它所知的 owner sid，
// 客户端改投一次。四类答复（成功 / 业务拒绝 / 可重试未执行 / 结果未知）由 Classify 判定。
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
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

// MaxSlots 是槽位总数的上限。槽位数编码在实体 ID 里、上线后不改，表按槽位逐个存 sid。
const MaxSlots = 65536

const (
	// CodeRetryable：请求没有执行，用同一 RequestID 重试（见 errcode.CodeRetryable）。
	CodeRetryable = errcode.CodeRetryable
	// CodeRedirect：本实例不是 owner，响应的 RedirectSID 是改投目标（见 errcode.CodeRedirect）。
	CodeRedirect = errcode.CodeRedirect
)

// SlotRange 是一段闭区间槽位 [First, Last] 归 Sid。
type SlotRange struct {
	Sid   int32
	First int
	Last  int
}

func (r SlotRange) String() string {
	if r.First == r.Last {
		return fmt.Sprintf("%d:%d", r.Sid, r.First)
	}
	return fmt.Sprintf("%d:%d-%d", r.Sid, r.First, r.Last)
}

// SlotTable 是“槽位 → sid”的路由表，构造时校验一次：槽位总数在 1..MaxSlots，区间在 [0, slots) 内、互不重叠、
// 覆盖全部槽位，sid 为正且不重复（一个实例只占一段连续区间）。构造后只读，可并发使用。
//
// 它只是路由提示：迁移、表过期时请求会落到不是 owner 的实例上，由服务端的改投答复纠正（CallSlotChecked）。
type SlotTable struct {
	slots  int
	owners []int32 // 下标是槽位
	ranges []SlotRange
}

// ParseSlotRange 解析 "sid:first-last" 或 "sid:slot"（例如 "3000:0-127"）。
func ParseSlotRange(spec string) (SlotRange, error) {
	sidText, slotsText, ok := strings.Cut(strings.TrimSpace(spec), ":")
	if !ok {
		return SlotRange{}, fmt.Errorf("servicerpc: slot range %q: want sid:first-last", spec)
	}
	sid, err := strconv.ParseInt(strings.TrimSpace(sidText), 10, 32)
	if err != nil {
		return SlotRange{}, fmt.Errorf("servicerpc: slot range %q: sid: %w", spec, err)
	}
	firstText, lastText, isRange := strings.Cut(slotsText, "-")
	first, err := strconv.Atoi(strings.TrimSpace(firstText))
	if err != nil {
		return SlotRange{}, fmt.Errorf("servicerpc: slot range %q: first slot: %w", spec, err)
	}
	last := first
	if isRange {
		if last, err = strconv.Atoi(strings.TrimSpace(lastText)); err != nil {
			return SlotRange{}, fmt.Errorf("servicerpc: slot range %q: last slot: %w", spec, err)
		}
	}
	return SlotRange{Sid: int32(sid), First: first, Last: last}, nil
}

// ParseSlotTable 解析配置里的区间表（每项 "sid:first-last"）并校验，见 NewSlotTable。
func ParseSlotTable(slots int, specs []string) (*SlotTable, error) {
	ranges := make([]SlotRange, 0, len(specs))
	var errs []error
	for _, spec := range specs {
		r, err := ParseSlotRange(spec)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		ranges = append(ranges, r)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return NewSlotTable(slots, ranges)
}

// NewSlotTable 校验并构造路由表；全部问题一次报出。
func NewSlotTable(slots int, ranges []SlotRange) (*SlotTable, error) {
	if slots <= 0 || slots > MaxSlots {
		return nil, fmt.Errorf("servicerpc: slot count %d must be between 1 and %d", slots, MaxSlots)
	}
	owners := make([]int32, slots)
	sids := make(map[int32]SlotRange, len(ranges))
	var errs []error
	for _, r := range ranges {
		if r.Sid <= 0 {
			errs = append(errs, fmt.Errorf("servicerpc: slot range %s: sid must be positive", r))
			continue
		}
		if r.First < 0 || r.Last < r.First || r.Last >= slots {
			errs = append(errs, fmt.Errorf("servicerpc: slot range %s must lie within 0-%d with first <= last", r, slots-1))
			continue
		}
		if previous, dup := sids[r.Sid]; dup {
			errs = append(errs, fmt.Errorf("servicerpc: sid %d appears in two ranges (%s and %s)", r.Sid, previous, r))
			continue
		}
		sids[r.Sid] = r
		for slot := r.First; slot <= r.Last; slot++ {
			if owners[slot] != 0 {
				errs = append(errs, fmt.Errorf("servicerpc: slot range %s overlaps sid %d at slot %d", r, owners[slot], slot))
				break
			}
			owners[slot] = r.Sid
		}
	}
	if missing := uncoveredSlots(owners); missing != "" && len(errs) == 0 {
		errs = append(errs, fmt.Errorf("servicerpc: slots %s are not assigned to any sid (the table must cover 0-%d)", missing, slots-1))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return &SlotTable{slots: slots, owners: owners, ranges: append([]SlotRange(nil), ranges...)}, nil
}

// uncoveredSlots 把没有 owner 的槽位写成 "a-b, c" 的形式；全部覆盖时为空。
func uncoveredSlots(owners []int32) string {
	var parts []string
	for slot := 0; slot < len(owners); slot++ {
		if owners[slot] != 0 {
			continue
		}
		end := slot
		for end+1 < len(owners) && owners[end+1] == 0 {
			end++
		}
		if end == slot {
			parts = append(parts, strconv.Itoa(slot))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", slot, end))
		}
		slot = end
	}
	return strings.Join(parts, ", ")
}

// Slots 返回槽位总数。
func (t *SlotTable) Slots() int { return t.slots }

// Ranges 返回构造时的区间（副本）。
func (t *SlotTable) Ranges() []SlotRange { return append([]SlotRange(nil), t.ranges...) }

// SlotOf 返回 key 的槽位：key 对槽位总数取模（负数取非负余数）。key 是编码了槽位的业务号（联盟 ID = 序号 ×
// 槽位总数 + 槽位，或玩家 ID），不是带 kind 位的完整实体 ID——后者先取 entity.ResolveEntityID(id).UniqueID。
func (t *SlotTable) SlotOf(key int64) int {
	n := int64(t.slots)
	return int(((key % n) + n) % n)
}

// OwnerOf 返回 key 所在槽位在表里的 sid。
func (t *SlotTable) OwnerOf(key int64) int32 { return t.owners[t.SlotOf(key)] }

// SlotTableConfig 是按槽位路由的服务在配置里的声明（规则声明一次）：嵌进 Mod 的配置结构体、加 section tag，
// app.LoadConfig 加载时经 ValidateConfig 校验一次，Table 取出路由表。调用方与服务端读同一个键，所有进程的
// slots 必须相同、上线后不改（编码在 ID 里）。
type SlotTableConfig struct {
	Slots  int      `config:"slots" required:"true" min:"1" max:"65536" example:"256" help:"槽位总数，编码在实体 ID 里（ID = 序号 × slots + 槽位）；全部进程相同，上线后不改"`
	Shards []string `config:"shards" required:"true" example:"[\"3000:0-255\"]" help:"槽位区间到实例 sid 的表（sid:first-last）。区间必须覆盖 0..slots-1、互不重叠，sid 不重复。\n只是路由提示：表过期时由服务端的改投答复纠正"`
}

// ValidateConfig 在加载时校验一次区间表。
func (c *SlotTableConfig) ValidateConfig(bool) error {
	_, err := ParseSlotTable(c.Slots, c.Shards)
	return err
}

// Table 返回校验过的路由表。
func (c SlotTableConfig) Table() (*SlotTable, error) { return ParseSlotTable(c.Slots, c.Shards) }

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

// Classify 把 Call / CallChecked / CallSlotChecked 的错误归到四类答复之一。
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

// CallSlotChecked 按 key 的槽位查表得到 sid，CallTo 它并检查响应状态；服务端答复改投（CodeRedirect，响应实现
// RedirectProvider 给出另一个 sid）时改投一次。改投后仍不对、或改投答复没带 sid，返回 CodeRedirect 错误（Classify
// 为 OutcomeRetryable）。两次尝试共用 BusClient 的一份 timeout 预算。必须在 I/O 段调用。
//
// resp 必须是指向响应结构体的指针：改投前清零，避免第一次响应里的字段（改投码、owner sid）残留到第二次解码结果里。
func (c *BusClient) CallSlotChecked(ctx context.Context, table *SlotTable, key int64, method string, req any, resp any, fallback string) error {
	if c == nil || c.bus == nil {
		return ErrBusNil
	}
	if table == nil {
		return fmt.Errorf("servicerpc: slot table is nil for %s", c.serviceType)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	target := table.OwnerOf(key)
	err := c.CallChecked(callCtx, target, method, req, resp, fallback)
	redirect, ok := redirectTarget(err, resp)
	if !ok || redirect == target {
		return err
	}
	if resetErr := resetResponse(resp); resetErr != nil {
		return errors.Join(err, resetErr)
	}
	return c.CallChecked(callCtx, redirect, method, req, resp, fallback)
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
		return fmt.Errorf("servicerpc: slot-routed response must be a non-nil pointer, got %T", resp)
	}
	value.Elem().SetZero()
	return nil
}
