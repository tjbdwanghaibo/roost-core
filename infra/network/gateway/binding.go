package gateway

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"time"
)

var (
	ErrBindingStale  = errors.New("gateway: stale binding")
	ErrBindingHeld   = errors.New("gateway: binding not active")
	ErrAdmissionFull = errors.New("gateway: admission capacity exhausted")
	ErrDraining      = errors.New("gateway: draining")
)

type connectionKey struct {
	Gate  ProcessIdentity
	Nonce string
}

func keyOf(binding Binding) connectionKey {
	return connectionKey{Gate: binding.Gate, Nonce: binding.ConnectionNonce}
}

type bindingState uint8

const (
	bindingPending bindingState = iota + 1
	bindingConfirmed
	bindingActive
	bindingClosed
)

type bindingRecord struct {
	identity      Binding
	ticketHash    [32]byte
	state         bindingState
	leaseUntil    time.Time
	terminalUntil time.Time
	ready         chan struct{}
	result        ControlResponse
	receiverID    uint64
	admission     uint64    // Bind 带来的接受序号（ControlRequest.Admission），只与同一 Gate 实例的记录比较
	generation    uint64    // Bind 带来的客户端连接代号（ControlRequest.Generation，0 为没带），只与另一个 Gate 实例的记录比较
	arrived       time.Time // Bind 到达 Game 的时刻（receiveControl 收下请求时），跨 Gate 判新旧时与已有绑定的确认时刻比较
	confirmedAt   time.Time // finish 确认这条绑定的时刻
}

// bindingSupersededReason 是迟到的旧 Bind 被拒的原因：同一 SessionID 已绑定的是更新的连接（同一 Gate 实例按接受
// 序号、跨 Gate 实例按客户端连接代号，见 supersedes）。
// Gate 据此把这次握手当作正常的会话关闭收尾（不按通道故障告警）。
const bindingSupersededReason = "superseded by a newer connection of the session"

// bindingTable 保留有效终态，并在新 Bind 准入时预留未来终态的位置。
// 关闭已有连接不能因 tombstone 容量满而忘掉事实，迟到 Bind 也不能复活它。
type bindingTable struct {
	mu           sync.Mutex
	config       Config
	game         ProcessIdentity
	byConnection map[connectionKey]*bindingRecord
	bySession    map[string]*bindingRecord
	byReceiver   map[uint64]*bindingRecord
	active       int
	nextReceiver uint64
	closing      bool
}

func newBindingTable(config Config, game ProcessIdentity) *bindingTable {
	return &bindingTable{config: config, game: game, byConnection: make(map[connectionKey]*bindingRecord), bySession: make(map[string]*bindingRecord), byReceiver: make(map[uint64]*bindingRecord)}
}

// reserve 的 owner 负责唯一一次鉴权与 finish。重复 Bind 等待同一结果，不重复注册队列。arrived 是这次 Bind 到达
// Game 的时刻（进入控制队列之前），只在本次成为 owner 时记入记录。
func (table *bindingTable) reserve(request ControlRequest, arrived time.Time) (record *bindingRecord, owner bool, err error) {
	if err := request.Validate(); err != nil {
		return nil, false, err
	}
	if request.Operation != Bind || request.Binding.Game != table.game {
		return nil, false, ErrBindingStale
	}
	key, hash := keyOf(request.Binding), sha256.Sum256([]byte(request.Ticket))
	table.mu.Lock()
	defer table.mu.Unlock()
	if table.closing {
		return nil, false, ErrDraining
	}
	if prior := table.byConnection[key]; prior != nil {
		want := prior.identity
		want.BindID = ""
		if want != request.Binding || prior.ticketHash != hash || prior.admission != request.Admission || prior.generation != request.Generation {
			return nil, false, ErrBindingStale
		}
		if prior.state == bindingClosed {
			return nil, false, ErrBindingStale
		}
		return prior, false, nil
	}
	// 常规请求不全表扫描。达到有界总量时只回收确已过期的终态，不淘汰有效记录。
	if len(table.byConnection) >= table.config.MaxTombstones {
		table.pruneLocked(time.Now())
	}
	if table.active >= table.config.MaxBindings || len(table.byConnection) >= table.config.MaxTombstones {
		return nil, false, ErrAdmissionFull
	}
	if table.nextReceiver == ^uint64(0) {
		return nil, false, ErrAdmissionFull
	}
	table.nextReceiver++
	identity := request.Binding
	identity.BindID = newBindingToken()
	record = &bindingRecord{identity: identity, ticketHash: hash, state: bindingPending, leaseUntil: time.Now().Add(table.config.RequestTimeout), ready: make(chan struct{}), receiverID: table.nextReceiver, admission: request.Admission, generation: request.Generation, arrived: arrived}
	table.byConnection[key] = record
	table.byReceiver[record.receiverID] = record
	table.active++
	return record, true, nil
}

func (table *bindingTable) pruneLocked(now time.Time) {
	for key, record := range table.byConnection {
		if record.state == bindingClosed && !now.Before(record.terminalUntil) {
			delete(table.byConnection, key)
		}
	}
}

// finish 必须在真实鉴权/队列注册结束后调用。替换 SessionID 时只关闭对应旧记录；
// 调用方在锁外按返回的 previous 清理旧 lifetime，不能在此持锁等待网络或 Nest。
//
// 同一 SessionID 的新旧：同一 Gate 实例（Binding.Gate 含进程代际）上的两条连接按 Gate 给的接受序号定，不按 Bind
// 完成的先后——已绑定的是更晚接受的连接时，本记录是一次迟到完成的旧 Bind（客户端放弃这次握手后已经重连，或旧连接
// 的鉴权慢于新连接），拒绝它、不退役任何人，Gate 收到拒绝后关掉那条旧连接；若让后完成者胜出，它会退役正在服务的
// 新绑定并通知 Gate 关掉新连接，而旧连接又被 Gate 本地按接受先后拒登记，两条都不剩。判定与写入在同一把锁里，两次
// Bind 无论怎样交错，留下的都是接受得更晚的那条，与嵌入 TCP 的 registerSession 同一规则。
// 不同 Gate 实例（含同一 Gate 重启后的新代际）的序号各自从头编号，不可比较，改比客户端连接代号，见 supersedes。
func (table *bindingTable) finish(record *bindingRecord, accepted bool) (previous *bindingRecord, result ControlResponse) {
	table.mu.Lock()
	defer table.mu.Unlock()
	if record.state != bindingPending {
		return nil, record.result
	}
	if !accepted || table.closing {
		table.closeLocked(record)
		return nil, record.result
	}
	previous = table.bySession[record.identity.SessionID]
	if previous != nil && previous.identity.PlayerID != record.identity.PlayerID {
		table.closeLocked(record)
		return nil, record.result
	}
	if previous != nil && supersedes(previous, record) {
		table.closeLocked(record)
		record.result.Reason = bindingSupersededReason
		return nil, record.result
	}
	record.state = bindingConfirmed
	record.confirmedAt = time.Now()
	record.leaseUntil = record.confirmedAt.Add(table.config.Lease)
	if previous != nil && previous != record {
		table.closeLocked(previous)
	}
	table.bySession[record.identity.SessionID] = record
	record.result = ControlResponse{ExecutionResult: ExecutionResult{Version: InternalVersion, Outcome: Completed}, Binding: record.identity, LeaseUntilUnixNano: record.leaseUntil.UnixNano()}
	close(record.ready)
	return previous, record.result
}

// supersedes 判断同一 SessionID 已确认的 current 是否比正要完成的 record 更新，是则 record 是一次迟到完成的旧
// Bind，应被拒。
//
//   - 同一 Gate 实例（Binding.Gate 含进程代际）：比 Gate 给的接受序号，精确、不依赖客户端，连接代号不参与。
//   - 不同 Gate 实例：接受序号不可比较，改比客户端在认证帧里带来的连接代号（wire.EncodeAuth），三个条件同时成立
//     才拒：两边都带了代号；current 的代号更大；record 到达 Game 早于 current 被确认（两次握手在 Game 上重叠）。
//     任一不成立就退回“后完成者胜出”。
//
// 第三个条件让代号只裁决重叠的握手：current 确认之后才到达的 Bind 无论代号多小都照常替换它。所以一个带了超大
// 代号（客户端缺陷或时钟回拨）的绑定挡不住之后的正常重连，客户端也不能用代号锁死自己的会话；被它拒掉的只有确认时
// 已经在途的那几次 Bind，客户端重连一次即恢复。代价是覆盖面：旧连接的 Bind 在新绑定确认之后才到达 Game（Gate A
// 在发 Bind 之前就慢了）时识别不出，结果与没有代号相同。
func supersedes(current, record *bindingRecord) bool {
	if current.identity.Gate == record.identity.Gate {
		return current.admission > record.admission
	}
	return current.generation != 0 && record.generation != 0 && current.generation > record.generation &&
		record.arrived.Before(current.confirmedAt)
}

func (table *bindingTable) await(ctx context.Context, record *bindingRecord) (ControlResponse, error) {
	select {
	case <-record.ready:
		table.mu.Lock()
		result := record.result
		table.mu.Unlock()
		return result, nil
	case <-ctx.Done():
		return ControlResponse{}, ctx.Err()
	}
}

func (table *bindingTable) control(request ControlRequest) (*bindingRecord, ControlResponse, error) {
	if err := request.Validate(); err != nil {
		return nil, ControlResponse{}, err
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	record := table.byConnection[keyOf(request.Binding)]
	if record == nil || record.identity != request.Binding || record.state == bindingPending {
		return nil, ControlResponse{}, ErrBindingStale
	}
	if record.state == bindingClosed {
		if request.Operation == Unbind {
			return record, ControlResponse{ExecutionResult: ExecutionResult{Version: InternalVersion, Outcome: Completed}, Binding: record.identity}, nil
		}
		return nil, ControlResponse{}, ErrBindingStale
	}
	if !time.Now().Before(record.leaseUntil) {
		table.closeLocked(record)
		return record, record.result, ErrBindingStale
	}
	switch request.Operation {
	case Activate:
		if table.closing {
			return nil, ControlResponse{}, ErrDraining
		}
		if record.state == bindingActive {
			return record, record.result, nil
		}
		record.state = bindingActive
	case Renew:
		if table.closing {
			return nil, ControlResponse{}, ErrDraining
		}
		if record.state != bindingActive {
			return nil, ControlResponse{}, ErrBindingHeld
		}
	case Unbind:
		table.closeLocked(record)
		return record, ControlResponse{ExecutionResult: ExecutionResult{Version: InternalVersion, Outcome: Completed}, Binding: record.identity}, nil
	default:
		return nil, ControlResponse{}, ErrInvalidRequest
	}
	record.leaseUntil = time.Now().Add(table.config.Lease)
	record.result.LeaseUntilUnixNano = record.leaseUntil.UnixNano()
	return record, record.result, nil
}

func (table *bindingTable) closeLocked(record *bindingRecord) bool {
	if record.state == bindingClosed {
		return false
	}
	pending := record.state == bindingPending
	record.state = bindingClosed
	record.terminalUntil = time.Now().Add(table.config.TombstoneTTL)
	record.result = ControlResponse{ExecutionResult: ExecutionResult{Version: InternalVersion, Outcome: NotAdmitted, Reason: "binding closed"}, Binding: record.identity}
	if table.bySession[record.identity.SessionID] == record {
		delete(table.bySession, record.identity.SessionID)
	}
	delete(table.byReceiver, record.receiverID)
	table.active--
	if pending {
		close(record.ready)
	}
	return true
}

func (table *bindingTable) close(binding Binding) (*bindingRecord, bool) {
	table.mu.Lock()
	defer table.mu.Unlock()
	record := table.byConnection[keyOf(binding)]
	if record == nil || record.identity != binding {
		return nil, false
	}
	return record, table.closeLocked(record)
}

func (table *bindingTable) lookup(binding Binding, active bool) (*bindingRecord, error) {
	table.mu.Lock()
	defer table.mu.Unlock()
	record := table.byConnection[keyOf(binding)]
	if record == nil || record.identity != binding || record.state == bindingClosed {
		return nil, ErrBindingStale
	}
	if active && record.state != bindingActive {
		return nil, ErrBindingHeld
	}
	if !time.Now().Before(record.leaseUntil) {
		return nil, ErrBindingStale
	}
	return record, nil
}

func (table *bindingTable) expire(stopAll bool) []*bindingRecord {
	table.mu.Lock()
	defer table.mu.Unlock()
	var closed []*bindingRecord
	now := time.Now()
	for _, record := range table.byConnection {
		if record.state != bindingClosed && (stopAll || !now.Before(record.leaseUntil)) {
			table.closeLocked(record)
			closed = append(closed, record)
		}
	}
	table.pruneLocked(now)
	return closed
}

func (table *bindingTable) beginDrain() { table.mu.Lock(); table.closing = true; table.mu.Unlock() }
