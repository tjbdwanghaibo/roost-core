// 快照恢复：恢复方式判定、取快照线程与共享、分块下发与续帧、失败通知 v2、历史钉住、分块编解码与 fuzz、客户端 FrameAssembler 收快照与续帧。

package lockstep

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// lanePacket 是按发送顺序记下的一个下行包。
type lanePacket struct {
	reliable bool
	data     []byte
}

// laneLog 按会话记下两条通道的下行包（保持先后），可让某会话的可靠发送失败。并发安全：分块与追帧页在后台发送。
type laneLog struct {
	mu             sync.Mutex
	packets        map[nettransport.SessionID][]lanePacket
	failReliable   map[nettransport.SessionID]int // 接下来失败的可靠发送次数（-1 一直失败）
	reliableCalls  int
	reliableLimit  int // MaxReliableBytes；0 不声明
	pacesReliable  bool
	reliableBefore func(nettransport.SessionID, []byte) error
}

func newLaneLog() *laneLog {
	return &laneLog{packets: make(map[nettransport.SessionID][]lanePacket), failReliable: make(map[nettransport.SessionID]int), pacesReliable: true}
}

func (l *laneLog) SendDatagram(_ context.Context, session nettransport.SessionID, packet []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.packets[session] = append(l.packets[session], lanePacket{data: append([]byte(nil), packet...)})
	return nil
}

func (l *laneLog) SendReliable(ctx context.Context, session nettransport.SessionID, packet []byte) error {
	l.mu.Lock()
	hook := l.reliableBefore
	l.mu.Unlock()
	if hook != nil {
		if err := hook(session, packet); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reliableCalls++
	if n := l.failReliable[session]; n != 0 {
		if n > 0 {
			l.failReliable[session] = n - 1
		}
		return errors.New("link down")
	}
	l.packets[session] = append(l.packets[session], lanePacket{reliable: true, data: append([]byte(nil), packet...)})
	return nil
}

func (l *laneLog) take(session nettransport.SessionID) []lanePacket {
	l.mu.Lock()
	defer l.mu.Unlock()
	packets := l.packets[session]
	l.packets[session] = nil
	return packets
}

func (l *laneLog) setFail(session nettransport.SessionID, n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failReliable[session] = n
}

// pacedLog / unpacedLog 让同一个 laneLog 声明或不声明“随链路放慢”与单条上限。
type pacedLog struct{ *laneLog }

func (p pacedLog) PacesReliableToLink() bool { return p.pacesReliable }
func (p pacedLog) MaxReliableBytes() int     { return p.reliableLimit }

// referenceProvider 模拟服务器参考模拟的检查点：快照帧取请求上界，内容 = 帧号重复 size 次，状态 hash 可推导。
type referenceProvider struct {
	size  int
	calls atomic.Int64
	fn    func(context.Context, SnapshotRequest) (Snapshot, error) // 非 nil 时取代缺省行为
}

func referenceData(frame FrameID, size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(uint32(frame) + uint32(i)*31)
	}
	return data
}

func referenceHash(frame FrameID) uint64 { return uint64(frame)*0x9E3779B97F4A7C15 + 1 }

func (p *referenceProvider) Snapshot(ctx context.Context, req SnapshotRequest) (Snapshot, error) {
	p.calls.Add(1)
	if p.fn != nil {
		return p.fn(ctx, req)
	}
	return Snapshot{Frame: req.MaxFrame, Data: referenceData(req.MaxFrame, p.size), StateHash: referenceHash(req.MaxFrame), HasStateHash: true}, nil
}

func newSnapshotMatch(t testing.TB, lanes *laneLog, provider SnapshotProvider, configure func(*MatchConfig)) *Match {
	t.Helper()
	config := MatchConfig{
		Sequencer:           SequencerConfig{Players: []PlayerID{1, 2}, MaxInputBytes: 16},
		RedundancyDepth:     3,
		CatchupBatchFrames:  10,
		CatchupSendWait:     testSendWait,
		HistoryRetainFrames: MinHistoryRetainFrames,
		Datagrams:           lanes,
		Reliable:            pacedLog{lanes},
		Snapshots:           SnapshotConfig{Provider: provider},
	}
	if configure != nil {
		configure(&config)
	}
	match, err := NewMatch(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(match.Close)
	return match
}

// awaitFetch 等在途的取快照返回（白盒：把结果放回缓冲通道），让后续 Tick 的走向确定。
func awaitFetch(t testing.TB, match *Match) {
	t.Helper()
	fetch := match.fetch
	if fetch == nil {
		t.Fatal("no snapshot fetch in flight")
	}
	select {
	case result := <-fetch.done:
		fetch.done <- result
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot provider did not return")
	}
}

// tickInputs 提交两席输入（载荷 = 帧号与席位）后切一帧。
func tickInputs(t testing.TB, match *Match) error {
	t.Helper()
	frame := match.NextFrame()
	for player := PlayerID(1); player <= 2; player++ {
		if _, err := match.SubmitInput(player, frame, []byte{byte(frame), byte(frame >> 8), byte(player)}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := match.Tick(context.Background())
	return err
}

// recoveringClient 是测试里的客户端：重组快照、按帧号连续应用，记下洞与失败通知。
type recoveringClient struct {
	receiver  *SnapshotReceiver
	next      FrameID
	frames    map[FrameID]Frame
	snapshots []*ReceivedSnapshot
	notice    *CatchupUnservableError
	holes     []string
}

func newRecoveringClient(next FrameID) *recoveringClient {
	return &recoveringClient{receiver: NewSnapshotReceiver(0), next: next, frames: make(map[FrameID]Frame)}
}

func (c *recoveringClient) feed(t testing.TB, packets []lanePacket) {
	t.Helper()
	for _, packet := range packets {
		switch {
		case IsSnapshotChunk(packet.data):
			if !packet.reliable {
				t.Fatal("snapshot chunk on the datagram lane")
			}
			snapshot, err := c.receiver.Accept(packet.data)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot != nil {
				c.snapshots = append(c.snapshots, snapshot)
				c.next = snapshot.Frame + 1
			}
		case IsCatchupNotice(packet.data):
			notice, err := DecodeCatchupUnservable(packet.data)
			if err != nil {
				t.Fatal(err)
			}
			c.notice = notice
		default:
			frames, err := DecodeBroadcast(packet.data)
			if err != nil {
				t.Fatal(err)
			}
			if c.receiver.Active() {
				continue // 快照传输期间的 C7 丢弃，快照之后的页会重发
			}
			for _, frame := range frames {
				if frame.ID < c.next {
					continue
				}
				if frame.ID > c.next {
					c.holes = append(c.holes, fmt.Sprintf("want %d got %d (reliable %v)", c.next, frame.ID, packet.reliable))
					c.next = frame.ID
				}
				c.frames[frame.ID] = frame
				c.next++
			}
		}
	}
}

// checkFrames 核对 [from, through] 每帧的输入与 tickInputs 提交的一致。
func (c *recoveringClient) checkFrames(t testing.TB, from, through FrameID) {
	t.Helper()
	for id := from; id <= through; id++ {
		frame, ok := c.frames[id]
		if !ok {
			t.Fatalf("frame %d missing (next %d)", id, c.next)
		}
		if len(frame.Inputs) != 2 || !bytes.Equal(frame.Inputs[1].Payload, []byte{byte(id), byte(id >> 8), 2}) {
			t.Fatalf("frame %d inputs %+v", id, frame.Inputs)
		}
	}
}

// 越界请求（起点已被裁出窗口）：配置了快照来源（auto）时不再终止，而是发快照、续帧、回到实时，帧号无洞。
// 红证据：修前同一场景 StartCatchup 返回 *CatchupUnservableError、下一 Tick 发 C9 通知（客户端终止）。
func TestMatchAutoRecoveryServesOutOfWindowRequestWithSnapshot(t *testing.T) {
	lanes := newLaneLog()
	provider := &referenceProvider{size: 3000}
	match := newSnapshotMatch(t, lanes, provider, nil)
	if match.recovery != RecoveryAuto {
		t.Fatalf("Recovery zero value with a provider resolved to %s, want auto", match.recovery)
	}
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	for range 200 {
		if err := tickInputs(t, match); err != nil {
			t.Fatal(err)
		}
	}
	lanes.take(101)
	start := match.History().FirstID()
	if start <= 5 {
		t.Fatalf("history start %d did not pass the request", start)
	}
	if err := match.StartCatchup(1, 5); err != nil {
		t.Fatalf("out-of-window request with a snapshot provider (auto): %v", err)
	}
	client := newRecoveringClient(5)
	if err := tickInputs(t, match); err != nil { // 发起取快照
		t.Fatal(err)
	}
	awaitFetch(t, match)
	for range 10 {
		if err := tickInputs(t, match); err != nil {
			t.Fatal(err)
		}
		client.feed(t, lanes.take(101))
	}
	if client.notice != nil {
		t.Fatalf("client received a recovery-failed notice: %v", client.notice)
	}
	if len(client.snapshots) != 1 || provider.calls.Load() != 1 {
		t.Fatalf("snapshots %d, provider calls %d", len(client.snapshots), provider.calls.Load())
	}
	snapshot := client.snapshots[0]
	if snapshot.Frame != 201 || !bytes.Equal(snapshot.Data, referenceData(201, 3000)) || !snapshot.HasStateHash || snapshot.StateHash != referenceHash(201) {
		t.Fatalf("snapshot frame %d, %d bytes, hash %v/%d", snapshot.Frame, len(snapshot.Data), snapshot.HasStateHash, snapshot.StateHash)
	}
	if match.CatchingUp(1) {
		t.Fatal("recovery did not return to live broadcasts")
	}
	if len(client.holes) != 0 {
		t.Fatalf("frame stream has holes after the snapshot: %v", client.holes)
	}
	head := match.History().Latest()
	if client.next != head+1 {
		t.Fatalf("client next %d, head %d", client.next, head)
	}
	client.checkFrames(t, snapshot.Frame+1, head)
}

// 判定表（设计 §3.2）：三种模式 × 窗口内 / 超阈值 / 越界；catchup 模式逐字节同现状。
func TestMatchRecoveryRouting(t *testing.T) {
	type outcome string
	const (
		pages    outcome = "pages"
		snapshot outcome = "snapshot"
		notice   outcome = "notice"
	)
	cases := []struct {
		mode      RecoveryMode
		provider  bool
		threshold int
		from      FrameID
		want      outcome
	}{
		{RecoveryCatchup, false, 0, 150, pages},
		{RecoveryCatchup, true, 10, 150, pages}, // catchup 模式不看阈值
		{RecoveryCatchup, false, 0, 5, notice},
		{RecoveryCatchup, true, 0, 5, notice},
		{RecoverySnapshot, true, 0, 195, snapshot},
		{RecoverySnapshot, true, 0, 5, snapshot},
		{RecoveryAuto, true, 0, 150, pages}, // 阈值 0：窗口内一律追帧
		{RecoveryAuto, true, 10, 195, pages},
		{RecoveryAuto, true, 10, 150, snapshot},
		{RecoveryAuto, true, 0, 5, snapshot},
		{RecoveryUnset, true, 0, 5, snapshot},
		{RecoveryUnset, false, 0, 5, notice},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/provider=%v/threshold=%d/from=%d", tc.mode, tc.provider, tc.threshold, tc.from), func(t *testing.T) {
			lanes := newLaneLog()
			var provider SnapshotProvider
			if tc.provider {
				provider = &referenceProvider{size: 100}
			}
			match := newSnapshotMatch(t, lanes, provider, func(c *MatchConfig) {
				c.Recovery = tc.mode
				c.Snapshots.AutoCatchupMaxFrames = tc.threshold
			})
			if err := match.Attach(1, 101); err != nil {
				t.Fatal(err)
			}
			for range 200 {
				if err := tickInputs(t, match); err != nil {
					t.Fatal(err)
				}
			}
			lanes.take(101)
			err := match.StartCatchup(1, tc.from)
			if tc.want == notice {
				var unservable *CatchupUnservableError
				if !errors.As(err, &unservable) || unservable.Reason != RecoveryHistoryTrimmed {
					t.Fatalf("StartCatchup = %v, want history_trimmed", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			_ = tickInputs(t, match)
			if match.fetch != nil {
				awaitFetch(t, match)
				_ = tickInputs(t, match)
			}
			var first []byte
			for _, packet := range lanes.take(101) {
				if packet.reliable {
					first = packet.data
					break
				}
			}
			var got outcome
			switch {
			case first == nil:
				t.Fatal("no reliable packet")
			case IsSnapshotChunk(first):
				got = snapshot
			case IsCatchupNotice(first):
				got = notice
			default:
				got = pages
			}
			if got != tc.want {
				t.Fatalf("route = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestMatchRecoveryConfigValidation(t *testing.T) {
	lanes := newLaneLog()
	provider := &referenceProvider{size: 1}
	base := func() MatchConfig {
		return MatchConfig{Sequencer: SequencerConfig{Players: []PlayerID{1}, MaxInputBytes: 8}, Datagrams: lanes, Reliable: pacedLog{lanes}}
	}
	invalid := map[string]func(*MatchConfig){
		"snapshot without provider": func(c *MatchConfig) { c.Recovery = RecoverySnapshot },
		"auto without provider":     func(c *MatchConfig) { c.Recovery = RecoveryAuto },
		"unknown mode":              func(c *MatchConfig) { c.Recovery = RecoveryAuto + 1; c.Snapshots.Provider = provider },
		"auto without reliable":     func(c *MatchConfig) { c.Snapshots.Provider = provider; c.Reliable = nil },
		"over hard limit":           func(c *MatchConfig) { c.Snapshots = SnapshotConfig{Provider: provider, MaxBytes: MaxSnapshotBytes + 1} },
		"negative threshold":        func(c *MatchConfig) { c.Snapshots = SnapshotConfig{Provider: provider, AutoCatchupMaxFrames: -1} },
		"negative pin":              func(c *MatchConfig) { c.Snapshots = SnapshotConfig{Provider: provider, PinFrames: -1} },
		"chunk over reliable limit": func(c *MatchConfig) {
			limited := newLaneLog()
			limited.reliableLimit = DefaultSnapshotChunkBytes
			c.Reliable = pacedLog{limited}
			c.Snapshots.Provider = provider
		},
	}
	for name, configure := range invalid {
		config := base()
		configure(&config)
		if _, err := NewMatch(config); !errors.Is(err, ErrMatchConfigInvalid) {
			t.Errorf("%s: NewMatch = %v, want ErrMatchConfigInvalid", name, err)
		}
	}
	// 零值：没配 Provider 是 catchup（现行行为），配了是 auto；缺省值按“随链路放慢”与否选每 Tick 预算。
	match, err := NewMatch(base())
	if err != nil || match.recovery != RecoveryCatchup || match.snapshots != nil {
		t.Fatalf("zero config: %v %s %v", err, match.recovery, match.snapshots)
	}
	config := base()
	config.HistoryRetainFrames = 1000
	config.Snapshots.Provider = provider
	match, err = NewMatch(config)
	if err != nil || match.recovery != RecoveryAuto {
		t.Fatalf("provider config: %v %s", err, match.recovery)
	}
	if s := match.snapshots; s.autoMax != 0 || s.maxBytes != 4<<20 || s.chunkBytes != 16<<10 || s.budget != 64<<10 || s.fetchTimeout != 2*time.Second || s.pinFrames != 1000 {
		t.Fatalf("defaults %+v", *s)
	}
	lanes.pacesReliable = false
	config.Reliable = pacedLog{lanes}
	if match, err = NewMatch(config); err != nil || match.snapshots.budget != 16<<10 {
		t.Fatalf("admission-only lane budget: %v %d", err, match.snapshots.budget)
	}
	config.Reliable = lanes // 不实现 ReliablePacer：同样按只准入
	if match, err = NewMatch(config); err != nil || match.snapshots.budget != 16<<10 {
		t.Fatalf("undeclared lane budget: %v %d", err, match.snapshots.budget)
	}
	lanes.pacesReliable = true
	lanes.reliableLimit = DefaultSnapshotChunkBytes + MaxSnapshotChunkHeaderBytes
	config.Reliable = pacedLog{lanes}
	if _, err = NewMatch(config); err != nil {
		t.Fatalf("chunk exactly at the reliable limit: %v", err)
	}
}

// Provider 在 Match 自己的 goroutine 上调用：永久阻塞（只响应 ctx）不拖慢 Tick；超时计失败后下一 Tick 重取；
// 多个会话同时等快照只调用一次；panic 被 recover 成失败。
func TestMatchSnapshotProviderRunsOffTheTick(t *testing.T) {
	lanes := newLaneLog()
	release := make(chan struct{})
	var blocked atomic.Bool
	blocked.Store(true)
	provider := &referenceProvider{size: 100}
	provider.fn = func(ctx context.Context, req SnapshotRequest) (Snapshot, error) {
		if blocked.Load() {
			select {
			case <-ctx.Done():
				return Snapshot{}, ctx.Err()
			case <-release:
			}
		}
		return Snapshot{Frame: req.MaxFrame, Data: []byte{1, 2, 3}}, nil
	}
	match := newSnapshotMatch(t, lanes, provider, func(c *MatchConfig) {
		c.Recovery = RecoverySnapshot
		c.CatchupSendWait = 0                             // 缺省 5ms：Tick 不等 Provider
		c.Snapshots.FetchTimeout = 300 * time.Millisecond // Tick 若等 Provider，5 个 Tick 至少 300ms
	})
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	if err := match.AttachSpectator(301); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		_ = tickInputs(t, match)
	}
	if err := match.StartCatchup(1, 1); err != nil {
		t.Fatal(err)
	}
	if err := match.SpectatorCatchup(301, 1); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	for range 5 {
		_ = tickInputs(t, match)
	}
	if elapsed := time.Since(began); elapsed > 150*time.Millisecond {
		t.Fatalf("5 ticks with a blocked provider took %v", elapsed)
	}
	for deadline := time.Now().Add(5 * time.Second); provider.calls.Load() == 0 && time.Now().Before(deadline); {
		runtime.Gosched()
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("two waiting sessions made %d provider calls, want 1 shared", calls)
	}
	time.Sleep(320 * time.Millisecond) // 过 FetchTimeout：下一 Tick 计一次失败，再下一 Tick 重取
	if err := tickInputs(t, match); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed-out fetch: %v", err)
	}
	if state := match.catchups[101]; state.failures != 1 || match.catchups[301].failures != 1 {
		t.Fatalf("failures %d/%d after one timeout", state.failures, match.catchups[301].failures)
	}
	blocked.Store(false)
	_ = tickInputs(t, match) // 重新发起
	for deadline := time.Now().Add(5 * time.Second); provider.calls.Load() < 2 && time.Now().Before(deadline); {
		runtime.Gosched()
	}
	if calls := provider.calls.Load(); calls != 2 {
		t.Fatalf("provider calls %d after retry", calls)
	}
	awaitFetch(t, match)
	_ = tickInputs(t, match)
	if match.catchups[101] != nil && match.catchups[101].failures != 0 {
		t.Fatal("successful fetch did not reset the failure count")
	}
	close(release)

	// panic：recover 成失败，进程不崩，下一 Tick 重取。
	lanes2 := newLaneLog()
	panicky := &referenceProvider{}
	panicky.fn = func(context.Context, SnapshotRequest) (Snapshot, error) { panic("business bug") }
	match2 := newSnapshotMatch(t, lanes2, panicky, func(c *MatchConfig) { c.Recovery = RecoverySnapshot })
	if err := match2.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	_ = tickInputs(t, match2)
	if err := match2.StartCatchup(1, 1); err != nil {
		t.Fatal(err)
	}
	_ = tickInputs(t, match2)
	awaitFetch(t, match2)
	if err := tickInputs(t, match2); err == nil || !errors.Is(err, errSnapshotProviderPanic) || !strings.Contains(err.Error(), "business bug") {
		t.Fatalf("panicking provider: %v", err)
	}
	_ = tickInputs(t, match2)
	for deadline := time.Now().Add(5 * time.Second); panicky.calls.Load() < 2 && time.Now().Before(deadline); {
		runtime.Gosched()
	}
	if panicky.calls.Load() != 2 {
		t.Fatalf("provider not retried after panic: %d calls", panicky.calls.Load())
	}
}

// 错误语义（设计 §4.3）：auto 超阈值取不到快照回退追帧；连续不可用达到上限发 snapshot_unavailable；
// 超大立即发 snapshot_too_large 且不重试；帧号越界计一次失败并重取。
func TestMatchSnapshotFailureSemantics(t *testing.T) {
	setup := func(t *testing.T, fn func(context.Context, SnapshotRequest) (Snapshot, error), configure func(*MatchConfig)) (*Match, *laneLog, *referenceProvider) {
		lanes := newLaneLog()
		provider := &referenceProvider{fn: fn}
		match := newSnapshotMatch(t, lanes, provider, func(c *MatchConfig) {
			c.CatchupMaxFailures = 3
			if configure != nil {
				configure(c)
			}
		})
		if err := match.Attach(1, 101); err != nil {
			t.Fatal(err)
		}
		for range 200 {
			_ = tickInputs(t, match)
		}
		lanes.take(101)
		return match, lanes, provider
	}
	drive := func(t *testing.T, match *Match, ticks int) []error {
		var errs []error
		for range ticks {
			if match.fetch != nil {
				awaitFetch(t, match)
			}
			if err := tickInputs(t, match); err != nil {
				errs = append(errs, err)
			}
		}
		return errs
	}
	unavailable := func(context.Context, SnapshotRequest) (Snapshot, error) { return Snapshot{}, ErrSnapshotUnavailable }

	t.Run("auto over threshold falls back to catch-up", func(t *testing.T) {
		match, lanes, provider := setup(t, unavailable, func(c *MatchConfig) { c.Snapshots.AutoCatchupMaxFrames = 20 })
		if err := match.StartCatchup(1, 160); err != nil {
			t.Fatal(err)
		}
		if errs := drive(t, match, 8); len(errs) != 0 {
			t.Fatalf("fallback reported errors: %v", errs)
		}
		client := newRecoveringClient(160)
		client.feed(t, lanes.take(101))
		if provider.calls.Load() != 1 || client.notice != nil || len(client.holes) != 0 || match.CatchingUp(1) {
			t.Fatalf("calls %d notice %v holes %v catching %v", provider.calls.Load(), client.notice, client.holes, match.CatchingUp(1))
		}
	})
	t.Run("send failures never switch to a snapshot", func(t *testing.T) {
		// §3.5：追帧页连续发不出去说明链路有问题，快照只会更大；auto 不因此换路，达到上限放弃且不通知。
		match, lanes, provider := setup(t, unavailable, nil)
		if err := match.StartCatchup(1, 160); err != nil {
			t.Fatal(err)
		}
		lanes.setFail(101, -1)
		errs := drive(t, match, 5)
		lanes.setFail(101, 0)
		if provider.calls.Load() != 0 || match.CatchingUp(1) || len(errs) != 3 || !strings.Contains(errs[2].Error(), "abandoned after 3 failures") {
			t.Fatalf("calls %d catching %v errs %v", provider.calls.Load(), match.CatchingUp(1), errs)
		}
		for _, packet := range lanes.take(101) {
			if packet.reliable {
				t.Fatal("abandoned catch-up sent something on the reliable lane")
			}
		}
	})
	t.Run("unavailable until the failure budget", func(t *testing.T) {
		match, lanes, provider := setup(t, unavailable, nil)
		if err := match.StartCatchup(1, 5); err != nil {
			t.Fatal(err)
		}
		errs := drive(t, match, 12)
		client := newRecoveringClient(5)
		client.feed(t, lanes.take(101))
		if provider.calls.Load() != 3 || client.notice == nil || client.notice.Reason != RecoverySnapshotUnavailable || match.CatchingUp(1) {
			t.Fatalf("calls %d notice %v catching %v", provider.calls.Load(), client.notice, match.CatchingUp(1))
		}
		var unservable *CatchupUnservableError
		if !errors.As(errors.Join(errs...), &unservable) || unservable.Reason != RecoverySnapshotUnavailable || !errors.Is(errors.Join(errs...), ErrSnapshotUnavailable) {
			t.Fatalf("tick errors: %v", errs)
		}
	})
	t.Run("too large is final", func(t *testing.T) {
		match, lanes, provider := setup(t, func(_ context.Context, req SnapshotRequest) (Snapshot, error) {
			return Snapshot{Frame: req.MaxFrame, Data: make([]byte, 1025)}, nil
		}, func(c *MatchConfig) { c.Snapshots.MaxBytes = 1024 })
		if err := match.StartCatchup(1, 5); err != nil {
			t.Fatal(err)
		}
		errs := drive(t, match, 6)
		client := newRecoveringClient(5)
		client.feed(t, lanes.take(101))
		if provider.calls.Load() != 1 || client.notice == nil || client.notice.Reason != RecoverySnapshotTooLarge || !errors.Is(errors.Join(errs...), ErrSnapshotTooLarge) {
			t.Fatalf("calls %d notice %v errs %v", provider.calls.Load(), client.notice, errs)
		}
	})
	t.Run("out-of-range frame and empty data are retried", func(t *testing.T) {
		var attempt atomic.Int64
		match, lanes, provider := setup(t, func(_ context.Context, req SnapshotRequest) (Snapshot, error) {
			switch attempt.Add(1) {
			case 1:
				return Snapshot{Frame: req.MaxFrame + 1, Data: []byte{1}}, nil
			case 2:
				return Snapshot{Frame: req.MaxFrame}, nil
			default:
				return Snapshot{Frame: req.MinFrame, Data: []byte{9}}, nil
			}
		}, nil)
		if err := match.StartCatchup(1, 5); err != nil {
			t.Fatal(err)
		}
		errs := drive(t, match, 30)
		client := newRecoveringClient(5)
		client.feed(t, lanes.take(101))
		if provider.calls.Load() != 3 || client.notice != nil || len(client.snapshots) != 1 || !errors.Is(errors.Join(errs...), ErrSnapshotInvalid) {
			t.Fatalf("calls %d notice %v snapshots %d errs %v", provider.calls.Load(), client.notice, len(client.snapshots), errs)
		}
		if len(client.holes) != 0 || match.CatchingUp(1) {
			t.Fatalf("holes %v catching %v", client.holes, match.CatchingUp(1))
		}
	})
}

// 分块下发：每 Tick 不超过预算；整批失败后从已确认偏移以同一传输编号重发；断线取消在途、重连换新编号。
func TestMatchSnapshotChunkPacingAndResend(t *testing.T) {
	lanes := newLaneLog()
	provider := &referenceProvider{size: 10_000}
	match := newSnapshotMatch(t, lanes, provider, func(c *MatchConfig) {
		c.Recovery = RecoverySnapshot
		c.Snapshots.ChunkBytes = 1000
		c.Snapshots.BytesPerTick = 3000
	})
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		_ = tickInputs(t, match)
	}
	lanes.take(101)
	if err := match.StartCatchup(1, 90); err != nil {
		t.Fatal(err)
	}
	_ = tickInputs(t, match)
	awaitFetch(t, match)
	chunksPerTick := func() (int, []SnapshotChunk) {
		_ = tickInputs(t, match)
		var chunks []SnapshotChunk
		for _, packet := range lanes.take(101) {
			if IsSnapshotChunk(packet.data) {
				chunk, err := DecodeSnapshotChunk(packet.data, 0)
				if err != nil {
					t.Fatal(err)
				}
				chunks = append(chunks, chunk)
			}
		}
		return len(chunks), chunks
	}
	n, first := chunksPerTick()
	if n != 3 || !first[0].First || first[0].Total != 10_000 || first[2].Offset != 2000 {
		t.Fatalf("first tick chunks %d %+v", n, first)
	}
	// 第二批第二块失败：已确认 1000 字节，下一 Tick 从 4000 同编号重发。
	var sent atomic.Int64
	lanes.reliableBefore = func(nettransport.SessionID, []byte) error {
		if sent.Add(1) == 2 {
			return errors.New("timeout")
		}
		return nil
	}
	if err := tickInputs(t, match); err == nil {
		t.Fatal("failed chunk batch not reported")
	}
	lanes.reliableBefore = nil
	lanes.take(101)
	n, resent := chunksPerTick()
	if n != 3 || resent[0].Offset != 4000 || resent[0].Transfer != first[0].Transfer {
		t.Fatalf("resend chunks %d first offset %d transfer %d/%d", n, resent[0].Offset, resent[0].Transfer, first[0].Transfer)
	}
	// 断线取消；重连后重新请求得到新编号。
	match.Detach(1)
	if match.CatchingUp(1) || match.fetch != nil {
		t.Fatal("detach left recovery state")
	}
	if err := match.Attach(1, 102); err != nil {
		t.Fatal(err)
	}
	if err := match.StartCatchup(1, 90); err != nil {
		t.Fatal(err)
	}
	_ = tickInputs(t, match)
	awaitFetch(t, match)
	_ = tickInputs(t, match)
	var renewed SnapshotChunk
	for _, packet := range lanes.take(102) {
		if IsSnapshotChunk(packet.data) {
			renewed, _ = DecodeSnapshotChunk(packet.data, 0)
			break
		}
	}
	if !renewed.First || renewed.Transfer <= first[0].Transfer {
		t.Fatalf("reconnect transfer %+v", renewed)
	}
}

// 历史钉住（设计 §6.4）：传输慢于窗口推进时快照帧之后的帧不被裁掉；超过 PinFrames 发 recovery_expired 并释放。
func TestMatchSnapshotPinsHistoryWithinBound(t *testing.T) {
	lanes := newLaneLog()
	// 检查点取请求下界（发起时窗口起点之前一帧）：快照帧之后的帧马上就会被正常窗口越过，必须靠钉住保留。
	provider := &referenceProvider{size: 5000}
	provider.fn = func(_ context.Context, req SnapshotRequest) (Snapshot, error) {
		return Snapshot{Frame: req.MinFrame, Data: referenceData(req.MinFrame, 5000)}, nil
	}
	match := newSnapshotMatch(t, lanes, provider, func(c *MatchConfig) {
		c.Recovery = RecoverySnapshot
		c.CatchupMaxFailures = 1 << 20
		c.Snapshots.ChunkBytes = 100
		c.Snapshots.BytesPerTick = 100 // 50 Tick 才传完
		c.Snapshots.PinFrames = 30
	})
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		_ = tickInputs(t, match)
	}
	lanes.take(101)
	if err := match.StartCatchup(1, 1); err != nil {
		t.Fatal(err)
	}
	_ = tickInputs(t, match)
	awaitFetch(t, match)
	_ = tickInputs(t, match) // 采用快照（帧 = 发起时窗口起点 - 1）
	snapshotFrame := match.catchups[101].snapshot.image.snapshot.Frame
	for range 25 {
		_ = tickInputs(t, match)
		if first := match.History().FirstID(); first > snapshotFrame+1 {
			t.Fatalf("history trimmed to %d past the pinned snapshot frame %d", first, snapshotFrame)
		}
	}
	if extra := match.History().Len() - MinHistoryRetainFrames; extra <= 0 || extra > 30 {
		t.Fatalf("pinned history holds %d frames beyond the window", extra)
	}
	var expired error
	for range 10 {
		if err := tickInputs(t, match); err != nil {
			expired = errors.Join(expired, err)
		}
	}
	var unservable *CatchupUnservableError
	if !errors.As(expired, &unservable) || unservable.Reason != RecoveryExpired {
		t.Fatalf("pin bound: %v", expired)
	}
	client := newRecoveringClient(1)
	client.receiver = NewSnapshotReceiver(0)
	for _, packet := range lanes.take(101) {
		if IsCatchupNotice(packet.data) {
			client.feed(t, []lanePacket{packet})
		}
	}
	if client.notice == nil || client.notice.Reason != RecoveryExpired || match.CatchingUp(1) {
		t.Fatalf("notice %v catching %v", client.notice, match.CatchingUp(1))
	}
	if got := match.History().Len(); got != MinHistoryRetainFrames {
		t.Fatalf("history after release = %d frames, want the window %d", got, MinHistoryRetainFrames)
	}
}

// 追帧途中被窗口越过：auto 改走快照，不通知客户端失败；catchup 模式照旧（TestMatchAutoRetentionOvertakesStalledCatchup）。
func TestMatchAutoRecoveryOvertakenCatchupSwitchesToSnapshot(t *testing.T) {
	lanes := newLaneLog()
	provider := &referenceProvider{size: 200}
	match := newSnapshotMatch(t, lanes, provider, func(c *MatchConfig) { c.CatchupMaxFailures = 1000 })
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	for range 2 * MinHistoryRetainFrames {
		_ = tickInputs(t, match)
	}
	from := match.History().FirstID()
	if err := match.StartCatchup(1, from); err != nil {
		t.Fatal(err)
	}
	lanes.setFail(101, -1)
	_ = tickInputs(t, match) // 页失败，Tick 末尾窗口越过游标
	lanes.setFail(101, 0)
	lanes.take(101)
	for range 6 {
		if match.fetch != nil {
			awaitFetch(t, match)
		}
		if err := tickInputs(t, match); errors.Is(err, ErrCatchupUnservable) {
			t.Fatalf("overtaken auto catch-up failed instead of switching: %v", err)
		}
	}
	client := newRecoveringClient(from)
	client.feed(t, lanes.take(101))
	if client.notice != nil || len(client.snapshots) != 1 || len(client.holes) != 0 || match.CatchingUp(1) {
		t.Fatalf("notice %v snapshots %d holes %v catching %v", client.notice, len(client.snapshots), client.holes, match.CatchingUp(1))
	}
	client.checkFrames(t, client.snapshots[0].Frame+1, match.History().Latest())
}

// 宿主强制快照：不论模式；没有 Provider 时明确拒绝；旁观者同样可用。
func TestMatchStartSnapshotIsHostForced(t *testing.T) {
	lanes := newLaneLog()
	plain := newSnapshotMatch(t, lanes, nil, nil)
	if err := plain.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	if err := plain.StartSnapshot(1); !errors.Is(err, ErrSnapshotNotConfigured) {
		t.Fatalf("StartSnapshot without provider: %v", err)
	}
	provider := &referenceProvider{size: 300}
	match := newSnapshotMatch(t, lanes, provider, func(c *MatchConfig) { c.Recovery = RecoveryCatchup })
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	if err := match.AttachSpectator(301); err != nil {
		t.Fatal(err)
	}
	for range 50 {
		_ = tickInputs(t, match)
	}
	lanes.take(101)
	lanes.take(301)
	if err := match.StartSnapshot(1); err != nil {
		t.Fatal(err)
	}
	if err := match.SpectatorSnapshot(301); err != nil {
		t.Fatal(err)
	}
	if err := match.StartCatchup(1, 40); err != nil { // 快照在途：追帧请求不改路
		t.Fatal(err)
	}
	_ = tickInputs(t, match)
	awaitFetch(t, match)
	for range 4 {
		_ = tickInputs(t, match)
	}
	for _, session := range []nettransport.SessionID{101, 301} {
		client := newRecoveringClient(1)
		client.feed(t, lanes.take(session))
		if len(client.snapshots) != 1 || client.notice != nil || len(client.holes) != 0 || client.next != match.History().Latest()+1 {
			t.Fatalf("session %d: snapshots %d notice %v holes %v next %d", session, len(client.snapshots), client.notice, client.holes, client.next)
		}
	}
	if provider.calls.Load() != 1 {
		t.Fatalf("seat and spectator made %d provider calls", provider.calls.Load())
	}
}

// 周期性断线—快照恢复的长局：钉住只在恢复期间多留帧，底层数组峰值有界。
func TestMatchLongRunPeriodicSnapshotRecoveryKeepsHistoryBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("long run")
	}
	lanes := newLaneLog()
	provider := &referenceProvider{size: 2000}
	match := newSnapshotMatch(t, lanes, provider, func(c *MatchConfig) {
		c.Snapshots.ChunkBytes = 500
		c.Snapshots.BytesPerTick = 500
	})
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	peak := 0
	recoveries := 0
	for tick := 1; tick <= 20_000; tick++ {
		if tick%1000 == 0 && tick < 20_000 {
			if err := match.StartCatchup(1, FrameID(tick-500)); err != nil {
				t.Fatal(err)
			}
			recoveries++
		}
		if match.fetch != nil {
			awaitFetch(t, match)
		}
		if err := tickInputs(t, match); err != nil {
			t.Fatal(err)
		}
		lanes.take(101)
		peak = max(peak, match.history.retainedCapacity)
	}
	if recoveries != 19 || provider.calls.Load() != 19 || match.CatchingUp(1) {
		t.Fatalf("recoveries %d provider calls %d", recoveries, provider.calls.Load())
	}
	if peak > 4*MinHistoryRetainFrames+256 || match.History().Len() != MinHistoryRetainFrames {
		t.Fatalf("peak backing array %d frames, final %d", peak, match.History().Len())
	}
}

func TestRecoveryFailureNoticeV2(t *testing.T) {
	for _, reason := range []RecoveryFailureReason{RecoveryHistoryTrimmed, RecoverySnapshotUnavailable, RecoverySnapshotTooLarge, RecoveryExpired} {
		from, start := FrameID(300), FrameID(70000)
		if reason != RecoveryHistoryTrimmed {
			from, start = 90000, 70000 // 非 history_trimmed 不要求 from < start
		}
		packet := EncodeRecoveryFailure(reason, from, start)
		notice, err := DecodeCatchupUnservable(packet)
		if err != nil || notice.Reason != reason || notice.From != from || notice.Start != start || !errors.Is(notice, ErrCatchupUnservable) {
			t.Fatalf("%s: %v %+v", reason, err, notice)
		}
		if !strings.Contains(notice.Error(), "from") || (reason != RecoveryHistoryTrimmed && !strings.Contains(notice.Error(), reason.String())) {
			t.Fatalf("error text %q", notice.Error())
		}
	}
	if !bytes.Equal(EncodeCatchupUnservable(300, 70000), EncodeRecoveryFailure(RecoveryHistoryTrimmed, 300, 70000)) {
		t.Fatal("EncodeCatchupUnservable is not history_trimmed")
	}
	for name, packet := range map[string][]byte{
		"v1":              {0xC9, 1, 1, 20},
		"reason 0":        {0xC9, 2, 0, 1, 20},
		"reason 5":        {0xC9, 2, 5, 1, 20},
		"trimmed from>=":  {0xC9, 2, 1, 20, 20},
		"from 0":          {0xC9, 2, 2, 0, 20},
		"trailing":        {0xC9, 2, 1, 1, 20, 0},
		"truncated":       {0xC9, 2, 1, 1},
		"frame overflows": append([]byte{0xC9, 2, 2}, binary.AppendUvarint(binary.AppendUvarint(nil, 1<<33), 1)...),
	} {
		if _, err := DecodeCatchupUnservable(packet); !errors.Is(err, ErrFrameCorrupt) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func snapshotChunks(t testing.TB, transfer uint64, frame FrameID, data []byte, chunk int, hash *uint64) [][]byte {
	t.Helper()
	image := &snapshotImage{snapshot: Snapshot{Frame: frame, Data: data}, digest: sha256.Sum256(data)}
	if hash != nil {
		image.snapshot.StateHash, image.snapshot.HasStateHash = *hash, true
	}
	var out [][]byte
	for offset := 0; offset < len(data); offset += chunk {
		out = append(out, image.chunk(transfer, offset, min(offset+chunk, len(data))))
	}
	return out
}

func TestSnapshotChunkCodecAndReassembly(t *testing.T) {
	data := referenceData(77, 10_000)
	hash := uint64(1<<63 + 5)
	chunks := snapshotChunks(t, 9, 77, data, 4096, &hash)
	first, err := DecodeSnapshotChunk(chunks[0], 0)
	if err != nil || !first.First || first.Frame != 77 || first.Total != 10_000 || !first.HasStateHash || first.StateHash != hash || len(first.Data) != 4096 || first.Digest != sha256.Sum256(data) {
		t.Fatalf("first chunk %v %+v", err, first)
	}
	if encoded, err := EncodeSnapshotChunk(first); err != nil || !bytes.Equal(encoded, chunks[0]) {
		t.Fatalf("re-encode first chunk: %v", err)
	}
	// 重发（已确认偏移之前）丢弃、不报错；整份到齐后摘要核对通过；完成后同编号的重发也丢弃。
	receiver := NewSnapshotReceiver(0)
	sequence := [][]byte{chunks[0], chunks[0], chunks[1], chunks[0], chunks[1], chunks[2], chunks[2], chunks[0]}
	var got []*ReceivedSnapshot
	for _, packet := range sequence {
		snapshot, err := receiver.Accept(packet)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot != nil {
			got = append(got, snapshot)
		}
	}
	if len(got) != 1 || !bytes.Equal(got[0].Data, data) || got[0].StateHash != hash || receiver.Active() {
		t.Fatalf("reassembled %d snapshots", len(got))
	}
	// 新编号取代未完成的旧传输。
	receiver = NewSnapshotReceiver(0)
	if _, err := receiver.Accept(chunks[0]); err != nil {
		t.Fatal(err)
	}
	replacement := snapshotChunks(t, 10, 78, []byte("short"), 4096, nil)
	if snapshot, err := receiver.Accept(replacement[0]); err != nil || snapshot == nil || snapshot.Frame != 78 || snapshot.HasStateHash {
		t.Fatalf("replacement transfer: %v %+v", err, snapshot)
	}
	// 坏包。
	corrupt := map[string]func() *SnapshotReceiver{}
	_ = corrupt
	cases := []struct {
		name    string
		packets [][]byte
		want    error
	}{
		{"offset skip", [][]byte{chunks[0], chunks[2]}, ErrFrameCorrupt},
		{"new transfer mid-stream", [][]byte{snapshotChunks(t, 11, 1, data, 4096, nil)[1]}, ErrFrameCorrupt},
		{"digest mismatch", func() [][]byte {
			bad := snapshotChunks(t, 12, 1, []byte("abcd"), 4096, nil)
			bad[0][len(bad[0])-1] ^= 1
			return bad
		}(), ErrSnapshotCorrupt},
		{"over limit", [][]byte{snapshotChunks(t, 13, 1, make([]byte, DefaultMaxSnapshotBytes+1), DefaultMaxSnapshotBytes+1, nil)[0][:200]}, ErrFrameCorrupt},
	}
	for _, tc := range cases {
		receiver := NewSnapshotReceiver(0)
		var err error
		for _, packet := range tc.packets {
			if _, err = receiver.Accept(packet); err != nil {
				break
			}
		}
		if !errors.Is(err, tc.want) || receiver.Active() {
			t.Errorf("%s: %v (active %v)", tc.name, err, receiver.Active())
		}
	}
	for name, packet := range map[string][]byte{
		"magic":          {0xCB, 1, 1, 1, 1},
		"version":        {0xCA, 2, 1, 1, 1},
		"transfer 0":     {0xCA, 1, 0, 1, 1},
		"empty":          {0xCA, 1, 1, 1},
		"total 0":        append([]byte{0xCA, 1, 1, 0, 1, 0, 0}, make([]byte, 33)...),
		"flags reserved": append([]byte{0xCA, 1, 1, 0, 1, 1, 2}, make([]byte, 33)...),
		"short digest":   append([]byte{0xCA, 1, 1, 0, 1, 1, 0}, make([]byte, 31)...),
		"first > total":  append([]byte{0xCA, 1, 1, 0, 1, 1, 0}, make([]byte, 34)...),
		"offset > limit": append([]byte{0xCA, 1, 1}, binary.AppendUvarint(nil, DefaultMaxSnapshotBytes)...),
	} {
		if _, err := DecodeSnapshotChunk(packet, 0); !errors.Is(err, ErrFrameCorrupt) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	for _, chunk := range []SnapshotChunk{{Offset: 0, Total: 1, Data: []byte{1}}, {Transfer: 1, Offset: 0, Total: 0, Data: []byte{1}}, {Transfer: 1, Offset: 5}} {
		if _, err := EncodeSnapshotChunk(chunk); !errors.Is(err, ErrFrameCorrupt) {
			t.Errorf("EncodeSnapshotChunk(%+v) = %v", chunk, err)
		}
	}
}

// FuzzDecodeSnapshotChunk：任意字节不 panic、只报 ErrFrameCorrupt；能解出的分块重新编码后再解码得到同一分块
// （uvarint 允许非最短编码，所以比较语义而不是字节；语料里留着一条非最短编码的样本）。
func FuzzDecodeSnapshotChunk(f *testing.F) {
	hash := uint64(42)
	for _, packet := range snapshotChunks(f, 3, 9, referenceData(9, 300), 128, &hash) {
		f.Add(packet)
	}
	f.Add([]byte{0xCA, 1, 1, 0})
	f.Fuzz(func(t *testing.T, packet []byte) {
		chunk, err := DecodeSnapshotChunk(packet, 1<<16)
		if err != nil {
			if !errors.Is(err, ErrFrameCorrupt) {
				t.Fatalf("non-corrupt error %v", err)
			}
			return
		}
		encoded, err := EncodeSnapshotChunk(chunk)
		if err != nil {
			t.Fatalf("decoded chunk does not re-encode: %v", err)
		}
		again, err := DecodeSnapshotChunk(encoded, 1<<16)
		if err != nil || again.Transfer != chunk.Transfer || again.Offset != chunk.Offset || again.First != chunk.First || again.Frame != chunk.Frame ||
			again.Total != chunk.Total || again.StateHash != chunk.StateHash || again.HasStateHash != chunk.HasStateHash || again.Digest != chunk.Digest || !bytes.Equal(again.Data, chunk.Data) {
			t.Fatalf("round trip: %v\n%x\n%x", err, packet, encoded)
		}
	})
}

// FuzzSnapshotReassembly：任意切块、重发与乱序注入下，重组器要么交出与原文一致的快照，要么报坏包，绝不交出错数据。
func FuzzSnapshotReassembly(f *testing.F) {
	f.Add([]byte("hello snapshot"), uint8(4), []byte{0, 1, 1, 2, 3})
	f.Add(referenceData(3, 700), uint8(100), []byte{0, 2, 1, 3, 4, 5, 6, 7})
	f.Fuzz(func(t *testing.T, data []byte, chunk uint8, order []byte) {
		if len(data) == 0 || chunk == 0 {
			return
		}
		chunks := snapshotChunks(t, 5, 12, data, int(chunk), nil)
		receiver := NewSnapshotReceiver(1 << 16)
		for _, index := range order {
			packet := chunks[int(index)%len(chunks)]
			snapshot, err := receiver.Accept(packet)
			if err != nil {
				if !errors.Is(err, ErrFrameCorrupt) {
					t.Fatalf("unexpected error %v", err)
				}
				return
			}
			if snapshot != nil {
				if !bytes.Equal(snapshot.Data, data) || snapshot.Frame != 12 {
					t.Fatal("reassembled snapshot differs from the original")
				}
				return
			}
		}
	})
}

// 新增指标名钉住并在 OBSERVABILITY.md 登记（与 TestMatchPromiseMetricNamesArePinned 同一约束）。
func TestMatchSnapshotMetricNames(t *testing.T) {
	previous := metrics.DefaultRegistry()
	metrics.SetDefaultRegistry(metrics.NewRegistry())
	t.Cleanup(func() { metrics.SetDefaultRegistry(previous) })
	lanes := newLaneLog()
	match := newSnapshotMatch(t, lanes, &referenceProvider{size: 64}, nil)
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		_ = tickInputs(t, match)
	}
	if err := match.StartCatchup(1, 1); err != nil {
		t.Fatal(err)
	}
	_ = tickInputs(t, match)
	awaitFetch(t, match)
	_ = tickInputs(t, match)
	seen := map[string]bool{}
	for _, metric := range metrics.Snapshot() {
		seen[metric.Name] = true
	}
	for _, name := range []string{MetricSnapshot, MetricSnapshotBytes} {
		if !seen[name] {
			t.Errorf("%s not emitted", name)
		}
	}
	if MetricSnapshot != "lockstep.snapshot.total" || MetricSnapshotBytes != "lockstep.snapshot.bytes.total" || MetricSnapshotFailed != "lockstep.snapshot.failed.total" {
		t.Fatal("snapshot metric names changed")
	}
}

// 没有会话再等快照（断线、关闭）时取消在途的取快照：Provider 的 ctx 被取消，结果不挂在 Match 上。
func TestMatchSnapshotFetchIsCancelledWhenNobodyWaits(t *testing.T) {
	for _, end := range []string{"detach", "close"} {
		t.Run(end, func(t *testing.T) {
			lanes := newLaneLog()
			cancelled := make(chan error, 1)
			provider := &referenceProvider{fn: func(ctx context.Context, _ SnapshotRequest) (Snapshot, error) {
				<-ctx.Done()
				cancelled <- ctx.Err()
				return Snapshot{}, ctx.Err()
			}}
			match := newSnapshotMatch(t, lanes, provider, func(c *MatchConfig) {
				c.Recovery = RecoverySnapshot
				c.Snapshots.FetchTimeout = time.Hour
			})
			if err := match.Attach(1, 101); err != nil {
				t.Fatal(err)
			}
			_ = tickInputs(t, match)
			if err := match.StartCatchup(1, 1); err != nil {
				t.Fatal(err)
			}
			_ = tickInputs(t, match)
			if match.fetch == nil {
				t.Fatal("fetch not launched")
			}
			if end == "detach" {
				match.Detach(1)
			} else {
				match.Close()
			}
			if match.fetch != nil {
				t.Fatal("fetch still held after the last waiter left")
			}
			select {
			case err := <-cancelled:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("provider ctx ended with %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("provider context was not cancelled")
			}
		})
	}
}

// detachedFailures 读 lockstep.snapshot.failed.total{reason="detached"} 的当前值。
func detachedFailures() int64 {
	for _, metric := range metrics.Snapshot() {
		if metric.Name == MetricSnapshotFailed && metric.Labels["reason"] == "detached" {
			return metric.Value
		}
	}
	return 0
}

// 快照恢复进行中的会话被 Detach（断线、换连接、旁观者离开）计一次 failed{reason="detached"}（维护者 2026-10-20 决定 A）：
// 否则独立 Gate 下“快照传到一半被断开、重连从头再传”的循环在失败指标上完全看不见。
// 等快照、传快照、快照之后续帧三个阶段都算；普通追帧、已在发失败通知的恢复、Match.Close 不算。
func TestMatchSnapshotRecoveryDetachIsCounted(t *testing.T) {
	previous := metrics.DefaultRegistry()
	t.Cleanup(func() { metrics.SetDefaultRegistry(previous) })
	// olderCheckpoint 交出窗口起点处的快照，让快照之后有几页续帧。
	olderCheckpoint := func(_ context.Context, req SnapshotRequest) (Snapshot, error) {
		frame := req.MinFrame + 1
		return Snapshot{Frame: frame, Data: referenceData(frame, 2000), StateHash: referenceHash(frame), HasStateHash: true}, nil
	}
	var lanes *laneLog
	setup := func(t *testing.T, provider *referenceProvider, spectator bool) (*Match, func()) {
		t.Helper()
		metrics.SetDefaultRegistry(metrics.NewRegistry())
		lanes = newLaneLog()
		match := newSnapshotMatch(t, lanes, provider, func(c *MatchConfig) {
			c.Snapshots.ChunkBytes = 500
			c.Snapshots.BytesPerTick = 500
			c.Snapshots.FetchTimeout = time.Hour
		})
		if err := match.Attach(1, 101); err != nil {
			t.Fatal(err)
		}
		detach := func() { match.Detach(1) }
		if spectator {
			if err := match.AttachSpectator(201); err != nil {
				t.Fatal(err)
			}
			detach = func() { match.DetachSpectator(201) }
		}
		for range 100 {
			_ = tickInputs(t, match)
		}
		return match, detach
	}
	request := func(t *testing.T, match *Match, spectator bool, from FrameID) {
		t.Helper()
		var err error
		if spectator {
			err = match.SpectatorCatchup(201, from)
		} else {
			err = match.StartCatchup(1, from)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	session := func(spectator bool) nettransport.SessionID {
		if spectator {
			return 201
		}
		return 101
	}

	for _, phase := range []string{"waiting", "transferring", "recovered"} {
		for _, spectator := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/spectator=%v", phase, spectator), func(t *testing.T) {
				provider := &referenceProvider{fn: olderCheckpoint}
				match, detach := setup(t, provider, spectator)
				request(t, match, spectator, 1) // 起点 1 早已裁出 64 帧窗口：auto 走快照
				_ = tickInputs(t, match)        // 发起取快照
				state := match.catchups[session(spectator)]
				if phase != "waiting" {
					awaitFetch(t, match)
					_ = tickInputs(t, match) // 收到快照、发第一批分块
				}
				if phase == "recovered" {
					for range 20 {
						if state.recovered {
							break
						}
						_ = tickInputs(t, match)
					}
				}
				switch {
				case state == nil || match.catchups[session(spectator)] != state:
					t.Fatal("recovery state gone before detach")
				case phase == "waiting" && (state.snapshot == nil || state.snapshot.image != nil):
					t.Fatalf("not waiting for the snapshot: %+v", state.snapshot)
				case phase == "transferring" && (state.snapshot == nil || state.snapshot.image == nil):
					t.Fatalf("not transferring: %+v", state.snapshot)
				case phase == "recovered" && !state.recovered:
					t.Fatal("never reached the continuation pages after the snapshot")
				}
				detach()
				if got := detachedFailures(); got != 1 {
					t.Fatalf("lockstep.snapshot.failed.total{reason=detached} = %d after detaching a session %s, want 1", got, phase)
				}
			})
		}
	}

	t.Run("plain catch-up is not a snapshot recovery", func(t *testing.T) {
		match, detach := setup(t, &referenceProvider{size: 2000}, false)
		request(t, match, false, match.NextFrame()-20) // 窗口内：auto 追帧
		_ = tickInputs(t, match)
		if state := match.catchups[101]; state == nil || state.snapshot != nil || state.recovered {
			t.Fatalf("want a plain catch-up, got %+v", state)
		}
		detach()
		if got := detachedFailures(); got != 0 {
			t.Fatalf("detached = %d for a plain catch-up, want 0", got)
		}
	})

	t.Run("failed recovery is counted once", func(t *testing.T) {
		provider := &referenceProvider{fn: func(context.Context, SnapshotRequest) (Snapshot, error) {
			return Snapshot{Frame: 1, Data: make([]byte, DefaultMaxSnapshotBytes+1)}, nil
		}}
		match, detach := setup(t, provider, false)
		match.sendWait = time.Millisecond // Tick 不等停住的通知（白盒：testSendWait 是一分钟）
		// 失败通知停在发送里，恢复状态留在“待通知”阶段直到 Detach。
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		lanes.reliableBefore = func(_ nettransport.SessionID, packet []byte) error {
			if IsCatchupNotice(packet) {
				<-release
			}
			return nil
		}
		request(t, match, false, 1)
		_ = tickInputs(t, match)
		awaitFetch(t, match)
		_ = tickInputs(t, match) // too_large：已计 failed{too_large}，失败通知在途
		if state := match.catchups[101]; state == nil || state.unservable == nil {
			t.Fatalf("want a recovery whose failure notice is in flight, got %+v", state)
		}
		detach()
		if got := detachedFailures(); got != 0 {
			t.Fatalf("detached = %d after the recovery already failed, want 0", got)
		}
	})

	t.Run("match close is not a detach", func(t *testing.T) {
		match, _ := setup(t, &referenceProvider{fn: olderCheckpoint}, false)
		request(t, match, false, 1)
		_ = tickInputs(t, match)
		match.Close()
		if got := detachedFailures(); got != 0 {
			t.Fatalf("detached = %d after Match.Close, want 0", got)
		}
	})
}

// 只准入的可靠通道（不实现 ReliablePacer，例如独立 Gate）按 UnpacedBytesPerTick 节流，不用 BytesPerTick。
func TestMatchSnapshotUnpacedLaneUsesTheFixedBudget(t *testing.T) {
	lanes := newLaneLog()
	match := newSnapshotMatch(t, lanes, &referenceProvider{size: 10_000}, func(c *MatchConfig) {
		c.Reliable = lanes // 不声明随链路放慢
		c.Recovery = RecoverySnapshot
		c.Snapshots.ChunkBytes = 1000
		c.Snapshots.BytesPerTick = 8000
		c.Snapshots.UnpacedBytesPerTick = 2000
	})
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	_ = tickInputs(t, match)
	if err := match.StartCatchup(1, 1); err != nil {
		t.Fatal(err)
	}
	_ = tickInputs(t, match)
	awaitFetch(t, match)
	lanes.take(101)
	for tick := range 5 {
		_ = tickInputs(t, match)
		chunks := 0
		for _, packet := range lanes.take(101) {
			if IsSnapshotChunk(packet.data) {
				chunks++
			}
		}
		if chunks != 2 {
			t.Fatalf("tick %d sent %d chunks, want 2 (UnpacedBytesPerTick 2000 / ChunkBytes 1000)", tick, chunks)
		}
	}
	_ = tickInputs(t, match) // 10 KB 在 5 个 Tick 传完，下一 Tick 一页续帧追到头
	if match.CatchingUp(1) {
		t.Fatal("recovery did not finish after the transfer and one catch-up page")
	}
}

// ---- 客户端 FrameAssembler：收快照、恢复、续帧（设计 §7.1 / §7.3） ----

// receiveAll 依次投包，返回释放的帧号、交出的快照与第一个错误（出错即停）。
func receiveAll(assembler *FrameAssembler, packets ...[]byte) ([]FrameID, []*ReceivedSnapshot, error) {
	var released []FrameID
	var snapshots []*ReceivedSnapshot
	for _, packet := range packets {
		delivery, err := assembler.Receive(packet)
		for _, frame := range delivery.Frames {
			released = append(released, frame.ID)
		}
		if delivery.Snapshot != nil {
			snapshots = append(snapshots, delivery.Snapshot)
		}
		if err != nil {
			return released, snapshots, err
		}
	}
	return released, snapshots, nil
}

func framePackets(ids ...FrameID) [][]byte {
	packets := make([][]byte, 0, len(ids))
	for _, id := range ids {
		packets = append(packets, EncodeBroadcast([]Frame{{ID: id, Inputs: []Input{{Player: 1, Payload: []byte{byte(id)}}}}}))
	}
	return packets
}

// 收快照的完整流程：缺口 → 快照分块（传输中的 C7 丢弃、计入 Duplicates）→ 交出快照（摘要已核对）→
// 恢复前继续投包被拒 → ResumeAfterSnapshot 清空乱序缓冲、续于快照帧 +1 → 同编号的重发分块丢弃 → 续帧。
func TestFrameAssemblerReceivesSnapshotThenResumesAfterIt(t *testing.T) {
	assembler := NewFrameAssembler(0)
	released, _, err := receiveAll(assembler, framePackets(1, 2, 3, 10, 11)...)
	if err != nil || len(released) != 3 || !assembler.Gap() || assembler.Next() != 4 {
		t.Fatalf("before the snapshot: released %v, gap %v, next %d, err %v", released, assembler.Gap(), assembler.Next(), err)
	}
	data := referenceData(50, 10_000)
	hash := uint64(1<<63 | 50)
	chunks := snapshotChunks(t, 7, 50, data, 4096, &hash)
	duplicates := assembler.Duplicates()
	// 分块之间夹着恢复开始前发出、晚到的实时帧：传输进行中一律丢弃。
	released, snapshots, err := receiveAll(assembler, chunks[0], framePackets(4)[0], chunks[1], framePackets(51)[0])
	if err != nil || len(released) != 0 || len(snapshots) != 0 || !assembler.SnapshotInProgress() {
		t.Fatalf("mid-transfer: released %v, snapshots %d, in progress %v, err %v", released, len(snapshots), assembler.SnapshotInProgress(), err)
	}
	if assembler.Duplicates() != duplicates+2 || assembler.Next() != 4 {
		t.Fatalf("frames during the transfer: duplicates %d → %d, next %d", duplicates, assembler.Duplicates(), assembler.Next())
	}
	_, snapshots, err = receiveAll(assembler, chunks[2])
	if err != nil || len(snapshots) != 1 || assembler.SnapshotInProgress() {
		t.Fatalf("last chunk: snapshots %d, in progress %v, err %v", len(snapshots), assembler.SnapshotInProgress(), err)
	}
	snapshot := snapshots[0]
	if snapshot.Frame != 50 || !bytes.Equal(snapshot.Data, data) || !snapshot.HasStateHash || snapshot.StateHash != hash {
		t.Fatalf("snapshot frame %d, %d bytes, hash %v/%d", snapshot.Frame, len(snapshot.Data), snapshot.HasStateHash, snapshot.StateHash)
	}
	// 交出后、恢复前：两条入口都拒绝，调用方不能在旧状态上继续模拟。
	if _, err := assembler.Receive(framePackets(51)[0]); !errors.Is(err, ErrSnapshotNotApplied) {
		t.Fatalf("Receive before ResumeAfterSnapshot: %v", err)
	}
	if _, err := assembler.IngestFrames([]Frame{{ID: 4}}); !errors.Is(err, ErrSnapshotNotApplied) {
		t.Fatalf("IngestFrames before ResumeAfterSnapshot: %v", err)
	}
	assembler.ResumeAfterSnapshot(snapshot.Frame)
	if assembler.Next() != 51 || assembler.Gap() {
		t.Fatalf("after resume: next %d, gap %v (frames 10, 11 must be dropped with the buffer)", assembler.Next(), assembler.Gap())
	}
	// 服务端把超时但其实已写出的分块再发一次：同编号的重发丢弃，不报错、不再交出快照。
	released, snapshots, err = receiveAll(assembler, chunks[2], chunks[0])
	if err != nil || len(released) != 0 || len(snapshots) != 0 || assembler.SnapshotInProgress() {
		t.Fatalf("resent chunks after completion: released %v, snapshots %d, err %v", released, len(snapshots), err)
	}
	released, _, err = receiveAll(assembler, EncodeBroadcast([]Frame{{ID: 50}, {ID: 51}, {ID: 52}}), framePackets(53)[0])
	if err != nil || len(released) != 3 || released[0] != 51 || released[2] != 53 || assembler.Next() != 54 {
		t.Fatalf("pages after the snapshot: released %v, next %d, err %v", released, assembler.Next(), err)
	}
}

// 快照帧可以早于已应用的帧（snapshot 模式下 Provider 的检查点落后于客户端）：状态整体回到快照帧，之后的帧重新交出。
func TestFrameAssemblerSnapshotMayRewindTheCursor(t *testing.T) {
	assembler := NewFrameAssembler(0)
	if _, _, err := receiveAll(assembler, framePackets(1, 2, 3, 4, 5)...); err != nil {
		t.Fatal(err)
	}
	_, snapshots, err := receiveAll(assembler, snapshotChunks(t, 1, 2, []byte("state@2"), 4096, nil)...)
	if err != nil || len(snapshots) != 1 || snapshots[0].HasStateHash {
		t.Fatalf("snapshot: %d, %v", len(snapshots), err)
	}
	assembler.ResumeAfterSnapshot(2)
	released, _, err := receiveAll(assembler, EncodeBroadcast([]Frame{{ID: 3}, {ID: 4}, {ID: 5}, {ID: 6}}))
	if err != nil || len(released) != 4 || released[0] != 3 || assembler.Next() != 7 {
		t.Fatalf("frames after a rewinding snapshot: %v, next %d, %v", released, assembler.Next(), err)
	}
}

// C9 v2 的每种原因都原样交给调用方（errors.Is ErrCatchupUnservable 对所有原因成立），快照传输进行中也一样；
// 解码 Ingest 与 Receive 一致。
func TestFrameAssemblerSurfacesRecoveryFailureReason(t *testing.T) {
	for _, reason := range []RecoveryFailureReason{RecoveryHistoryTrimmed, RecoverySnapshotUnavailable, RecoverySnapshotTooLarge, RecoveryExpired} {
		from, start := FrameID(4), FrameID(70)
		if reason != RecoveryHistoryTrimmed {
			from = 90
		}
		notice := EncodeRecoveryFailure(reason, from, start)
		assembler := NewFrameAssembler(0)
		chunks := snapshotChunks(t, 2, 60, referenceData(60, 5000), 4096, nil)
		_, _, err := receiveAll(assembler, chunks[0], notice)
		var failed *CatchupUnservableError
		if !errors.As(err, &failed) || !errors.Is(err, ErrCatchupUnservable) || failed.Reason != reason || failed.From != from || failed.Start != start {
			t.Fatalf("%s via Receive mid-transfer: %v", reason, err)
		}
		if _, err := NewFrameAssembler(0).Ingest(notice); !errors.As(err, &failed) || failed.Reason != reason {
			t.Fatalf("%s via Ingest: %v", reason, err)
		}
	}
	if _, err := NewFrameAssembler(0).Receive([]byte{0xC9, 1, 1, 20}); !errors.Is(err, ErrFrameCorrupt) {
		t.Fatalf("C9 v1 accepted: %v", err)
	}
}

// 校验失败一律返回错误、不交出快照（调用方终止，不自动重试）；只会追帧的 Ingest 遇到分块明确报不支持。
func TestFrameAssemblerRejectsBadSnapshots(t *testing.T) {
	data := referenceData(30, 9000)
	chunks := snapshotChunks(t, 5, 30, data, 4096, nil)
	badDigest := snapshotChunks(t, 6, 30, data, 4096, nil)
	badDigest[2] = append([]byte(nil), badDigest[2]...)
	badDigest[2][len(badDigest[2])-1] ^= 1
	cases := []struct {
		name      string
		assembler *FrameAssembler
		packets   [][]byte
		want      error
	}{
		{"digest mismatch", NewFrameAssembler(0), badDigest, ErrSnapshotCorrupt},
		{"offset skip", NewFrameAssembler(0), [][]byte{chunks[0], chunks[2]}, ErrFrameCorrupt},
		{"new transfer mid-stream", NewFrameAssembler(0), [][]byte{chunks[1]}, ErrFrameCorrupt},
		{"over the client limit", NewFrameAssemblerLimits(0, 8000), [][]byte{chunks[0]}, ErrFrameCorrupt},
		{"truncated chunk", NewFrameAssembler(0), [][]byte{chunks[0][:20]}, ErrFrameCorrupt},
	}
	for _, tc := range cases {
		_, snapshots, err := receiveAll(tc.assembler, tc.packets...)
		if !errors.Is(err, tc.want) || len(snapshots) != 0 || tc.assembler.SnapshotInProgress() {
			t.Errorf("%s: err %v, snapshots %d, in progress %v", tc.name, err, len(snapshots), tc.assembler.SnapshotInProgress())
		}
	}
	if _, err := NewFrameAssembler(0).Ingest(chunks[0]); !errors.Is(err, ErrSnapshotUnsupported) || errors.Is(err, ErrFrameCorrupt) {
		t.Fatalf("Ingest on a snapshot chunk: %v", err)
	}
	mismatch := &SnapshotMismatchError{Frame: 30, Want: 1, Got: 2}
	if !errors.Is(mismatch, ErrSnapshotMismatch) || !strings.Contains(mismatch.Error(), "frame 30") {
		t.Fatalf("SnapshotMismatchError: %v", mismatch)
	}
}

// 连续性：Match（auto，越界请求）经可靠通道发快照与续帧页，客户端只用 FrameAssembler.Receive——
// 快照 → 续帧 → 实时，帧号无洞、输入载荷与提交的一致。
func TestFrameAssemblerRecoversFromAMatchSnapshot(t *testing.T) {
	lanes := newLaneLog()
	provider := &referenceProvider{size: 50_000} // 多于一个分块
	match := newSnapshotMatch(t, lanes, provider, nil)
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	assembler := NewFrameAssembler(0)
	restored := 0
	feed := func() {
		t.Helper()
		for _, packet := range lanes.take(101) {
			delivery, err := assembler.Receive(packet.data)
			if err != nil {
				t.Fatalf("Receive (next %d): %v", assembler.Next(), err)
			}
			for _, frame := range delivery.Frames {
				if len(frame.Inputs) != 2 || !bytes.Equal(frame.Inputs[1].Payload, []byte{byte(frame.ID), byte(frame.ID >> 8), 2}) {
					t.Fatalf("frame %d inputs %+v", frame.ID, frame.Inputs)
				}
			}
			if snapshot := delivery.Snapshot; snapshot != nil {
				if !bytes.Equal(snapshot.Data, referenceData(snapshot.Frame, 50_000)) || snapshot.StateHash != referenceHash(snapshot.Frame) {
					t.Fatalf("snapshot at %d does not match the provider", snapshot.Frame)
				}
				assembler.ResumeAfterSnapshot(snapshot.Frame)
				restored++
			}
		}
	}
	for range 5 {
		if err := tickInputs(t, match); err != nil {
			t.Fatal(err)
		}
		feed()
	}
	match.Detach(1) // 断线超过窗口
	for range 4 * MinHistoryRetainFrames {
		if err := tickInputs(t, match); err != nil {
			t.Fatal(err)
		}
	}
	if err := match.Attach(1, 101); err != nil {
		t.Fatal(err)
	}
	if err := match.StartCatchup(1, assembler.Next()); err != nil {
		t.Fatalf("out-of-window request: %v", err)
	}
	if err := tickInputs(t, match); err != nil {
		t.Fatal(err)
	}
	awaitFetch(t, match)
	for i := 0; match.CatchingUp(1) || i < 3; i++ {
		if i > 100 {
			t.Fatal("recovery did not finish in 100 Ticks")
		}
		if err := tickInputs(t, match); err != nil {
			t.Fatal(err)
		}
		feed()
	}
	if provider.calls.Load() != 1 || restored != 1 {
		t.Fatalf("provider calls %d, snapshots restored %d", provider.calls.Load(), restored)
	}
	if head := match.History().Latest(); assembler.Next() != head+1 || assembler.Gap() {
		t.Fatalf("client next %d (gap %v), head %d", assembler.Next(), assembler.Gap(), head)
	}
}

// 快照之前已有 frame+1、frame+2 进了乱序缓冲（恢复开始前的实时帧）：ResumeAfterSnapshot 清空整个缓冲，
// 服务端从 frame+1 重发的历史页按序续到头部，不会因为缓冲里的 frame+1 被当重复帧跳过而永久卡住。
// （P3 C# 实施时发现的同一问题；只清 <= frame 的写法在这里卡在 frame+1。）
func TestFrameAssemblerResumeDropsBufferedFramesPastTheSnapshot(t *testing.T) {
	assembler := NewFrameAssembler(0)
	if _, _, err := receiveAll(assembler, framePackets(1, 2, 21, 22)...); err != nil || assembler.Next() != 3 || !assembler.Gap() {
		t.Fatalf("before the snapshot: next %d, gap %v, %v", assembler.Next(), assembler.Gap(), err)
	}
	_, snapshots, err := receiveAll(assembler, snapshotChunks(t, 1, 20, []byte("state@20"), 4096, nil)...)
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("snapshot: %d, %v", len(snapshots), err)
	}
	assembler.ResumeAfterSnapshot(20)
	if assembler.Gap() || assembler.Next() != 21 {
		t.Fatalf("after resume: next %d, gap %v", assembler.Next(), assembler.Gap())
	}
	var released []FrameID
	for _, page := range [][]Frame{{{ID: 21}, {ID: 22}}, {{ID: 23}, {ID: 24}}} {
		got, _, err := receiveAll(assembler, EncodeBroadcast(page))
		if err != nil {
			t.Fatal(err)
		}
		released = append(released, got...)
	}
	if len(released) != 4 || released[0] != 21 || released[3] != 24 || assembler.Next() != 25 || assembler.Gap() {
		t.Fatalf("pages from frame+1: released %v, next %d, gap %v", released, assembler.Next(), assembler.Gap())
	}
}
