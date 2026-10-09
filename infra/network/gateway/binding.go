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
}

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

// reserve 的 owner 负责唯一一次鉴权与 finish。重复 Bind 等待同一结果，不重复注册队列。
func (table *bindingTable) reserve(request ControlRequest) (record *bindingRecord, owner bool, err error) {
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
		if want != request.Binding || prior.ticketHash != hash {
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
	record = &bindingRecord{identity: identity, ticketHash: hash, state: bindingPending, leaseUntil: time.Now().Add(table.config.RequestTimeout), ready: make(chan struct{}), receiverID: table.nextReceiver}
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
	record.state = bindingConfirmed
	record.leaseUntil = time.Now().Add(table.config.Lease)
	if previous != nil && previous != record {
		table.closeLocked(previous)
	}
	table.bySession[record.identity.SessionID] = record
	record.result = ControlResponse{ExecutionResult: ExecutionResult{Version: InternalVersion, Outcome: Completed}, Binding: record.identity, LeaseUntilUnixNano: record.leaseUntil.UnixNano()}
	close(record.ready)
	return previous, record.result
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
