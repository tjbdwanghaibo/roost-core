package heartbeat

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// Delivery 是心跳交给 Nest 的方式。缺省批量（DefaultDelivery）：进程 CPU 低于单目标，代价是同一快池上普通业务消息的
// 尾延迟略高（测量见 docs/performance/NEST-BATCH-HEARTBEAT-20261021.md）；单目标保留为可选项。
type Delivery string

const (
	// DeliveryDispatch 每拍一条单目标 nest.Client.Dispatch。
	DeliveryDispatch Delivery = "dispatch"
	// DeliveryBatch 按 64 拍一批交给 Nest 批量作业（nest.TryBatchDispatcher，设计见
	// docs/maintenance/FEATURE-2026-10-21-nest-batch-heartbeat.md）。Remote 托管实体的拍仍单目标 Dispatch。缺省。
	DeliveryBatch Delivery = "batch"
	// DefaultDelivery 是空值（Config.Delivery 未设、配置值为空串）对应的投递方式；wiring/heartbeat 的
	// heartbeat.delivery 声明缺省与它相同。批量方式要求 Bind 的客户端实现 nest.TryBatchDispatcher（*nest.NestMgr 实现；
	// 只实现 nest.Client 的包装要显式设 DeliveryDispatch）。
	DefaultDelivery = DeliveryBatch
)

// ParseDelivery 解析配置值 dispatch / batch（大小写不敏感）；空串是缺省 DefaultDelivery。
func ParseDelivery(value string) (Delivery, error) {
	switch d := Delivery(strings.ToLower(strings.TrimSpace(value))); d {
	case "":
		return DefaultDelivery, nil
	case DeliveryDispatch:
		return DeliveryDispatch, nil
	case DeliveryBatch:
		return DeliveryBatch, nil
	default:
		return "", fmt.Errorf("heartbeat: unknown delivery %q (want dispatch or batch)", value)
	}
}

// --- 投递身份 ---
//
// 一个登记的状态分四件事，各有各的字段与写者：
//
//   - 登记状态：该不该跳（Reg.active / Reg.ring / Reg.closed，启停与 Close 维护）；
//   - 投递代际：哪一次投递在途（投递槽 Reg.delivery 里的代际，与这一拍消息携带的凭据 beatTicket.gen）；
//   - 重试次数：同一次投递被卡住兜底重投了几次（Reg.stalls，只决定下一次重投的阈值，不改代际）；
//   - 正在执行：在途的这一代是否已被执行侧认领（投递槽的最低位）。
//
// 投递槽是一个原子字，转换只有下面几条：
//
//	空闲 ──驱动分配第 g 代、投出──▶ 已投出(g) ──执行侧认领──▶ 执行中(g) ──handler 返回（含 panic）──▶ 空闲
//	                                 │
//	                                 └──准入被拒（同步）/ 批量作业交回（异步，下一格开头处理）──▶ 空闲
//
// 谁拥有这一拍：
//
//   - 驱动 goroutine 只在槽空闲时分配新代（每个登记从 1 递增）并把槽置为已投出(g)；槽不空闲时到期的拍按 inflight
//     跳过，在途超过卡住阈值时重投第 g 代（新凭据、同一代际），槽不变。所以每个登记至多一代在途。
//   - 投出之后，第 g 代归先到的结算者：执行侧用 CAS 已投出(g)→执行中(g) 认领，返回时回到空闲；准入被拒与交回用
//     CAS 已投出(g)→空闲 结算。每一代至多结算一次。
//   - 认领或结算失败的消息（槽里不是已投出(g)：这一代已经执行、被拒或交回，下一代可能已经投出；或这一代正在执行）
//     是同一代的另一份：丢弃并计 dropped{reason="duplicate"}，不执行回调，不改槽。所以晚到的旧重投、旧交回不会
//     执行回调，也不会结算下一代。

// 投递槽的取值：0 空闲；gen<<1 第 gen 代已投出（等执行或交回）；gen<<1|1 第 gen 代正在执行。
const deliveryIdle uint64 = 0

func deliveredState(gen uint64) uint64 { return gen << 1 }
func runningState(gen uint64) uint64   { return gen<<1 | 1 }

// inFlight 返回在途的代际；槽空闲时 ok 为 false。任何 goroutine 可调用（只读）。
func (r *Reg) inFlight() (gen uint64, ok bool) {
	s := r.delivery.Load()
	return s >> 1, s != deliveryIdle
}

// issue 分配下一代并把槽置为已投出，返回这一次投递的凭据。只由驱动 goroutine 在槽空闲时调用：空闲只有驱动能离开，
// 所以这里直接写。
func (r *Reg) issue() *beatTicket {
	r.issued++
	r.delivery.Store(deliveredState(r.issued))
	return newBeatTicket(r, r.issued)
}

// claim 是执行侧认领第 gen 代：成功后这一代正在执行，交回不能再结算它。
func (r *Reg) claim(gen uint64) bool {
	return r.delivery.CompareAndSwap(deliveredState(gen), runningState(gen))
}

// finish 是认领了第 gen 代的执行侧返回：槽回到空闲。
func (r *Reg) finish(gen uint64) { r.delivery.CompareAndSwap(runningState(gen), deliveryIdle) }

// settle 结算没有执行的第 gen 代（准入被拒、交回）。这一代已被结算或正在执行时返回 false、不改槽。
func (r *Reg) settle(gen uint64) bool {
	return r.delivery.CompareAndSwap(deliveredState(gen), deliveryIdle)
}

// beatTicket 是一次投递的凭据，作为这一拍 Nest 消息的唯一参数：哪个登记、第几代。构造后不变；重投用同一代际的新凭据。
// 凭据不复用：队列里晚到的旧消息仍持有旧凭据，复用会让它读到新的代际、冒充新的一拍。
type beatTicket struct {
	reg    *Reg
	gen    uint64
	params [1]any // params[0] 是凭据自己；消息参数直接切它，不另分配
}

func newBeatTicket(r *Reg, gen uint64) *beatTicket {
	t := &beatTicket{reg: r, gen: gen}
	t.params[0] = t
	return t
}

// nestParams 是这一拍消息的参数。
func (t *beatTicket) nestParams() nest.Params { return t.params[:] }

// GoString 只输出构造后不变的字段。Nest 的慢派发 trace 在 handler 执行期间由另一个 goroutine 取参数样本：实现了
// fmt.GoStringer 的参数调用 GoString，所以这里不读登记里回调正在原子写的状态。
func (t *beatTicket) GoString() string {
	if t == nil {
		return "(*heartbeat.beatTicket)(nil)"
	}
	return fmt.Sprintf("&heartbeat.beat{registration:%q, entity:%d, generation:%d}", t.reg.name, t.reg.host.id, t.gen)
}

// --- 投递层 ---

// deliverer 把一格到期、核对通过的一批拍（每拍一张凭据）一次交出。rejected 对每个没有准入的拍在驱动 goroutine 上
// 同步调用一次（驱动结算这一代、按跳拍处理）；准入之后 Nest 才发现不能执行的拍经 Driver.handBack 交回（见 batchDeliverer）。
type deliverer interface {
	deliverBatch(ctx context.Context, batch []*beatTicket, rejected func(*beatTicket, error))
}

// beatBatchSize 是一个 Nest 批量作业带的拍数（取值依据见 docs/performance/NEST-BATCH-HEARTBEAT-20261021.md）。
const beatBatchSize = 64

// dispatchDeliverer 是单目标投递（DeliveryDispatch）：每拍一条 Dispatch，走 Nest 正式准入。batchDeliverer 用它投
// Remote 托管实体的拍。
type dispatchDeliverer struct{ client nest.Client }

func (p dispatchDeliverer) deliverBatch(ctx context.Context, batch []*beatTicket, rejected func(*beatTicket, error)) {
	for i, b := range batch {
		if err := ctx.Err(); err != nil {
			// 停止中：剩下的不再投，按准入失败处理（结算这一代）。
			for _, rest := range batch[i:] {
				rejected(rest, err)
			}
			return
		}
		if err := p.client.Dispatch(ctx, b.reg.handler, b.reg.host.id, b.nestParams()); err != nil {
			rejected(b, err)
		}
	}
}

// batchDeliverer（DeliveryBatch）按 beatBatchSize 个一批交给 Nest 批量作业（nest.NestMgr.DispatchTryBatch，设计见
// docs/maintenance/FEATURE-2026-10-21-nest-batch-heartbeat.md）：每拍仍在实体自己的 Guard 作用域与事务里执行；
// 同 ID 有已准入未完成的消息时 Nest 当场把这一拍转成单目标作业排在它后面（不跳拍），只有准入被拒、目标不在内存等
// 才交回（Driver.handBack，下一格开头按跳拍 / 丢弃处理）。Remote 托管实体的拍逐个单目标 Dispatch：它的写要在
// I/O 池取得 Remote 写批次，批量作业不接受。只由驱动 goroutine 调用。
type batchDeliverer struct {
	single  dispatchDeliverer
	batch   nest.TryBatchDispatcher
	driver  *Driver
	targets []nest.TryBatchTarget
	pending []*beatTicket // 与 targets 一一对应，整批准入失败时逐拍 rejected
	remote  []*beatTicket
}

func (p *batchDeliverer) deliverBatch(ctx context.Context, batch []*beatTicket, rejected func(*beatTicket, error)) {
	p.targets, p.pending, p.remote = p.targets[:0], p.pending[:0], p.remote[:0]
	flush := func() {
		if len(p.targets) == 0 {
			return
		}
		if err := ctx.Err(); err != nil {
			for _, b := range p.pending {
				rejected(b, err)
			}
		} else if err := p.batch.DispatchTryBatch(ctx, p.targets, p.driver.handBack); err != nil {
			// 整批没有准入（引擎停止 / fence、ctx 取消）：逐拍按准入失败处理。
			for _, b := range p.pending {
				rejected(b, err)
			}
		}
		clear(p.targets)
		p.targets, p.pending = p.targets[:0], p.pending[:0]
	}
	for _, b := range batch {
		if b.reg.host.remote != nil {
			p.remote = append(p.remote, b)
			continue
		}
		p.targets = append(p.targets, nest.TryBatchTarget{Handler: b.reg.handler, ID: b.reg.host.id, Params: b.nestParams()})
		p.pending = append(p.pending, b)
		if len(p.targets) == beatBatchSize {
			flush()
		}
	}
	flush()
	if len(p.remote) > 0 {
		p.single.deliverBatch(ctx, p.remote, rejected)
	}
	clear(p.pending)
	clear(p.remote)
}

// returnedBeat 是 Nest 批量作业交回的一拍（没有执行）与原因。
type returnedBeat struct {
	ticket *beatTicket
	err    error
}

// handBack 是批量作业的交回回调：可能在 Nest worker 上调用，只把凭据放进交回队列（一把短锁的追加），不改投递槽；
// 驱动在下一格开头结算（drainReturned）。这一代在那之前保持已投出，这期间到期的同一登记照常按 inflight 跳拍。
func (d *Driver) handBack(target nest.TryBatchTarget, err error) {
	t, ok := target.Params[0].(*beatTicket)
	if !ok || t == nil {
		return
	}
	d.returnedMu.Lock()
	d.returned = append(d.returned, returnedBeat{ticket: t, err: err})
	d.returnedMu.Unlock()
}

// drainReturned 结算交回队列里的拍（见 refused）。只由驱动 goroutine 调用。
func (d *Driver) drainReturned(ctx context.Context) {
	d.returnedMu.Lock()
	returned := d.returned
	d.returned = d.drained[:0]
	d.returnedMu.Unlock()
	for i, rb := range returned {
		returned[i] = returnedBeat{}
		d.refused(ctx, rb.ticket, rb.err)
	}
	d.drained = returned[:0]
}

// refused 结算一拍没有执行的投递（准入被拒，或批量作业交回）：只结算凭据自己那一代。这一代已经结算或正在执行时
// （晚到的旧重投被交回）计 dropped{duplicate}，槽与跳拍计数都不动。结算成功时：目标不在内存计 dropped{not_loaded}；
// 驱动停止中（ctx 已取消）只结算；其余按跳拍计数（queue_full / rejected）并限频 Warn。只由驱动 goroutine 调用。
func (d *Driver) refused(ctx context.Context, t *beatTicket, err error) {
	r := t.reg
	if !r.settle(t.gen) {
		d.stats.dropDuplicate.add(r.host.id)
		return
	}
	switch {
	case errors.Is(err, nest.ErrEntityNotFound):
		d.stats.dropNotLoaded.add(r.host.id)
	case ctx.Err() != nil:
	default:
		d.skip(r, err)
	}
}

// skip 记一次因准入失败（含批量作业交回）跳过的拍：计数、限频 Warn。只由驱动 goroutine 调用。
func (d *Driver) skip(r *Reg, err error) {
	r.skipped.Add(1)
	reason := "rejected"
	if errors.Is(err, nest.ErrQueueFull) {
		reason = "queue_full"
		d.stats.skipQueueFull.add(r.host.id)
	} else {
		d.stats.skipRejected.add(r.host.id)
	}
	d.warn.noteErr(r, reason, err)
}

// newDelivererLocked 按配置的投递方式建投递层。调用方持有 d.mu。
func (d *Driver) newDelivererLocked(client nest.Client) (deliverer, error) {
	single := dispatchDeliverer{client: client}
	if d.config.Delivery != DeliveryBatch {
		return single, nil
	}
	batch, ok := client.(nest.TryBatchDispatcher)
	if !ok {
		return nil, fmt.Errorf("heartbeat: driver %s: delivery %q needs a nest client that implements nest.TryBatchDispatcher, got %T", d.config.Name, DeliveryBatch, client)
	}
	return &batchDeliverer{single: single, batch: batch, driver: d}, nil
}
