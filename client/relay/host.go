package relay

// 换 host（方案 FEATURE-2026-11-16-lobby-relay §4.6；契约 docs/framework/RELAY.md §2.3、§4A）：解 relay 发来的
// 64～69 控制帧、记当前角色，发 80～84 控制帧，当房主时定时推快照、压缩快照与交接数据。
//
// 角色的变化只按 relay 的控制帧走（relay 是角色的权威，§4.6.5）：
//
//	Welcome            → 照 Welcome 的角色、房主槽位与 epoch（选人阶段房主槽位为 0、epoch 是新的一轮）
//	YouAreHost{epoch}  → 候选（Candidate）：先交给 OnYouAreHost，游戏准备好后调 HostReady(epoch)；
//	                     Options.AutoHostReady 为 true 时回调返回后 SDK 自动回
//	HostChanged{epoch} → Seq 是新房主槽位：等于自己就是房主，否则是成员
//	YouAreMember{epoch}→ 成员（被换下的旧房主、超时的候选），房主槽位记为 0（还在选人）
//	HostResumed{epoch} → 房主槽位是 Seq（宽限期内原房主回来了）
//	HostChanging{epoch}→ 角色不变；只通知游戏“发给房主的游戏帧被丢了”，SDK 不缓存、不重发，要不要重发由游戏决定
//
// 前台上报（RELAY.md §2.3）：SetForeground 记下自己在前台还是后台并经 relay 告诉当前房主（MemberPresence）；收到
// HostChanged、以及每次连上（含自动重连、Rejoin）后，设过状态的 SDK 自动再发一次，新房主不用问。房主一侧只经
// OnMemberPresence 交给游戏，Go SDK 不自动排候选顺序（游戏自己 SetSuccessors）。

import (
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"sync"
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
	// Epoch 是最近一次见到的 host_epoch（Welcome / YouAreHost / YouAreMember / HostChanged / HostResumed 带来）。
	// Welcome 带着进房时房间的 epoch，所以换过 host 之后才进房、或重连回来的客户端也拿到房间此刻的值。
	Epoch uint64
}

// MemberPresence 是房主收到的成员前台上报（RelayOpMemberPresence）：Slot 是成员槽位，Foreground 为 false 表示后台。
// 成员可以谎报，它只是排候选顺序的参考。
type MemberPresence struct {
	Slot       uint32
	Foreground bool
}

var (
	// ErrSnapshotTooSoon：离上一份快照不到 Options.SnapshotMinInterval，这一份没有发出。
	ErrSnapshotTooSoon = errors.New("relay: snapshot sent too soon after the previous one")
	// ErrSnapshotStale：这一份的状态已经过时，没有发出，也没占用 seq。两种情况：准备这一份（取状态、压缩）期间换了连接
	// 或房主任期（例如转出去又转回来）；或者同一任期里比它晚发起的一份已经先发出了（并发推送以发起顺序为准）。
	ErrSnapshotStale = errors.New("relay: snapshot dropped: a newer snapshot was sent, or the connection or host term changed, while it was being prepared")
	// ErrStateTooBig：压缩后的快照或交接数据超过 Options.SnapshotMaxBytes，没有发出。
	ErrStateTooBig = errors.New("relay: snapshot or handoff exceeds SnapshotMaxBytes after compression")
	// ErrTooManySuccessors：候选顺序超过 wire.RelayMaxSuccessors 个槽位。
	ErrTooManySuccessors = errors.New("relay: too many successors")
	// ErrUnknownCompression：收到的快照或交接数据的压缩方式标识既不是自己的 Compressor.ID()，也不是内置的两种
	// （房间里有人用了别的压缩方式）。YouAreHost.StateErr 用 errors.Is 认它。
	ErrUnknownCompression = errors.New("relay: state uses an unknown compression method")
)

// Compressor 压缩快照与交接数据（房主发出前压缩，新房主收到 YouAreHost 时解压）。relay 不压缩、不解压、不解析。
// 缺省 Deflate；游戏可以换成 LZ4 或 zstd 的实现（SDK 不带第三方依赖，接法见 client/relay/README.md）。
//
// ID 是压缩方式标识（SDK 之间的约定，docs/framework/RELAY.md §2.3）：SDK 把它作为 1 个字节写在压缩后的状态字节前面，
// 收到时按它选解压方式——自己的 Compressor，或内置的 NoCompression（0）、Deflate（1）。0 与 1 归内置的两种，游戏自带的
// 用 2～255（README 的示例里 zstd 用 2、LZ4 用 3）。标识不认识时 YouAreHost.StateErr 是 ErrUnknownCompression。
// Decompress 解出的字节超过 limit 时返回错误（防解压炸弹）。
type Compressor interface {
	ID() byte
	Compress(data []byte) ([]byte, error)
	Decompress(data []byte, limit int) ([]byte, error)
}

// Deflate 是缺省的 Compressor：RFC 1951 原始 deflate（Go compress/flate，C# DeflateStream 与它互通）。
var Deflate Compressor = deflateCompressor{}

// NoCompression 原样发出、原样交出。
var NoCompression Compressor = noCompression{}

// MaxStateBytes 是 YouAreHost 里状态字节解压后的上限。
const MaxStateBytes = 16 << 20

// 内置压缩方式的标识。
const (
	compressionNone    byte = 0
	compressionDeflate byte = 1
)

type deflateCompressor struct{}

func (deflateCompressor) ID() byte { return compressionDeflate }

// deflate 的压缩器与解压器在多次调用之间复用（2026-11-18 测量：每次新建 flate.Writer 固定约 1 MB 分配，1KiB 的小快照也一样）。
// Reset 之后的 Writer 与新建的输出逐字节相同（compress/flate 的 Reset 契约），线上字节不变，见 TestDeflateReuse。
// 放回池子之前把它们对调用方缓冲区的引用断开（deflateSink.out、bytes.Reader），池子里不留住快照字节。
var (
	deflateWriters = sync.Pool{New: func() any {
		d := &pooledDeflater{}
		d.w, _ = flate.NewWriter(&d.sink, flate.DefaultCompression) // DefaultCompression 是合法级别，不会出错
		return d
	}}
	deflateReaders = sync.Pool{New: func() any {
		i := &pooledInflater{src: bytes.NewReader(nil)}
		i.r = flate.NewReader(i.src)
		return i
	}}
)

// pooledDeflater 是池子里的一个压缩器：w 总是写进 sink，sink 再转到这一次调用的输出缓冲区。
type pooledDeflater struct {
	w    *flate.Writer
	sink deflateSink
}

// deflateSink 把压缩器的输出转进当前调用的缓冲区；两次调用之间 out 为 nil。
type deflateSink struct{ out *bytes.Buffer }

func (s *deflateSink) Write(p []byte) (int, error) { return s.out.Write(p) }

// pooledInflater 是池子里的一个解压器：r 总是读 src，src 每次换成这一次的输入。
type pooledInflater struct {
	src *bytes.Reader // 实现 io.ByteReader：flate 直接读它，不再包一层 bufio
	r   io.ReadCloser
}

func (deflateCompressor) Compress(data []byte) ([]byte, error) {
	var out bytes.Buffer
	d := deflateWriters.Get().(*pooledDeflater)
	d.sink.out = &out
	d.w.Reset(&d.sink)
	_, err := d.w.Write(data)
	if err == nil {
		err = d.w.Close()
	}
	d.sink.out = nil
	deflateWriters.Put(d)
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (deflateCompressor) Decompress(data []byte, limit int) ([]byte, error) {
	i := deflateReaders.Get().(*pooledInflater)
	i.src.Reset(data)
	if err := i.r.(flate.Resetter).Reset(i.src, nil); err != nil {
		i.src.Reset(nil)
		deflateReaders.Put(i)
		return nil, fmt.Errorf("relay: inflate state: %w", err)
	}
	out, err := io.ReadAll(io.LimitReader(i.r, int64(limit)+1))
	i.src.Reset(nil)
	deflateReaders.Put(i)
	if err != nil {
		return nil, fmt.Errorf("relay: inflate state: %w", err)
	}
	if len(out) > limit {
		return nil, fmt.Errorf("relay: inflated state exceeds %d bytes", limit)
	}
	return out, nil
}

type noCompression struct{}

func (noCompression) ID() byte { return compressionNone }

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
	candidate bool
	snapSeq   uint64    // 最近发出（或从 YouAreHost 接过来）的快照 seq；下一份是它加 1
	snapAt    time.Time // 最近一次发出快照的时刻；零值表示当上房主之后还没发过
	// snapStarted 是最近一份快照发起时取的号（hostTerm 里加 1，只增不减）；snapSent 是已分配 seq 的那些份里最大的号。
	// 号比 snapSent 小的一份是在已发出的一份之前发起的，状态更旧，分配 seq 时丢弃（见 hostTerm）。
	snapStarted uint64
	snapSent    uint64
	autoAt      time.Time // 最近一次定时推送的尝试（State 返回 nil、压缩后太大也算），下一次定时推送从它和 snapAt 的较晚者算
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

// OnMemberPresence 注册“成员上报了前台 / 后台”的回调（只有房主收到；换 host 后新房主收到各成员重发的状态）。
// 要不要据此调 SetSuccessors 由游戏决定。
func (c *Client) OnMemberPresence(fn func(MemberPresence)) {
	c.mu.Lock()
	c.h.memberPresence = fn
	c.mu.Unlock()
}

// SetForeground 记下自己在前台（true）还是后台（false），并经 relay 告诉当前房主（RelayOpPresence）。Unity 可以在
// OnApplicationPause 里调。状态记在 SDK 里：没有连接时返回 ErrNotConnected，状态仍然记下，下次连上时发出；之后每次
// 收到 HostChanged、每次连上都自动重发。从没调过时 SDK 不发。房主自己发的 relay 丢弃（降为成员后由重发补上）。
func (c *Client) SetForeground(foreground bool) error {
	c.mu.Lock()
	c.presence = presenceBackground
	if foreground {
		c.presence = presenceForeground
	}
	c.mu.Unlock()
	return c.SendControl(wire.RelayOpPresence, 0, wire.EncodeRelayPresence(foreground))
}

// resendPresence 在收到 HostChanged、连上之后重发记下的前台状态；没设过就不发。发不出去（连接刚断）不报错：
// 下次连上还会发。
func (c *Client) resendPresence() {
	c.mu.Lock()
	presence := c.presence
	c.mu.Unlock()
	if presence != presenceUnset {
		_ = c.SendControl(wire.RelayOpPresence, 0, wire.EncodeRelayPresence(presence == presenceForeground))
	}
}

// presenceState 是 SetForeground 记下的状态。
type presenceState byte

const (
	presenceUnset presenceState = iota
	presenceForeground
	presenceBackground
)

// Role 返回 SDK 记录的当前角色。
func (c *Client) Role() Role {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Role{Slot: c.role.Slot, Host: c.role.Host, Candidate: c.host.candidate, HostSlot: c.role.HostSlot, Epoch: c.role.Epoch}
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
// SnapshotMinInterval 的不发（ErrSnapshotTooSoon）；压缩后超过 SnapshotMaxBytes 的不发（ErrStateTooBig）；压缩期间
// 换了连接或房主任期、或者比它晚发起的一份已经发出的不发（ErrSnapshotStale，见 hostTerm）。relay 仍可能拒收
// （例如它的 snapshot_min_interval 比 SDK 的大），经 OnRejected 通知。state 应是调用时刻的状态：SDK 只能把这一份绑定到调用时的任期。
func (c *Client) Snapshot(state []byte) error {
	term, err := c.hostTerm()
	if err != nil {
		return err
	}
	return c.snapshotIn(term, state)
}

// hostTerm 是一份快照所属的连接与房主任期（epoch）。取状态、压缩都在锁外，可能很慢；期间房主可能换了一圈
// （A→B→A，epoch 0→2）或断线重连。旧任期的状态拿新任期的 seq 发出去，relay 会把它当成新任期最新的一份存下，下一任
// 房主恢复的就是旧状态（v1.54.0 审查 RV-20261118-02）。所以准备之前记下任期，分配 seq 之前在同一把锁里核对：
// 变了就丢弃，不发、不占 seq（与 TS / C# SDK 相同）。核对通过后 seq 已按这一任期分配，之后再换任期的话，relay
// 按 seq 递增（新任期的 seq 不低于 epoch<<32，见 seqFloor）拒收它。
//
// 同一任期里也可以并发推（游戏从几个 goroutine 调 Snapshot，或定时快照与手动的重叠）：先发起的一份若取状态、压缩更慢，
// 会拿到更大的 seq，盖掉 relay 已存的、后发起的较新状态。所以每份在发起时取一个递增的号（ticket），分配 seq 时同样在锁里
// 核对：比它晚发起的一份已经分配过 seq，就丢弃这一份（ErrSnapshotStale），并发推送以发起顺序为准、后发起的胜出
// （维护者 2026-11-18 定，与 TS / C# SDK 相同）。
type hostTerm struct {
	sess   *session
	epoch  uint64
	ticket uint64
}

// hostTerm 返回此刻的连接与房主任期，并给这一份取发起号：不是已就绪的房主时 ErrNotRoomHost，没有连接时 ErrNotConnected。
func (c *Client) hostTerm() (hostTerm, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case !c.role.Host:
		return hostTerm{}, ErrNotRoomHost
	case c.current == nil:
		return hostTerm{}, ErrNotConnected
	}
	c.host.snapStarted++
	return hostTerm{sess: c.current, epoch: c.role.Epoch, ticket: c.host.snapStarted}, nil
}

// snapshotIn 压缩 state，核对它仍属于 term，分配 seq 后在 term 的连接上发出。
func (c *Client) snapshotIn(term hostTerm, state []byte) error {
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
	case c.current != term.sess || c.role.Epoch != term.epoch:
		c.mu.Unlock()
		return ErrSnapshotStale
	case term.ticket < c.host.snapSent: // 晚发起的一份已经发出
		c.mu.Unlock()
		return ErrSnapshotStale
	case !c.host.snapAt.IsZero() && now.Sub(c.host.snapAt) < c.opts.SnapshotMinInterval:
		c.mu.Unlock()
		return ErrSnapshotTooSoon
	}
	c.host.snapSeq++
	c.host.snapAt = now
	c.host.snapSent = term.ticket
	seq := c.host.snapSeq
	c.mu.Unlock()
	frame, err := encodeFrame(Frame{Channel: wire.RelayControl, MsgID: wire.RelayOpSnapshot,
		Payload: wire.EncodeRelaySnapshot(wire.RelaySnapshot{Seq: seq, Data: data})}, c.frameLimit)
	if err != nil {
		return err
	}
	return c.write(term.sess, frame)
}

// compressState 把状态打成线上的字节：[压缩方式标识 1B][压缩后的字节]；空状态就是空的（不带标识，收到时也当空状态）。
// 上限 SnapshotMaxBytes 算上标识这 1 个字节（relay 的 snapshot_max_bytes 管的就是这些字节）。
func (c *Client) compressState(state []byte) ([]byte, error) {
	if len(state) == 0 {
		return nil, nil
	}
	packed, err := c.opts.Compressor.Compress(state)
	if err != nil {
		return nil, err
	}
	if 1+len(packed) > c.opts.SnapshotMaxBytes {
		return nil, ErrStateTooBig
	}
	return append([]byte{c.opts.Compressor.ID()}, packed...), nil
}

// decompressState 是 compressState 的反面：按标识选解压方式（自己的 Compressor 优先，再是内置的两种），解出的字节
// 不超过 limit。标识不认识返回 ErrUnknownCompression；标识认识、内容解不开返回解压错误。
func (c *Client) decompressState(data []byte, limit int) ([]byte, error) {
	if len(data) == 0 {
		return []byte{}, nil
	}
	id := data[0]
	var method Compressor
	switch {
	case id == c.opts.Compressor.ID():
		method = c.opts.Compressor
	case id == compressionDeflate:
		method = Deflate
	case id == compressionNone:
		method = NoCompression
	default:
		return nil, fmt.Errorf("%w: marker %d (this client compresses with %d)", ErrUnknownCompression, id, c.opts.Compressor.ID())
	}
	state, err := method.Decompress(data[1:], limit)
	if err != nil {
		return nil, fmt.Errorf("relay: decompress state (method %d): %w", id, err)
	}
	return state, nil
}

// autoSnapshot 是当房主时的定时快照（心跳 goroutine 上调用）：Options.SnapshotState 返回 nil 时这一轮不推。
// 任期在向游戏要状态之前记下（SnapshotState 可能很慢，期间可能换了任期，见 hostTerm）。
func (c *Client) autoSnapshot() {
	c.mu.Lock()
	c.host.autoAt = time.Now()
	c.mu.Unlock()
	term, err := c.hostTerm()
	if err != nil {
		return
	}
	state := c.opts.SnapshotState()
	if state != nil {
		_ = c.snapshotIn(term, state)
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
		c.role.Slot, c.role.Host, c.role.HostSlot, c.role.Epoch = f.Slot, false, 0, offer.Epoch
		c.host.candidate = true
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
			event.State, event.StateErr = c.decompressState(offer.State, MaxStateBytes)
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
		c.role.Host, c.role.HostSlot, c.role.Epoch = false, 0, epoch // 还在选人
		c.host.candidate = false
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
		c.role.HostSlot, c.role.Host, c.role.Epoch = f.Slot, f.Slot == c.role.Slot, changed.Epoch
		c.host.candidate = false
		if c.role.Host && !wasHost {
			c.becomeHostLocked()
		}
		c.mu.Unlock()
		c.roleChanged()
		c.resendPresence() // 新房主不知道之前的上报：设过状态的人各自再报一次
		if h.hostChanged != nil {
			h.hostChanged(HostEvent{Epoch: changed.Epoch, HostSlot: f.Slot, Account: changed.Account})
		}
		return wire.RelayReasonNone, nil
	},
	wire.RelayOpMemberPresence: func(_ *Client, h handlers, f Frame) (wire.RelayReason, error) {
		foreground, err := wire.DecodeRelayPresence(f.Payload)
		if err != nil {
			return wire.RelayReasonProtocol, err
		}
		if h.memberPresence != nil {
			h.memberPresence(MemberPresence{Slot: f.Slot, Foreground: foreground})
		}
		return wire.RelayReasonNone, nil
	},
	wire.RelayOpHostResumed: func(c *Client, h handlers, f Frame) (wire.RelayReason, error) {
		epoch, err := wire.DecodeRelayEpoch(f.Payload)
		if err != nil {
			return wire.RelayReasonProtocol, err
		}
		c.mu.Lock()
		c.role.HostSlot, c.role.Epoch = f.Slot, epoch
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
