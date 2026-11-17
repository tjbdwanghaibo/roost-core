package relay

// 换 host（方案 FEATURE-2026-11-16-lobby-relay §4.6；契约 docs/framework/RELAY.md §2.3、§4A）：解 relay 发来的
// 64～69 控制帧、记当前角色，发 80～84 控制帧，当房主时定时推快照、压缩快照与交接数据。
//
// 角色的变化只按 relay 的控制帧走（relay 是角色的权威，§4.6.5）：
//
//	Welcome            → 照 Welcome 的角色与房主槽位（Welcome 不带 epoch，Epoch 保留最近见到的值）
//	YouAreHost{epoch}  → 候选（Candidate）：先交给 OnYouAreHost，游戏准备好后调 HostReady(epoch)；
//	                     Options.AutoHostReady 为 true 时回调返回后 SDK 自动回
//	HostChanged{epoch} → Seq 是新房主槽位：等于自己就是房主，否则是成员
//	YouAreMember{epoch}→ 成员（被换下的旧房主、超时的候选），房主槽位记为 0（还在选人）
//	HostResumed{epoch} → 房主槽位是 Seq（宽限期内原房主回来了）
//	HostChanging{epoch}→ 角色不变；只通知游戏“发给房主的游戏帧被丢了”，SDK 不缓存、不重发，要不要重发由游戏决定

import (
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
)

// StateSource 说明 YouAreHost.State 从哪来：StateNone（没有）、StateHandoff（主动转交的交接数据）、
// StateSnapshot（relay 存的最新快照）。
type StateSource = wire.RelayStateSource

const (
	StateNone     = wire.RelayStateNone
	StateHandoff  = wire.RelayStateHandoff
	StateSnapshot = wire.RelayStateSnapshot
)

// Seat 是 YouAreHost 里的一个成员：房间存续期间进过房、没被踢的账号，含离线的、含旧房主，按槽位排列。
type Seat struct {
	Slot    uint32
	Account string
	Online  bool
}

// YouAreHost 是“relay 选自己当新房主”（you_are_host）。游戏据此接手（交接数据或快照，再加上自己本地的状态），
// 准备好后调 Client.HostReady(Epoch)；relay.host_ready_timeout（缺省 10s）内不回就换下一个候选。
type YouAreHost struct {
	Epoch  uint64
	Source StateSource
	// SnapshotSeq / SnapshotAge：Source 为 StateSnapshot 时是那份快照的 seq 与 relay 收到它到发出这一帧的时长
	// （新房主据此知道快照落后了多少）；其余来源为 0。
	SnapshotSeq uint64
	SnapshotAge time.Duration
	Members     []Seat
	// State 是解压后的状态字节（Options.Compressor 解；Source 为 StateNone 时为空）。解压失败时 State 为 nil、
	// StateErr 是原因，游戏可以当没有状态处理（用自己本地的状态，或重开）。
	State    []byte
	StateErr error
}

// HostEvent 是 HostChanged（新房主已就绪，Account 是新房主账号）与 HostResumed（宽限期内原房主回来了，Account 为空）。
type HostEvent struct {
	Epoch    uint64
	HostSlot uint32
	Account  string
}

// RejectCode 是 Rejected 的原因：wire.RelayRejectTooBig、TooSoon、StaleSeq、BadTarget、NotHost。
type RejectCode = wire.RelayRejectCode

// Rejected 是 relay 拒收了自己发的快照或转交（连接保留）。Op 是被拒的操作码（wire.RelayOpSnapshot 或
// wire.RelayOpTransfer），Seq 是被拒快照的 seq（转交为 0）。
type Rejected struct {
	Op   uint32
	Code RejectCode
	Seq  uint64
}

// Role 是 SDK 记录的当前角色，只按 relay 的控制帧更新（见本文件开头）。
type Role struct {
	Slot uint32 // 自己的槽位
	// Host：自己是已就绪的房主（Welcome 是房主，或 HostChanged 的槽位是自己）。
	Host bool
	// Candidate：收到了 YouAreHost、还没收到 HostChanged / YouAreMember；这时应当回 HostReady(Epoch)。
	Candidate bool
	// HostSlot 是当前房主的槽位；正在选新房主时为 0。
	HostSlot uint32
	// Epoch 是最近一次见到的 host_epoch（YouAreHost / YouAreMember / HostChanged / HostResumed 带来）。
	// Welcome 不带 epoch：连上之后还没见过换 host 时为 0（不一定是房间真实的 epoch）。
	Epoch uint64
}

var (
	// ErrSnapshotTooSoon：离上一份快照不到 Options.SnapshotMinInterval，这一份没有发出。
	ErrSnapshotTooSoon = errors.New("relay: snapshot sent too soon after the previous one")
	// ErrStateTooBig：压缩后的快照或交接数据超过 Options.SnapshotMaxBytes，没有发出。
	ErrStateTooBig = errors.New("relay: snapshot or handoff exceeds SnapshotMaxBytes after compression")
	// ErrTooManySuccessors：候选顺序超过 wire.RelayMaxSuccessors 个槽位。
	ErrTooManySuccessors = errors.New("relay: too many successors")
)

// Compressor 压缩快照与交接数据（房主发出前压缩，新房主收到 YouAreHost 时解压）。relay 不压缩、不解压、不解析，
// 房间里每个客户端要用同一种。缺省 Deflate；游戏可以换成 LZ4 或 zstd 的实现（SDK 不带第三方依赖）。
// Decompress 解出的字节超过 limit 时返回错误（防解压炸弹）。
type Compressor interface {
	Compress(data []byte) ([]byte, error)
	Decompress(data []byte, limit int) ([]byte, error)
}

// Deflate 是缺省的 Compressor：RFC 1951 原始 deflate（Go compress/flate，C# DeflateStream 与它互通）。
var Deflate Compressor = deflateCompressor{}

// NoCompression 原样发出、原样交出。
var NoCompression Compressor = noCompression{}

// MaxStateBytes 是 YouAreHost 里状态字节解压后的上限。
const MaxStateBytes = 16 << 20

type deflateCompressor struct{}

func (deflateCompressor) Compress(data []byte) ([]byte, error) {
	var out bytes.Buffer
	w, err := flate.NewWriter(&out, flate.DefaultCompression)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (deflateCompressor) Decompress(data []byte, limit int) ([]byte, error) {
	r := flate.NewReader(bytes.NewReader(data))
	defer r.Close()
	out, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("relay: inflate state: %w", err)
	}
	if len(out) > limit {
		return nil, fmt.Errorf("relay: inflated state exceeds %d bytes", limit)
	}
	return out, nil
}

type noCompression struct{}

func (noCompression) Compress(data []byte) ([]byte, error) { return data, nil }
func (noCompression) Decompress(data []byte, limit int) ([]byte, error) {
	if len(data) > limit {
		return nil, fmt.Errorf("relay: state exceeds %d bytes", limit)
	}
	return data, nil
}

// youAreHostOverhead 是 YouAreHost 除状态字节以外的余量（固定头 + 成员名单），与 service/relay 相同：
// 收帧上限至少是 SnapshotMaxBytes 加上它。
const youAreHostOverhead = 16 << 10

// hostState 是换 host 相关的状态，由 Client.mu 保护。
type hostState struct {
	epoch     uint64
	candidate bool
	snapSeq   uint64    // 最近发出（或从 YouAreHost 接过来）的快照 seq；下一份是它加 1
	snapAt    time.Time // 最近一次发出快照的时刻；零值表示当上房主之后还没发过
	autoAt    time.Time // 最近一次定时推送的尝试（State 返回 nil、压缩后太大也算），下一次定时推送从它和 snapAt 的较晚者算
}

// OnYouAreHost 注册“relay 选自己当新房主”的回调。游戏准备好后调 HostReady(e.Epoch)（或打开 Options.AutoHostReady）。
func (c *Client) OnYouAreHost(fn func(YouAreHost)) { c.mu.Lock(); c.h.youAreHost = fn; c.mu.Unlock() }

// OnYouAreMember 注册“自己不是（或没能成为）房主”的回调：主动转交后的旧房主、没在时限内回 HostReady 的候选。
func (c *Client) OnYouAreMember(fn func(epoch uint64)) {
	c.mu.Lock()
	c.h.youAreMember = fn
	c.mu.Unlock()
}

// OnHostChanging 注册“发给房主的游戏帧被丢了”的回调（宽限期或选人期间，每次换 host 至多一次）。relay 不缓存这些帧，
// SDK 也不重发：要不要等 HostChanged 后重发由游戏决定。
func (c *Client) OnHostChanging(fn func(epoch uint64)) {
	c.mu.Lock()
	c.h.hostChanging = fn
	c.mu.Unlock()
}

// OnHostChanged 注册“新房主已就绪”的回调（房间里每个人都收到，含新房主自己）；之后游戏帧恢复转发。
func (c *Client) OnHostChanged(fn func(HostEvent)) { c.mu.Lock(); c.h.hostChanged = fn; c.mu.Unlock() }

// OnHostResumed 注册“房主在宽限期内回来了、仍是原来的房主”的回调（只有成员收到）。
func (c *Client) OnHostResumed(fn func(HostEvent)) { c.mu.Lock(); c.h.hostResumed = fn; c.mu.Unlock() }

// OnRejected 注册“relay 拒收了自己发的快照或转交”的回调（连接保留）。
func (c *Client) OnRejected(fn func(Rejected)) { c.mu.Lock(); c.h.rejected = fn; c.mu.Unlock() }

// Role 返回 SDK 记录的当前角色。
func (c *Client) Role() Role {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Role{Slot: c.role.Slot, Host: c.role.Host, Candidate: c.host.candidate, HostSlot: c.role.HostSlot, Epoch: c.host.epoch}
}

// HostReady 告诉 relay 自己已接手（epoch 是 YouAreHost 里的）。relay 广播 HostChanged 之后自己才是房主；
// 迟到的、epoch 不对的 HostReady 被 relay 忽略。
func (c *Client) HostReady(epoch uint64) error {
	return c.SendControl(wire.RelayOpHostReady, 0, wire.EncodeRelayEpoch(epoch))
}

// Transfer 把房主主动转交给槽位 slot 上的在线成员（只有房主能发）。handoff 是交接数据（经 Options.Compressor 压缩后
// 不超过 SnapshotMaxBytes），relay 原样放进新房主的 YouAreHost。目标不在线或是自己时 relay 回 Rejected{bad_target}。
func (c *Client) Transfer(slot uint32, handoff []byte) error {
	if !c.IsHost() {
		return ErrNotRoomHost
	}
	data, err := c.compressState(handoff)
	if err != nil {
		return err
	}
	return c.SendControl(wire.RelayOpTransfer, slot, data)
}

// SetSuccessors 声明候选顺序（只有房主能发）：房主离开或断线时 relay 按这个顺序取第一个在线的成员当新房主。
// relay 只记最新一份，换 host 后清空，由新房主重新声明。
func (c *Client) SetSuccessors(slots []uint32) error {
	if len(slots) > wire.RelayMaxSuccessors {
		return ErrTooManySuccessors
	}
	if !c.IsHost() {
		return ErrNotRoomHost
	}
	return c.SendControl(wire.RelayOpSuccessors, 0, wire.EncodeRelaySuccessors(slots))
}

// Snapshot 推一份快照（只有房主能发）：state 经 Options.Compressor 压缩，seq 由 SDK 递增。离上一份不到
// SnapshotMinInterval 的不发（ErrSnapshotTooSoon）；压缩后超过 SnapshotMaxBytes 的不发（ErrStateTooBig）。
// relay 仍可能拒收（例如它的 snapshot_min_interval 比 SDK 的大），经 OnRejected 通知。
func (c *Client) Snapshot(state []byte) error {
	data, err := c.compressState(state)
	if err != nil {
		return err
	}
	now := time.Now()
	c.mu.Lock()
	switch {
	case !c.role.Host:
		c.mu.Unlock()
		return ErrNotRoomHost
	case !c.host.snapAt.IsZero() && now.Sub(c.host.snapAt) < c.opts.SnapshotMinInterval:
		c.mu.Unlock()
		return ErrSnapshotTooSoon
	}
	c.host.snapSeq++
	c.host.snapAt = now
	seq := c.host.snapSeq
	c.mu.Unlock()
	return c.SendControl(wire.RelayOpSnapshot, 0, wire.EncodeRelaySnapshot(wire.RelaySnapshot{Seq: seq, Data: data}))
}

func (c *Client) compressState(state []byte) ([]byte, error) {
	data, err := c.opts.Compressor.Compress(state)
	if err != nil {
		return nil, err
	}
	if len(data) > c.opts.SnapshotMaxBytes {
		return nil, ErrStateTooBig
	}
	return data, nil
}

// autoSnapshot 是当房主时的定时快照（心跳 goroutine 上调用）：Options.SnapshotState 返回 nil 时这一轮不推。
func (c *Client) autoSnapshot() {
	c.mu.Lock()
	c.host.autoAt = time.Now()
	c.mu.Unlock()
	state := c.opts.SnapshotState()
	if state != nil {
		_ = c.Snapshot(state)
	}
}

// snapshotDue 返回下一次定时快照的时刻；不该自动推（不是房主、没设 SnapshotState）时 ok 为 false。
func (c *Client) snapshotDue() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.role.Host || c.opts.SnapshotState == nil {
		return time.Time{}, false
	}
	last := c.host.snapAt
	if c.host.autoAt.After(last) {
		last = c.host.autoAt
	}
	return last.Add(c.opts.SnapshotInterval), true // 当上房主后还没推过（都是零值）就立即推
}

// becomeHostLocked 在自己当上已就绪的房主时调用：relay 对新房主重新计最小间隔，SDK 也重新算。
func (c *Client) becomeHostLocked() { c.host.snapAt, c.host.autoAt = time.Time{}, time.Time{} }

// seqFloor 是新房主快照 seq 的下限：relay 要求 seq 在房间内递增（跨房主也一样），可 YouAreHost 只在来源是快照时才带
// relay 存着的那份的 seq，主动转交时为 0，而 relay 可能还存着旧房主更大的 seq。epoch 每次换 host 加 1，所以
// 让 seq 不低于 epoch<<32 就一定大于以前任何房主用本 SDK 发过的 seq（每个房主 2^32 份以内）。
func seqFloor(epoch uint64) uint64 { return epoch << 32 }

// pingInterval 是当前角色的心跳间隔：房主与候选用 HostPingInterval（relay 的 host_idle_timeout 缺省 2s），成员用 PingInterval。
func (c *Client) pingInterval() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.role.Host || c.host.candidate {
		return c.opts.HostPingInterval
	}
	return c.opts.PingInterval
}

// roleChanged 叫醒当前连接的心跳 goroutine：角色变了，心跳间隔与定时快照要立即按新角色算。
func (c *Client) roleChanged() {
	c.mu.Lock()
	sess := c.current
	c.mu.Unlock()
	if sess != nil {
		select {
		case sess.wake <- struct{}{}:
		default:
		}
	}
}

// hostControlHandlers 是换 host 的控制帧（relay 发出的 64～69）的内置处理，并进 controlHandlers。
var hostControlHandlers = map[uint32]func(c *Client, h handlers, f Frame) (wire.RelayReason, error){
	wire.RelayOpYouAreHost: func(c *Client, h handlers, f Frame) (wire.RelayReason, error) {
		offer, err := wire.DecodeRelayYouAreHost(f.Payload)
		if err != nil {
			return wire.RelayReasonProtocol, err
		}
		c.mu.Lock()
		c.role.Slot, c.role.Host, c.role.HostSlot = f.Slot, false, 0
		c.host.epoch, c.host.candidate = offer.Epoch, true
		// 新房主的快照 seq 从 relay 存着的那份往上接（§4.6.3），也不低于这一轮 epoch 的下限（见 seqFloor）。
		c.host.snapSeq = max(c.host.snapSeq, offer.SnapshotSeq, seqFloor(offer.Epoch))
		c.mu.Unlock()
		c.roleChanged()
		event := YouAreHost{Epoch: offer.Epoch, Source: offer.Source, SnapshotSeq: offer.SnapshotSeq,
			SnapshotAge: time.Duration(offer.SnapshotAgeMillis) * time.Millisecond, Members: make([]Seat, len(offer.Members))}
		for i, seat := range offer.Members {
			event.Members[i] = Seat(seat)
		}
		if offer.Source != StateNone {
			event.State, event.StateErr = c.opts.Compressor.Decompress(offer.State, MaxStateBytes)
		}
		if h.youAreHost != nil {
			h.youAreHost(event)
		}
		if c.opts.AutoHostReady {
			_ = c.HostReady(offer.Epoch)
		}
		return wire.RelayReasonNone, nil
	},
	wire.RelayOpYouAreMember: func(c *Client, h handlers, f Frame) (wire.RelayReason, error) {
		epoch, err := wire.DecodeRelayEpoch(f.Payload)
		if err != nil {
			return wire.RelayReasonProtocol, err
		}
		c.mu.Lock()
		c.role.Host, c.role.HostSlot = false, 0 // 还在选人
		c.host.epoch, c.host.candidate = epoch, false
		c.mu.Unlock()
		c.roleChanged()
		if h.youAreMember != nil {
			h.youAreMember(epoch)
		}
		return wire.RelayReasonNone, nil
	},
	wire.RelayOpHostChanging: func(_ *Client, h handlers, f Frame) (wire.RelayReason, error) {
		epoch, err := wire.DecodeRelayEpoch(f.Payload)
		if err != nil {
			return wire.RelayReasonProtocol, err
		}
		if h.hostChanging != nil {
			h.hostChanging(epoch)
		}
		return wire.RelayReasonNone, nil
	},
	wire.RelayOpHostChanged: func(c *Client, h handlers, f Frame) (wire.RelayReason, error) {
		changed, err := wire.DecodeRelayHostChanged(f.Payload)
		if err != nil {
			return wire.RelayReasonProtocol, err
		}
		c.mu.Lock()
		wasHost := c.role.Host
		c.role.HostSlot, c.role.Host = f.Slot, f.Slot == c.role.Slot
		c.host.epoch, c.host.candidate = changed.Epoch, false
		if c.role.Host && !wasHost {
			c.becomeHostLocked()
		}
		c.mu.Unlock()
		c.roleChanged()
		if h.hostChanged != nil {
			h.hostChanged(HostEvent{Epoch: changed.Epoch, HostSlot: f.Slot, Account: changed.Account})
		}
		return wire.RelayReasonNone, nil
	},
	wire.RelayOpHostResumed: func(c *Client, h handlers, f Frame) (wire.RelayReason, error) {
		epoch, err := wire.DecodeRelayEpoch(f.Payload)
		if err != nil {
			return wire.RelayReasonProtocol, err
		}
		c.mu.Lock()
		c.role.HostSlot, c.host.epoch = f.Slot, epoch
		c.mu.Unlock()
		if h.hostResumed != nil {
			h.hostResumed(HostEvent{Epoch: epoch, HostSlot: f.Slot})
		}
		return wire.RelayReasonNone, nil
	},
	wire.RelayOpRejected: func(_ *Client, h handlers, f Frame) (wire.RelayReason, error) {
		rejected, err := wire.DecodeRelayRejected(f.Payload)
		if err != nil {
			return wire.RelayReasonProtocol, err
		}
		if h.rejected != nil {
			h.rejected(Rejected(rejected))
		}
		return wire.RelayReasonNone, nil
	},
}

func init() {
	for op, fn := range hostControlHandlers {
		controlHandlers[op] = fn
	}
}
