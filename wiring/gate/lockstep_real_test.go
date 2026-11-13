//go:build integration

package gate_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/lockstep"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	gatewiring "github.com/tjbdwanghaibo/roost-core/wiring/gate"
)

// Match 的唯一 owner 独立于 NATS callback；Input/Attach/Detach/Tick 都进入该作用域。
// 此测试验证原始 LS 协议/生命周期；生成 PB→Nest→DAO/WAL 另由生成消费者覆盖。
func TestRealGateLockstepInputFramesCatchupAndOldDisconnect(t *testing.T) {
	type roomJob struct {
		run  func() error
		done chan error
	}
	jobs := make(chan roomJob, 32)
	ownerDone := make(chan struct{})
	go func() {
		defer close(ownerDone)
		for job := range jobs {
			job.done <- job.run()
		}
	}()
	t.Cleanup(func() { close(jobs); <-ownerDone })
	run := func(fn func() error) error { done := make(chan error, 1); jobs <- roomJob{fn, done}; return <-done }
	var room *lockstep.Match
	var h *gateHarness
	h = realGateHarness(t, func(ctx context.Context, s gateway.Session, id, seq uint32, kind wire.PayloadKind, payload []byte) (any, error) {
		if id != 301 || kind != wire.PayloadLockstep {
			return nil, gateway.ErrInvalidRequest
		}
		command, err := lockstep.DecodeCommand(payload)
		if err != nil {
			return nil, err
		}
		_, receiver, ok := h.game.SessionReceiver(s.Principal().SessionID)
		if !ok {
			return nil, gateway.ErrBindingStale
		}
		return nil, run(func() error {
			if err := room.HandleCommand(nettransport.SessionID(receiver), command); err != nil {
				return err
			}
			_, err := room.Tick(ctx)
			return err
		})
	}, func(deps *gateway.GameDependencies) {
		deps.OnActive = func(_ gateway.Binding, id uint64) error {
			return run(func() error { return room.Attach(123, nettransport.SessionID(id)) })
		}
		deps.OnClosed = func(_ gateway.Binding, id uint64) {
			if err := run(func() error { room.DetachSession(123, nettransport.SessionID(id)); return nil }); err != nil {
				t.Error(err)
			}
		}
	})
	transport := gatewiring.Transport{Game: h.game, LockstepMessageID: 302}
	var err error
	room, err = lockstep.NewMatch(lockstep.MatchConfig{Sequencer: lockstep.SequencerConfig{Players: []lockstep.PlayerID{123}, MaxInputBytes: 16}, RedundancyDepth: 1, Datagrams: transport, Reliable: transport, CatchupSendWait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	conn := h.connect(t)
	send := func(seq uint32, command lockstep.Command) {
		t.Helper()
		data, err := lockstep.EncodeCommand(command)
		if err != nil {
			t.Fatal(err)
		}
		if err := wire.Write(conn, []*wire.Packet{{Flags: wire.FlagLockstep, MsgID: 301, Seq: seq, Payload: data}}, h.config.MaxPayloadBytes); err != nil {
			t.Fatal(err)
		}
	}
	read := func() []lockstep.Frame {
		t.Helper()
		packet, err := wire.Read(conn, h.config.MaxPayloadBytes)
		if err != nil || packet.MsgID != 302 || packet.Flags != wire.FlagLockstep|wire.FlagPush {
			t.Fatalf("LS packet=%+v err=%v", packet, err)
		}
		frames, err := lockstep.DecodeBroadcast(packet.Payload)
		if err != nil {
			t.Fatal(err)
		}
		return frames
	}
	send(2, lockstep.Command{Operation: lockstep.OpInput, Frame: 1, Payload: []byte{7}})
	frames := read()
	if len(frames) != 1 || len(frames[0].Inputs) != 1 || frames[0].Inputs[0].Player != 123 || frames[0].Inputs[0].Payload[0] != 7 {
		t.Fatalf("input frame=%+v", frames)
	}
	old, oldReceiver, ok := h.game.SessionReceiver("session")
	if !ok {
		t.Fatal("old receiver absent")
	}
	// 相同认证 SessionID 的重连由 Game 原子替换，旧关闭可能迟到。
	next := h.connect(t)
	conn = next
	send(2, lockstep.Command{Operation: lockstep.OpCatchup, Frame: 1})
	frames = read()
	if len(frames) != 2 || frames[0].ID != 1 || frames[1].ID != 2 {
		t.Fatalf("catchup frames=%+v", frames)
	}
	if err := run(func() error {
		if room.DetachSession(123, nettransport.SessionID(oldReceiver)) {
			return errors.New("late disconnect detached replacement")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.game.PushBound(ctx, old, 303, wire.PayloadLockstep, []byte("stale")); !errors.Is(err, gateway.ErrBindingStale) {
		t.Fatalf("old outbound=%v", err)
	}
	send(3, lockstep.Command{Operation: lockstep.OpInput, Frame: 3, Payload: []byte{9}})
	frames = read()
	if len(frames) != 1 || frames[0].ID != 3 || len(frames[0].Inputs) != 1 || frames[0].Inputs[0].Payload[0] != 9 {
		t.Fatalf("replacement input=%+v", frames)
	}
}

// TestRealGateLockstepCatchupOverASlowLinkDrill 是独立 Gate 慢链路上的纯追帧页演练（KNOWN-LIMITS 能力表 Lockstep 一行
// “独立 Gate 慢链路上的纯追帧页”；嵌入 TCP 的同一演练见 2026-10-20 夜间补测第 10 项）。真实 Gate + Game Ingress（私有 NATS）：
// 8 席、每席每帧 50 B 输入、RedundancyDepth 1，先无连接地切 LS_DRILL_FRAMES 帧（缺省 3000，< 历史窗口），客户端经 toxiproxy
// （下行 bandwidth toxic，LS_DRILL_RATES 逗号分隔的 KB/s，0 = 不限速）连上 Gate，从第 1 帧追帧，之后 30Hz Tick。
// 报服务端追帧结束时间、客户端回到头部时间、与“积压 /（链路 − 每秒新帧）”之比、Tick 错误、连接是否被关、追帧计数。
// 独立 Gate 下 Game 侧的“发出”只到 Gate 准入（不是写出），慢链路上的积压落在 Gate 的每连接出站队列里，可能被 Gate 关闭
// （与大快照同一结构，维护者 2026-10-20 决定暂不做写出回执）；本演练测的就是纯追帧页是否也这样。
//
// 只在设置 LS_DRILL_RATES、ROOST_DATAENGINE_IT_TOXIPROXY_URL 与 ROOST_DATAENGINE_IT_NATS_URL 时运行（测量，不是回归）：
//
//	LS_DRILL_RATES=0,256,64 GOWORK=off go test -tags integration -count=1 -v -timeout 15m -run TestRealGateLockstepCatchupOverASlowLinkDrill ./wiring/gate
func TestRealGateLockstepCatchupOverASlowLinkDrill(t *testing.T) {
	api := os.Getenv("ROOST_DATAENGINE_IT_TOXIPROXY_URL")
	if api == "" || os.Getenv("LS_DRILL_RATES") == "" {
		t.Skip("set LS_DRILL_RATES and ROOST_DATAENGINE_IT_TOXIPROXY_URL to run the slow-link catch-up drill (a measurement)")
	}
	for _, field := range strings.Split(os.Getenv("LS_DRILL_RATES"), ",") {
		rate, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || rate < 0 {
			t.Fatalf("LS_DRILL_RATES entry %q is not a non-negative integer", field)
		}
		t.Run(fmt.Sprintf("rate%dKBps", rate), func(t *testing.T) { gateCatchupDrill(t, api, rate) })
	}
}

func gateCatchupDrill(t *testing.T, api string, rateKB int) {
	const tickPeriod = time.Second / 30
	const player lockstep.PlayerID = 123 // realGateHarness 的鉴权固定给出玩家 123
	seats := []lockstep.PlayerID{player, 2, 3, 4, 5, 6, 7, 8}
	frames := 3000
	if n, err := strconv.Atoi(os.Getenv("LS_DRILL_FRAMES")); err == nil && n > 0 {
		frames = n
	}
	input := bytes.Repeat([]byte{0x5a}, 50)

	// Match 只在一个 owner 协程里动（与上面的用例同一写法）：Gate 回调与 Tick 都经 run 进来。
	type roomJob struct {
		run  func() error
		done chan error
	}
	jobs := make(chan roomJob, 32)
	ownerDone := make(chan struct{})
	go func() {
		defer close(ownerDone)
		for job := range jobs {
			job.done <- job.run()
		}
	}()
	t.Cleanup(func() { close(jobs); <-ownerDone })
	run := func(fn func() error) error { done := make(chan error, 1); jobs <- roomJob{fn, done}; return <-done }

	var room *lockstep.Match
	var attached atomic.Uint64
	var closedSessions atomic.Int64
	h := realGateHarness(t, nil, func(deps *gateway.GameDependencies) {
		deps.OnActive = func(_ gateway.Binding, id uint64) error {
			attached.Store(id)
			return run(func() error { return room.Attach(player, nettransport.SessionID(id)) })
		}
		deps.OnClosed = func(_ gateway.Binding, id uint64) {
			closedSessions.Add(1)
			_ = run(func() error { room.DetachSession(player, nettransport.SessionID(id)); return nil })
		}
	})
	transport := gatewiring.Transport{Game: h.game, LockstepMessageID: 302}
	var err error
	room, err = lockstep.NewMatch(lockstep.MatchConfig{Sequencer: lockstep.SequencerConfig{Players: seats, MaxInputBytes: 64}, RedundancyDepth: 1, Datagrams: transport, Reliable: transport})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run(func() error { room.Close(); return nil }) })
	submit := func() error {
		for _, seat := range seats {
			if _, err := room.SubmitInput(seat, room.NextFrame(), input); err != nil {
				return err
			}
		}
		return nil
	}
	if err := run(func() error {
		for range frames {
			if err := submit(); err != nil {
				return err
			}
			if _, err := room.Tick(context.Background()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	listen := gateDrillProxy(t, api, fmt.Sprintf("lsgate-catchup-%d-%d", os.Getpid(), rateKB), h.server.Addr().String(), rateKB)
	conn, err := net.DialTimeout("tcp", listen, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := wire.Write(conn, []*wire.Packet{{MsgID: 0, Seq: 1, Payload: []byte("ticket")}}, 8192); err != nil {
		t.Fatal(err)
	}
	if ack, err := wire.Read(conn, 8192); err != nil || ack.MsgID != 0 || ack.Seq != 1 {
		t.Fatalf("auth through proxy: %+v %v", ack, err)
	}
	_ = conn.SetDeadline(time.Time{})
	for deadline := time.Now().Add(3 * time.Second); attached.Load() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the Gate binding never activated")
		}
	}

	var next atomic.Uint64
	var wireBytes atomic.Int64
	var readErr atomic.Pointer[error]
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		assembler := lockstep.NewFrameAssembler(0)
		for {
			packet, err := wire.Read(conn, h.config.MaxPayloadBytes)
			if err != nil {
				readErr.Store(&err)
				return
			}
			if packet.MsgID != 302 {
				continue
			}
			wireBytes.Add(int64(len(packet.Payload)))
			if _, err := assembler.Ingest(packet.Payload); err != nil {
				readErr.Store(&err)
				return
			}
			next.Store(uint64(assembler.Next()))
		}
	}()

	var backlog lockstep.FrameID
	before := metrics.Snapshot()
	start := time.Now()
	if err := run(func() error {
		backlog = room.History().Latest()
		return room.StartCatchup(player, 1)
	}); err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(tickPeriod)
	defer ticker.Stop()
	var serverDone, headAt time.Duration
	var lastFrame lockstep.FrameID
	var tickErrors int
	var lastTickErr error
	var bytesAtServerDone int64
	tick := func() {
		var catching bool
		_ = run(func() error {
			if err := submit(); err != nil {
				return err
			}
			frame, err := room.Tick(context.Background())
			lastFrame = frame.ID
			if err != nil {
				tickErrors++
				lastTickErr = err
			}
			catching = room.CatchingUp(player)
			return nil
		})
		if serverDone == 0 && !catching {
			serverDone = time.Since(start)
			bytesAtServerDone = wireBytes.Load()
		}
	}
	for closedSessions.Load() == 0 && readErr.Load() == nil && time.Since(start) < 240*time.Second {
		<-ticker.C
		tick()
		if serverDone != 0 && next.Load() >= uint64(lastFrame) {
			headAt = time.Since(start)
			break
		}
	}
	// 回到头部后再保持实时 5 秒，确认连接不断、帧流连续。
	for liveUntil := time.Now().Add(5 * time.Second); closedSessions.Load() == 0 && time.Now().Before(liveUntil); {
		<-ticker.C
		tick()
	}
	time.Sleep(500 * time.Millisecond)
	closed := closedSessions.Load() != 0
	stillLive := !closed && readErr.Load() == nil && next.Load() >= uint64(lastFrame)
	totalBytes := wireBytes.Load()
	_ = conn.Close()
	<-readerDone
	after := metrics.Snapshot()

	frameBytes := 0.0
	if lastFrame > 0 {
		frameBytes = float64(totalBytes) / float64(lastFrame)
	}
	var expect time.Duration
	if rateKB > 0 {
		if netRate := float64(rateKB*1000) - frameBytes*30; netRate > 0 {
			expect = time.Duration(frameBytes * float64(backlog) / netRate * float64(time.Second))
		}
	}
	ratio := 0.0
	if expect > 0 && headAt > 0 {
		ratio = float64(headAt) / float64(expect)
	}
	t.Logf("rate %d KB/s: backlog %d frames, client read %d B over %d frames (%.0f B/frame); server catch-up done %v (client had %d B); client at head %v; expected backlog/(rate-live) %v; ratio %.2f; Tick errors %d (last %v); closed by Gate %v; live after head %v; client next %d head %d",
		rateKB, backlog, totalBytes, lastFrame, frameBytes, serverDone.Round(time.Millisecond), bytesAtServerDone, headAt.Round(time.Millisecond), expect.Round(time.Millisecond), ratio, tickErrors, lastTickErr, closed, stillLive, next.Load(), lastFrame)
	for _, name := range []string{"lockstep.catchup.frames.total", "lockstep.catchup.pages.total", "lockstep.catchup.failed.total", "lockstep.catchup.unservable.total", "player_tcp_write_deadline_total", "player_tcp_outbound_overflow_total"} {
		t.Logf("metric %s: %v -> %v", name, drillMetric(before, name), drillMetric(after, name))
	}
	if closed || headAt == 0 || !stillLive {
		t.Errorf("catch-up over the Gate did not return to live: closed %v, at head after %v, live afterwards %v", closed, headAt, stillLive)
	}
}

// gateDrillProxy 在 toxiproxy 上建一个到 upstream 的代理（用例结束删除），rateKB > 0 时给下行加 bandwidth toxic。
func gateDrillProxy(t *testing.T, api, name, upstream string, rateKB int) string {
	t.Helper()
	call := func(method, path string, body any) []byte {
		raw, _ := json.Marshal(body)
		request, err := http.NewRequest(method, api+path, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		out, _ := io.ReadAll(response.Body)
		if response.StatusCode/100 != 2 {
			t.Fatalf("toxiproxy %s %s: %d %s", method, path, response.StatusCode, out)
		}
		return out
	}
	created := call(http.MethodPost, "/proxies", map[string]any{"name": name, "listen": "127.0.0.1:0", "upstream": upstream, "enabled": true})
	t.Cleanup(func() { call(http.MethodDelete, "/proxies/"+name, nil) })
	var proxy struct{ Listen string }
	if err := json.Unmarshal(created, &proxy); err != nil {
		t.Fatal(err)
	}
	if rateKB > 0 {
		call(http.MethodPost, "/proxies/"+name+"/toxics", map[string]any{"name": "bw", "type": "bandwidth", "stream": "downstream", "toxicity": 1, "attributes": map[string]int{"rate": rateKB}})
	}
	return proxy.Listen
}

func drillMetric(snapshot []metrics.Metric, name string) []string {
	var out []string
	for _, metric := range snapshot {
		if metric.Name == name {
			out = append(out, fmt.Sprintf("%v=%d", metric.Labels, metric.Value))
		}
	}
	return out
}
