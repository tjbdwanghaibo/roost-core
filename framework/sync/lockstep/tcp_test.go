package lockstep_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/lockstep"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
)

// 这组用例把 Match 经 TCPSender 接到真实的嵌入 TCP 接入（gateway.TCPServer + TCPRuntime），
// 客户端是本机回环上的真实 socket，慢链路由客户端读得慢（或停读）造成。

const tcpLockstepMessageID = 50002

// tcpLockstepSeats 让每帧足够大（8 席 × 1 KiB 输入 ≈ 8 KiB），几百帧历史就有几 MiB，
// 超过嵌入 TCP 每连接出站的字节上限（默认约 2 MiB）。只有接上连接的席位收广播。
var tcpLockstepSeats = []lockstep.PlayerID{1, 2, 3, 4, 5, 6, 7, 8}

type tcpLockstepServer struct {
	server  *gateway.TCPServer
	runtime *gateway.TCPRuntime
	closed  chan gateway.TCPSessionClosed
}

// newTCPLockstepServer 起一个嵌入 TCP 接入：令牌就是会话 ID，players 给出令牌对应的玩家 ID。
func newTCPLockstepServer(t *testing.T, writeTimeout time.Duration, players map[string]int64) *tcpLockstepServer {
	t.Helper()
	return newTCPLockstepServerWith(t, writeTimeout, players, nil)
}

// newTCPLockstepServerWith 与 newTCPLockstepServer 相同，configure 非 nil 时在建服务器前改配置。
func newTCPLockstepServerWith(t *testing.T, writeTimeout time.Duration, players map[string]int64, configure func(*gateway.TCPConfig)) *tcpLockstepServer {
	t.Helper()
	config := gateway.DefaultTCPConfig()
	config.Addr = "127.0.0.1:0"
	config.WriteTimeout = writeTimeout
	if configure != nil {
		configure(&config)
	}
	runtime := gateway.NewTCPRuntime(nil, config.LoginTimeout)
	dispatch := func(context.Context, gateway.Session, uint32, uint32, wire.PayloadKind, []byte) (any, error) {
		return nil, nil
	}
	server, err := gateway.NewTCPServer(config, dispatch, gateway.AuthenticatorFunc(func(_ context.Context, token string, _ net.Addr) (gateway.Principal, error) {
		return gateway.Principal{PlayerID: players[token], SessionID: token}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.ConnectRuntime(runtime); err != nil {
		t.Fatal(err)
	}
	closed := make(chan gateway.TCPSessionClosed, 16)
	runtime.OnSessionClosed(func(event gateway.TCPSessionClosed) { closed <- event })
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Stop(ctx)
	})
	return &tcpLockstepServer{server: server, runtime: runtime, closed: closed}
}

// dial 完成握手并把客户端接收缓冲设成 readBuffer，返回已鉴权的连接。
func (s *tcpLockstepServer) dial(t *testing.T, token string, playerID int64, readBuffer int) net.Conn {
	t.Helper()
	connection, err := net.Dial("tcp", s.server.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	_ = connection.(*net.TCPConn).SetReadBuffer(readBuffer)
	if err := wire.Write(connection, []*wire.Packet{{Seq: 1, Payload: []byte(token)}}, 0); err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	ack, err := wire.Read(connection, 0)
	if err != nil || ack.MsgID != 0 || ack.Seq != 1 {
		t.Fatalf("authentication ack %+v: %v", ack, err)
	}
	_ = connection.SetReadDeadline(time.Time{})
	for deadline := time.Now().Add(2 * time.Second); s.runtime.ActiveSessions(playerID) == 0; {
		if time.Now().After(deadline) {
			t.Fatalf("session %s never registered", token)
		}
		time.Sleep(time.Millisecond)
	}
	return connection
}

// sessionClosed 报告这条会话是否已被服务端关闭（读掉已有的关闭事件，不等待）。
func (s *tcpLockstepServer) sessionClosed(sessionID string, seen map[string]bool) bool {
	for {
		select {
		case event := <-s.closed:
			seen[event.SessionID] = true
		default:
			return seen[sessionID]
		}
	}
}

// tcpLockstepSender 是被测的接线：实时通道只入队，追帧页入队并等写出。
func tcpLockstepSender(t *testing.T, runtime *gateway.TCPRuntime, sessions map[nettransport.SessionID]string) *lockstep.TCPSender {
	t.Helper()
	sender, err := lockstep.NewTCPSender(tcpLockstepMessageID, 0, func(id nettransport.SessionID) (string, bool) {
		session, ok := sessions[id]
		return session, ok
	}, runtime.PushLockstepSession, runtime.PushLockstepSessionWritten)
	if err != nil {
		t.Fatal(err)
	}
	return sender
}

// buildHistory 切 frames 帧满载输入的历史，此时还没有席位接上连接，不产生广播。
func buildHistory(t *testing.T, match *lockstep.Match, frames int) {
	t.Helper()
	input := bytes.Repeat([]byte{0x5a}, lockstep.MaxInputPayloadBytes)
	for range frames {
		for _, seat := range tcpLockstepSeats {
			if _, err := match.SubmitInput(seat, match.NextFrame(), input); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := match.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

// lockstepReader 在客户端连接上读 Lockstep 推送并交给 FrameAssembler；next 是下一帧期望的 ID。
type lockstepReader struct {
	next   atomic.Uint64
	failed atomic.Pointer[error]
	done   chan struct{}
}

// readLockstep 启动读协程：先等 start 关闭（停读阶段），每读一个包后睡 pause（限速）。
func readLockstep(connection net.Conn, start <-chan struct{}, pause time.Duration) *lockstepReader {
	reader := &lockstepReader{done: make(chan struct{})}
	reader.next.Store(1)
	go func() {
		defer close(reader.done)
		<-start
		assembler := lockstep.NewFrameAssembler(0)
		for {
			packet, err := wire.Read(connection, 0)
			if err != nil {
				return // 连接关闭：由用例自己判断是否应当关闭
			}
			if packet.Flags != wire.FlagPush|wire.FlagLockstep || packet.MsgID != tcpLockstepMessageID {
				continue
			}
			if _, err := assembler.Ingest(packet.Payload); err != nil {
				reader.failed.Store(&err)
				return
			}
			reader.next.Store(uint64(assembler.Next()))
			if pause > 0 {
				time.Sleep(pause)
			}
		}
	}()
	return reader
}

func (reader *lockstepReader) err() error {
	if failed := reader.failed.Load(); failed != nil {
		return *failed
	}
	return nil
}

// 慢链路上的追帧要随链路速度推进：每会话一页在途、等这一页写出才发下一页，连接出站积压始终只有一页。
// 只做准入时页面按每 Tick 一页灌进出站队列，几 MiB 的历史在链路读走之前就超过字节上限，连接被关闭、追帧失败。
// 客户端先停读 200ms（页面在途但写不出，未超过 CatchupSendTimeout），再每读一页歇 5ms 慢读。
func TestACatchUpOverEmbeddedTCPFollowsTheLinkInsteadOfOverflowingIt(t *testing.T) {
	const historyFrames = 1000 // ≈ 8 MiB，远超每连接约 2 MiB 的出站字节上限与 socket 缓冲
	tcp := newTCPLockstepServer(t, 10*time.Second, map[string]int64{"slow": 1})
	connection := tcp.dial(t, "slow", 1, 16<<10)
	match, err := lockstep.NewMatch(lockstep.MatchConfig{
		Sequencer:          lockstep.SequencerConfig{Players: tcpLockstepSeats, MaxInputBytes: lockstep.MaxInputPayloadBytes},
		RedundancyDepth:    1,
		CatchupSendTimeout: 10 * time.Second, // 停读阶段不超时：这里只看追帧是否随链路推进
		CatchupSendWait:    5 * time.Millisecond,
		Datagrams:          tcpLockstepSender(t, tcp.runtime, map[nettransport.SessionID]string{1: "slow"}),
		Reliable:           tcpLockstepSender(t, tcp.runtime, map[nettransport.SessionID]string{1: "slow"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer match.Close()
	buildHistory(t, match, historyFrames)
	if err := match.Attach(1, 1); err != nil {
		t.Fatal(err)
	}
	if err := match.StartCatchup(1, 1); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	reader := readLockstep(connection, start, 5*time.Millisecond)
	time.AfterFunc(200*time.Millisecond, func() { close(start) })

	var tickErrs []error
	closed := map[string]bool{}
	deadline := time.Now().Add(30 * time.Second)
	for match.CatchingUp(1) && time.Now().Before(deadline) && !tcp.sessionClosed("slow", closed) {
		if _, err := match.Tick(context.Background()); err != nil {
			tickErrs = append(tickErrs, err)
		}
		time.Sleep(time.Millisecond)
	}
	if tcp.sessionClosed("slow", closed) || len(tickErrs) > 0 {
		t.Fatalf("catch-up over a slow TCP link closed the connection (closed=%v) or failed (%d Tick errors, first: %v): pages were admitted faster than the link drains them",
			closed["slow"], len(tickErrs), errors.Join(tickErrs...))
	}
	if match.CatchingUp(1) {
		t.Fatalf("catch-up still running after 30s; the client has frames up to %d of %d", reader.next.Load()-1, match.History().Latest())
	}
	// 追上后转实时广播：客户端要拿到截至最新帧的连续帧流，连接一直在。
	head := uint64(match.History().Latest())
	for wait := time.Now().Add(5 * time.Second); reader.next.Load() <= head && reader.err() == nil && time.Now().Before(wait); {
		if _, err := match.Tick(context.Background()); err != nil {
			t.Fatalf("live Tick after the catch-up: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	if err := reader.err(); err != nil {
		t.Fatalf("client frame stream: %v", err)
	}
	if got := reader.next.Load(); got <= head {
		t.Fatalf("client has frames up to %d, want at least %d", got-1, head)
	}
	if tcp.sessionClosed("slow", closed) || tcp.runtime.ActiveSessions(1) != 1 {
		t.Fatal("the catching-up connection was closed")
	}
}

// 链路完全不读时，一页追帧在 CatchupSendTimeout 内写不出就计一次失败，连续 CatchupMaxFailures 次放弃这次追帧；
// 期间出站积压只有在途的那一页，连接不会因超过出站上限被关闭（write_timeout 远大于这里的时长）。
// 同一 Match 的另一条正常连接同时追帧，它的追帧照常完成、之后的实时帧照常到达，Tick 不被停读连接拖住。
func TestACatchUpPageTheLinkCannotTakeTimesOutWithoutHoldingOtherSessions(t *testing.T) {
	const (
		historyFrames = 600 // ≈ 4.8 MiB：只做准入时足以让停读连接超过出站字节上限
		sendTimeout   = 150 * time.Millisecond
		maxFailures   = 3
		sendWait      = 5 * time.Millisecond
	)
	tcp := newTCPLockstepServer(t, 30*time.Second, map[string]int64{"stalled": 1, "live": 2})
	_ = tcp.dial(t, "stalled", 1, 4<<10) // 从不读
	live := tcp.dial(t, "live", 2, 64<<10)
	sessions := map[nettransport.SessionID]string{1: "stalled", 2: "live"}
	match, err := lockstep.NewMatch(lockstep.MatchConfig{
		Sequencer:          lockstep.SequencerConfig{Players: tcpLockstepSeats, MaxInputBytes: lockstep.MaxInputPayloadBytes},
		RedundancyDepth:    1,
		CatchupSendTimeout: sendTimeout,
		CatchupMaxFailures: maxFailures,
		CatchupSendWait:    sendWait,
		Datagrams:          tcpLockstepSender(t, tcp.runtime, sessions),
		Reliable:           tcpLockstepSender(t, tcp.runtime, sessions),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer match.Close()
	buildHistory(t, match, historyFrames)
	for seat := lockstep.PlayerID(1); seat <= 2; seat++ {
		if err := match.Attach(seat, nettransport.SessionID(seat)); err != nil {
			t.Fatal(err)
		}
		if err := match.StartCatchup(seat, 1); err != nil {
			t.Fatal(err)
		}
	}
	started := make(chan struct{})
	close(started)
	reader := readLockstep(live, started, 0)

	var abandoned error
	var otherErrs []error
	var slowestTick time.Duration
	closed := map[string]bool{}
	began := time.Now()
	for abandoned == nil && time.Since(began) < 10*time.Second {
		tickStart := time.Now()
		_, err := match.Tick(context.Background())
		slowestTick = max(slowestTick, time.Since(tickStart))
		if err != nil {
			if strings.Contains(err.Error(), "abandoned after") {
				abandoned = err
			} else if !errors.Is(err, context.DeadlineExceeded) {
				otherErrs = append(otherErrs, err)
			}
		}
		time.Sleep(time.Millisecond)
	}
	stalledClosed := tcp.sessionClosed("stalled", closed)
	if abandoned == nil || !errors.Is(abandoned, context.DeadlineExceeded) || stalledClosed || len(otherErrs) > 0 {
		t.Fatalf("stalled catch-up: abandoned=%v, stalled connection closed=%v, other Tick errors=%v; want the page send to time out after %v %d times in a row while the connection stays open",
			abandoned, stalledClosed, errors.Join(otherErrs...), sendTimeout, maxFailures)
	}
	if match.CatchingUp(2) {
		t.Fatal("the reading session's catch-up did not finish while the stalled one was timing out")
	}
	head := uint64(match.History().Latest())
	for wait := time.Now().Add(5 * time.Second); reader.next.Load() <= head && reader.err() == nil && time.Now().Before(wait); {
		time.Sleep(time.Millisecond)
	}
	if err := reader.err(); err != nil || reader.next.Load() <= head {
		t.Fatalf("reading session has frames up to %d of %d (err %v)", reader.next.Load()-1, head, err)
	}
	// Tick 只等本次发起的页最多 CatchupSendWait；留出 -race 与调度余量，但远小于一次发送超时。
	if slowestTick > 100*time.Millisecond {
		t.Fatalf("slowest Tick took %v: the stalled session's page send held the match (CatchupSendWait %v)", slowestTick, sendWait)
	}
}

// 实时广播绝不等写出：一条停读连接的实时帧只进它自己的出站队列，满了就关闭它；同一 Tick 里其他接收者的帧照常到达，
// Tick 本身不随停读连接的 write_timeout 变慢。（追帧页可以等写出，实时帧不可以：两条通道在 TCPSender 里分开。）
func TestALiveBroadcastOverEmbeddedTCPNeverWaitsForAStalledConnection(t *testing.T) {
	const maxTicks = 8192 // 最多约 64 MiB；按实际关闭结束，不假定 4.8 MiB 一定装不进内核 + 应用缓冲。
	tcp := newTCPLockstepServer(t, 30*time.Second, map[string]int64{"stalled": 1, "live": 2})
	_ = tcp.dial(t, "stalled", 1, 4<<10)
	live := tcp.dial(t, "live", 2, 64<<10)
	sessions := map[nettransport.SessionID]string{1: "stalled", 2: "live"}
	match, err := lockstep.NewMatch(lockstep.MatchConfig{
		Sequencer:       lockstep.SequencerConfig{Players: tcpLockstepSeats, MaxInputBytes: lockstep.MaxInputPayloadBytes},
		RedundancyDepth: 1,
		Datagrams:       tcpLockstepSender(t, tcp.runtime, sessions),
		Reliable:        tcpLockstepSender(t, tcp.runtime, sessions),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer match.Close()
	for seat := lockstep.PlayerID(1); seat <= 2; seat++ {
		if err := match.Attach(seat, nettransport.SessionID(seat)); err != nil {
			t.Fatal(err)
		}
	}
	started := make(chan struct{})
	close(started)
	reader := readLockstep(live, started, 0)
	input := bytes.Repeat([]byte{0x5a}, lockstep.MaxInputPayloadBytes)
	var slowestTick time.Duration
	closed := map[string]bool{}
	ticks := 0
	for ticks < maxTicks {
		for _, seat := range tcpLockstepSeats {
			if _, err := match.SubmitInput(seat, match.NextFrame(), input); err != nil {
				t.Fatal(err)
			}
		}
		tickStart := time.Now()
		_, _ = match.Tick(context.Background()) // 停读连接被关闭后对它的发送失败，属预期
		slowestTick = max(slowestTick, time.Since(tickStart))
		ticks++
		// 每批只准入约 256 KiB，让健康连接的客户端确实保持读取；等待在 Tick 测量之外。
		if ticks%32 == 0 {
			deadline := time.Now().Add(5 * time.Second)
			for reader.next.Load() <= uint64(ticks) && reader.err() == nil && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if err := reader.err(); err != nil || reader.next.Load() <= uint64(ticks) {
				t.Fatalf("live reader stopped at %d of %d: %v", reader.next.Load()-1, ticks, err)
			}
			if tcp.sessionClosed("stalled", closed) {
				break
			}
		}
	}
	if slowestTick > 100*time.Millisecond {
		t.Fatalf("slowest live Tick took %v: a broadcast waited on the stalled connection", slowestTick)
	}
	for wait := time.Now().Add(5 * time.Second); reader.next.Load() <= uint64(ticks) && reader.err() == nil && time.Now().Before(wait); {
		time.Sleep(time.Millisecond)
	}
	if err := reader.err(); err != nil || reader.next.Load() <= uint64(ticks) {
		t.Fatalf("live session has frames up to %d of %d (err %v)", reader.next.Load()-1, ticks, err)
	}
	for wait := time.Now().Add(2 * time.Second); !tcp.sessionClosed("stalled", closed) && time.Now().Before(wait); {
		time.Sleep(time.Millisecond)
	}
	if !closed["stalled"] || closed["live"] {
		t.Fatalf("closed sessions %v, want only the stalled one (its outbound queue overflowed)", closed)
	}
}

// 嵌入 TCP 的全进程出站驻留预算耗尽时，实时帧按 Lockstep 已有的发送失败语义处理：这一帧对被拒的接收者算丢失
// （Tick 返回带 gateway.ErrResidentBudgetExhausted 的错误），不等预算、不关闭任何连接；预算归还后实时帧继续，
// 丢掉的帧由下一个广播包携带的冗余帧补上，客户端的 FrameAssembler 连续收齐，不需要追帧。
// 预算由一条停读连接占满：推到它的帧卡在 socket 与出站队列里。
func TestALiveBroadcastOverEmbeddedTCPDropsTheFrameWhenTheResidentBudgetIsExhausted(t *testing.T) {
	tcp := newTCPLockstepServerWith(t, 30*time.Second, map[string]int64{"filler": 1, "live": 2}, func(config *gateway.TCPConfig) {
		config.MaxPayloadBytes = 64 << 10
		config.OutboundResidentBytes = 256 << 10 // 低于每连接 2 MiB 上限：占预算的连接不会先因自己的上限被关
	})
	filler := tcp.dial(t, "filler", 1, 4<<10)
	live := tcp.dial(t, "live", 2, 64<<10)
	sessions := map[nettransport.SessionID]string{2: "live"}
	match, err := lockstep.NewMatch(lockstep.MatchConfig{
		Sequencer:       lockstep.SequencerConfig{Players: tcpLockstepSeats, MaxInputBytes: lockstep.MaxInputPayloadBytes},
		RedundancyDepth: 2,
		Datagrams:       tcpLockstepSender(t, tcp.runtime, sessions),
		Reliable:        tcpLockstepSender(t, tcp.runtime, sessions),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer match.Close()
	if err := match.Attach(2, 2); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	close(started)
	reader := readLockstep(live, started, 0)
	input := bytes.Repeat([]byte{0x5a}, lockstep.MaxInputPayloadBytes)
	tick := func() error {
		for _, seat := range tcpLockstepSeats {
			if _, err := match.SubmitInput(seat, match.NextFrame(), input); err != nil {
				t.Fatal(err)
			}
		}
		_, err := match.Tick(context.Background())
		return err
	}
	waitFrames := func(want uint64) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); reader.next.Load() <= want && reader.err() == nil; {
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if err := reader.err(); err != nil || reader.next.Load() <= want {
			t.Fatalf("live client has frames up to %d, want %d (err %v)", reader.next.Load()-1, want, err)
		}
	}
	if err := tick(); err != nil {
		t.Fatal(err)
	}
	waitFrames(1)

	// 占满预算：越来越小的帧（32 KiB → 1 KiB）把停读连接的 socket 缓冲与出站队列推满；停一会儿再探，
	// 直到停下来之后连 1 KiB 也放不下。一个广播包约 16 KiB，必然被拒。
	fill := func() {
		for size := 32 << 10; size >= 1<<10; size /= 2 {
			for attempt := 0; ; attempt++ {
				err := tcp.runtime.PushLockstepSession(context.Background(), "filler", tcpLockstepMessageID, make([]byte, size))
				if err == nil {
					if attempt > 4096 {
						t.Fatal("the filler connection never filled the process budget")
					}
					continue
				}
				if !errors.Is(err, gateway.ErrResidentBudgetExhausted) {
					t.Fatalf("filler push refused with %v, want gateway.ErrResidentBudgetExhausted", err)
				}
				break
			}
		}
	}
	for settled := false; !settled; {
		fill()
		time.Sleep(20 * time.Millisecond)
		settled = tcp.runtime.PushLockstepSession(context.Background(), "filler", tcpLockstepMessageID, make([]byte, 1<<10)) != nil
	}

	tickStart := time.Now()
	err = tick() // 第 2 帧：live 被预算拒收
	if elapsed := time.Since(tickStart); elapsed > 100*time.Millisecond {
		t.Fatalf("a Tick with the budget exhausted took %v: it waited for the budget", elapsed)
	}
	if !errors.Is(err, gateway.ErrResidentBudgetExhausted) {
		t.Fatalf("Tick with the budget exhausted = %v, want an error carrying gateway.ErrResidentBudgetExhausted", err)
	}
	closed := map[string]bool{}
	if tcp.sessionClosed("live", closed) || tcp.sessionClosed("filler", closed) || tcp.runtime.ActiveSessions(2) != 1 {
		t.Fatalf("closed sessions %v: no connection is closed for an exhausted process budget", closed)
	}

	// 占预算的连接关闭后预算归还：第 3 帧的广播带着冗余的第 2 帧，客户端连续收齐。
	_ = filler.Close()
	for deadline := time.Now().Add(5 * time.Second); !tcp.sessionClosed("filler", closed); {
		if time.Now().After(deadline) {
			t.Fatal("the filler connection was never closed after its client went away")
		}
		time.Sleep(time.Millisecond)
	}
	if err := tick(); err != nil {
		t.Fatalf("Tick after the budget was returned = %v", err)
	}
	waitFrames(3)
	if closed["live"] {
		t.Fatal("the live connection was closed")
	}
}

// snapshotReader 在客户端连接上读 Lockstep 推送：CA 分块交给 SnapshotReceiver，快照到齐后从快照帧续接 C7 帧。
type snapshotReader struct {
	next     atomic.Uint64
	snapshot atomic.Pointer[lockstep.ReceivedSnapshot]
	failed   atomic.Pointer[error]
	done     chan struct{}
}

func readSnapshotRecovery(connection net.Conn, start <-chan struct{}, pause time.Duration) *snapshotReader {
	reader := &snapshotReader{done: make(chan struct{})}
	reader.next.Store(1)
	fail := func(err error) { reader.failed.Store(&err) }
	go func() {
		defer close(reader.done)
		<-start
		receiver := lockstep.NewSnapshotReceiver(0)
		next := lockstep.FrameID(1)
		for {
			packet, err := wire.Read(connection, 0)
			if err != nil {
				return
			}
			if packet.Flags != wire.FlagPush|wire.FlagLockstep || packet.MsgID != tcpLockstepMessageID {
				continue
			}
			switch {
			case lockstep.IsSnapshotChunk(packet.Payload):
				snapshot, err := receiver.Accept(packet.Payload)
				if err != nil {
					fail(err)
					return
				}
				if snapshot != nil {
					reader.snapshot.Store(snapshot)
					next = snapshot.Frame + 1
				}
			case lockstep.IsCatchupNotice(packet.Payload):
				_, err := lockstep.DecodeCatchupUnservable(packet.Payload)
				fail(errors.Join(errors.New("recovery failed"), err))
				return
			default:
				frames, err := lockstep.DecodeBroadcast(packet.Payload)
				if err != nil {
					fail(err)
					return
				}
				if receiver.Active() {
					continue
				}
				for _, frame := range frames {
					if frame.ID < next {
						continue
					}
					if frame.ID > next {
						fail(errors.New("frame gap after the snapshot"))
						return
					}
					next++
				}
			}
			reader.next.Store(uint64(next))
			if pause > 0 {
				time.Sleep(pause)
			}
		}
	}()
	return reader
}

func (reader *snapshotReader) err() error {
	if failed := reader.failed.Load(); failed != nil {
		return *failed
	}
	return nil
}

// 4 MiB 快照经 TCPSender 发给先停读、再慢读的客户端：分块随链路放慢（等写出），连接出站积压不超过一批，
// 连接不断（4 MiB 远超每连接约 2 MiB 的出站字节上限，只准入地灌会被关闭）；快照后续帧无洞并回到实时。
// 同一 Match 的另一条连接一直收实时帧，不受慢连接影响。
//
// 对照：可靠通道换成只准入（push 不等写出）时，同一场景在停读期间就把出站灌满、连接被关闭或拒收——
// 说明这条用例落在“随链路放慢”这个承诺上，而不是规模恰好小到能过。
func TestASnapshotOverEmbeddedTCPFollowsTheLinkAndKeepsOtherSessionsLive(t *testing.T) {
	t.Run("admission-only control overflows", func(t *testing.T) {
		if overflowed := snapshotOverEmbeddedTCP(t, false); !overflowed {
			t.Fatal("admission-only snapshot chunks did not overflow the stalled connection: the scenario is too small to tell")
		}
	})
	t.Run("written lane follows the link", func(t *testing.T) { snapshotOverEmbeddedTCP(t, true) })
}

// snapshotOverEmbeddedTCP 跑一次场景。written=false 时可靠通道只准入，只返回慢连接是否溢出（被关闭或拒收），不做其他断言。
func snapshotOverEmbeddedTCP(t *testing.T, written bool) bool {
	const snapshotBytes = 4 << 20
	tcp := newTCPLockstepServer(t, 10*time.Second, map[string]int64{"slow": 1, "fast": 2})
	slow := tcp.dial(t, "slow", 1, 16<<10)
	fast := tcp.dial(t, "fast", 2, 1<<20)
	sessions := map[nettransport.SessionID]string{1: "slow", 2: "fast"}
	data := bytes.Repeat([]byte("roost snapshot "), snapshotBytes/15+1)[:snapshotBytes]
	var calls atomic.Int64
	reliable := tcpLockstepSender(t, tcp.runtime, sessions)
	if !written {
		admission, err := lockstep.NewTCPSender(tcpLockstepMessageID, 0, func(id nettransport.SessionID) (string, bool) {
			session, ok := sessions[id]
			return session, ok
		}, tcp.runtime.PushLockstepSession, tcp.runtime.PushLockstepSession)
		if err != nil {
			t.Fatal(err)
		}
		reliable = admission
	}
	match, err := lockstep.NewMatch(lockstep.MatchConfig{
		Sequencer:           lockstep.SequencerConfig{Players: []lockstep.PlayerID{1, 2}, MaxInputBytes: 8},
		RedundancyDepth:     3,
		HistoryRetainFrames: lockstep.MinHistoryRetainFrames,
		CatchupSendTimeout:  10 * time.Second,
		CatchupSendWait:     5 * time.Millisecond,
		Datagrams:           tcpLockstepSender(t, tcp.runtime, sessions),
		Reliable:            reliable,
		// 窗口只有 64 帧而慢读要几百个 Tick：钉住上限放宽，这里只看分块是否随链路推进（钉住上限另有用例）。
		Snapshots: lockstep.SnapshotConfig{PinFrames: 1 << 20, Provider: lockstep.SnapshotProviderFunc(func(_ context.Context, req lockstep.SnapshotRequest) (lockstep.Snapshot, error) {
			calls.Add(1)
			return lockstep.Snapshot{Frame: req.MaxFrame, Data: data, StateHash: 99, HasStateHash: true}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer match.Close()
	if err := match.Attach(2, 2); err != nil {
		t.Fatal(err)
	}
	reading := make(chan struct{})
	close(reading)
	fastReader := readLockstep(fast, reading, 0)
	tick := func() error {
		frame := match.NextFrame()
		for _, seat := range []lockstep.PlayerID{1, 2} {
			if _, err := match.SubmitInput(seat, frame, []byte{byte(frame)}); err != nil {
				t.Fatal(err)
			}
		}
		_, err := match.Tick(context.Background())
		return err
	}
	for range 3 * lockstep.MinHistoryRetainFrames { // 窗口已滚动：帧 1 不在历史里，只能走快照
		if err := tick(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Microsecond)
	}
	if err := match.Attach(1, 1); err != nil {
		t.Fatal(err)
	}
	if err := match.StartCatchup(1, 1); err != nil {
		t.Fatalf("out-of-window request: %v", err)
	}
	start := make(chan struct{})
	reader := readSnapshotRecovery(slow, start, time.Millisecond)
	time.AfterFunc(200*time.Millisecond, func() { close(start) })

	var tickErrs []error
	closed := map[string]bool{}
	began := time.Now()
	deadline := began.Add(60 * time.Second)
	for match.CatchingUp(1) && time.Now().Before(deadline) && !tcp.sessionClosed("slow", closed) {
		if err := tick(); err != nil {
			tickErrs = append(tickErrs, err)
		}
		time.Sleep(time.Millisecond)
	}
	if !written {
		return tcp.sessionClosed("slow", closed) || len(tickErrs) > 0
	}
	if tcp.sessionClosed("slow", closed) || len(tickErrs) > 0 {
		t.Fatalf("snapshot over a slow TCP link closed the connection (closed=%v) or failed (%d Tick errors, first: %v)", closed["slow"], len(tickErrs), errors.Join(tickErrs...))
	}
	if match.CatchingUp(1) {
		t.Fatalf("snapshot recovery still running after 60s (client next %d, err %v)", reader.next.Load(), reader.err())
	}
	if elapsed := time.Since(began); elapsed < 200*time.Millisecond {
		t.Fatalf("4 MiB reached a stalled reader in %v: chunks did not wait for the link", elapsed)
	}
	head := uint64(match.History().Latest())
	for wait := time.Now().Add(5 * time.Second); (reader.next.Load() <= head || fastReader.next.Load() <= head) && reader.err() == nil && time.Now().Before(wait); {
		if err := tick(); err != nil {
			t.Fatalf("live Tick after the snapshot: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	if err := reader.err(); err != nil {
		t.Fatalf("recovering client: %v", err)
	}
	if err := fastReader.err(); err != nil {
		t.Fatalf("live client: %v", err)
	}
	snapshot := reader.snapshot.Load()
	if snapshot == nil || !bytes.Equal(snapshot.Data, data) || snapshot.StateHash != 99 || calls.Load() != 1 {
		t.Fatalf("snapshot %v, provider calls %d", snapshot != nil, calls.Load())
	}
	if reader.next.Load() <= head || fastReader.next.Load() <= head {
		t.Fatalf("clients at %d / %d, head %d", reader.next.Load()-1, fastReader.next.Load()-1, head)
	}
	if tcp.sessionClosed("slow", closed) || tcp.runtime.ActiveSessions(1) != 1 {
		t.Fatal("the recovering connection was closed")
	}
	return false
}
