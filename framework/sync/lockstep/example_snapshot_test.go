package lockstep_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/lockstep"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
)

// 本文件是 Lockstep 快照恢复的接线样例（设计见 docs/maintenance/FEATURE-2026-10-20-lockstep-snapshot-recovery.md §8）：
//
//  1. 房间 owner（唯一驱动 Match 的 goroutine）每切一帧，先让服务器参考模拟应用这一帧，
//     每 checkpointEvery 帧把参考状态编码成一份不可变检查点，经原子指针发布；
//  2. SnapshotProvider 在 Match 自己起的 goroutine 上被调用，只读已发布的检查点，不碰 owner 独占的模拟状态；
//  3. 断线超过历史窗口的客户端照常发 OpCatchup：配置了 Provider 时 Recovery 零值即 auto，
//     服务端判定起点已被裁掉，改发快照分块，再从快照帧 +1 续发历史页，追到头后回到实时广播；
//  4. 客户端用 FrameAssembler.Receive 收帧和快照：快照到齐后业务恢复状态、核对状态 hash，
//     再调用 ResumeAfterSnapshot，之后的帧照常应用。
//
// 检查点缓存是业务代码，框架本期不导出这个类型；模拟、编码与 hash 也都是业务的。

const checkpointEvery = 16

// world 是一个确定性小模拟：每席一个整数位置，每帧加上该席输入（int8 位移）。
type world struct {
	frame     lockstep.FrameID
	positions [2]int64
}

func (w *world) apply(frame lockstep.Frame) {
	w.frame = frame.ID
	for _, input := range frame.Inputs {
		w.positions[input.Player-1] += int64(int8(input.Payload[0]))
	}
}

func (w *world) encode() []byte {
	out := binary.LittleEndian.AppendUint32(nil, uint32(w.frame))
	for _, position := range w.positions {
		out = binary.LittleEndian.AppendUint64(out, uint64(position))
	}
	return out
}

func decodeWorld(data []byte) (world, error) {
	if len(data) != 4+8*2 {
		return world{}, errors.New("bad world snapshot")
	}
	w := world{frame: lockstep.FrameID(binary.LittleEndian.Uint32(data))}
	for i := range w.positions {
		w.positions[i] = int64(binary.LittleEndian.Uint64(data[4+8*i:]))
	}
	return w, nil
}

// hash 是状态 hash：快照携带它，客户端恢复后用同一算法核对（与关键帧 ReportHash 同一算法）。
func (w *world) hash() uint64 {
	sum := fnv.New64a()
	_, _ = sum.Write(w.encode())
	return sum.Sum64()
}

// checkpointCache 是检查点缓存：owner 写、Provider 读，原子指针，无锁。
// 发布的 Snapshot 交出后不再修改（Data 由多个恢复中的会话共享）。
type checkpointCache struct {
	latest atomic.Pointer[lockstep.Snapshot]
}

func (c *checkpointCache) Publish(snapshot lockstep.Snapshot) { c.latest.Store(&snapshot) }

// Snapshot 实现 lockstep.SnapshotProvider：只交出落在请求范围内的检查点。
// 最新检查点早于 MinFrame（检查点间隔大于历史窗口）时返回 ErrSnapshotUnavailable，Match 计失败、下一 Tick 再取。
func (c *checkpointCache) Snapshot(_ context.Context, req lockstep.SnapshotRequest) (lockstep.Snapshot, error) {
	snapshot := c.latest.Load()
	if snapshot == nil || snapshot.Frame < req.MinFrame || snapshot.Frame > req.MaxFrame {
		return lockstep.Snapshot{}, lockstep.ErrSnapshotUnavailable
	}
	return *snapshot, nil
}

// loopback 把两条下行通道按发送顺序放进每个会话的收件箱（真实部署是 KCP / 嵌入 TCP / 独立 Gate）。
// 并发安全：快照分块与追帧页由 Match 的后台 goroutine 发送。
type loopback struct {
	mu    sync.Mutex
	inbox map[nettransport.SessionID][][]byte
}

func (l *loopback) SendDatagram(_ context.Context, session nettransport.SessionID, packet []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inbox[session] = append(l.inbox[session], append([]byte(nil), packet...))
	return nil
}

func (l *loopback) SendReliable(ctx context.Context, session nettransport.SessionID, packet []byte) error {
	return l.SendDatagram(ctx, session, packet)
}

func (l *loopback) take(session nettransport.SessionID) [][]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	packets := l.inbox[session]
	delete(l.inbox, session)
	return packets
}

// client 是客户端：FrameAssembler 收帧与快照，world 是本地模拟。
type client struct {
	assembler *lockstep.FrameAssembler
	state     world
	restored  []lockstep.FrameID
}

func (c *client) receive(packets [][]byte) error {
	for _, packet := range packets {
		delivery, err := c.assembler.Receive(packet)
		if err != nil {
			return err // 包括 *CatchupUnservableError：恢复失败，客户端终止本场
		}
		if snapshot := delivery.Snapshot; snapshot != nil {
			restored, err := decodeWorld(snapshot.Data)
			if err != nil {
				return err
			}
			if got := restored.hash(); snapshot.HasStateHash && got != snapshot.StateHash {
				return &lockstep.SnapshotMismatchError{Frame: snapshot.Frame, Want: snapshot.StateHash, Got: got}
			}
			c.state = restored
			c.restored = append(c.restored, snapshot.Frame)
			c.assembler.ResumeAfterSnapshot(snapshot.Frame)
		}
		for _, frame := range delivery.Frames {
			c.state.apply(frame)
		}
	}
	return nil
}

// Example_snapshotRecovery 演示：席位 2 断线超过历史窗口（64 帧）后重连，经服务器参考模拟的检查点恢复，
// 续帧到头部后与参考模拟、一直在线的席位 1 状态一致。
func Example_snapshotRecovery() {
	lanes := &loopback{inbox: make(map[nettransport.SessionID][][]byte)}
	checkpoints := &checkpointCache{}
	match, err := lockstep.NewMatch(lockstep.MatchConfig{
		Sequencer:           lockstep.SequencerConfig{Players: []lockstep.PlayerID{1, 2}, MaxInputBytes: 1},
		HistoryRetainFrames: lockstep.MinHistoryRetainFrames,
		Datagrams:           lanes,
		Reliable:            lanes,
		// Recovery 留零值：配置了 Provider 时即 RecoveryAuto（窗口内追帧，越界发快照）。
		Snapshots: lockstep.SnapshotConfig{Provider: checkpoints},
	})
	if err != nil {
		panic(err)
	}
	defer match.Close()

	reference := &world{}
	a := &client{assembler: lockstep.NewFrameAssembler(0)}
	b := &client{assembler: lockstep.NewFrameAssembler(0)}
	bOnline, bSession := true, nettransport.SessionID(102)
	// tick 是房间 owner 的一步：收两席输入、切帧、推进参考模拟、按间隔发布检查点。
	tick := func() lockstep.FrameID {
		next := match.NextFrame()
		if _, err := match.SubmitInput(1, next, []byte{byte(int8(next%5) - 2)}); err != nil {
			panic(err)
		}
		if bOnline {
			if _, err := match.SubmitInput(2, next, []byte{byte(int8(next%3) - 1)}); err != nil {
				panic(err)
			}
		}
		frame, err := match.Tick(context.Background()) // err 只说明部分发送失败，帧已切出
		if err != nil {
			panic(err)
		}
		reference.apply(frame)
		if frame.ID%checkpointEvery == 0 {
			checkpoints.Publish(lockstep.Snapshot{Frame: frame.ID, Data: reference.encode(), StateHash: reference.hash(), HasStateHash: true})
		}
		if err := a.receive(lanes.take(101)); err != nil {
			panic(err)
		}
		if err := b.receive(lanes.take(bSession)); err != nil {
			panic(err)
		}
		return frame.ID
	}
	if err := match.Attach(1, 101); err != nil {
		panic(err)
	}
	if err := match.Attach(2, 102); err != nil {
		panic(err)
	}
	for match.NextFrame() <= 20 {
		tick()
	}
	// 席位 2 断线：之后的帧它收不到，也不再提交输入。
	match.Detach(2)
	bOnline = false
	for match.NextFrame() <= 200 {
		tick()
	}
	// 换一个会话重连，照常请求从缺的第一帧追帧；那一帧早已被裁出窗口。
	bSession = 202
	if err := match.Attach(2, bSession); err != nil {
		panic(err)
	}
	bOnline = true
	from := b.assembler.Next()
	if err := match.HandleCommand(bSession, lockstep.Command{Operation: lockstep.OpCatchup, Frame: from}); err != nil {
		panic(err) // catchup 模式（没有 Provider）在这里返回 *CatchupUnservableError
	}
	fmt.Printf("B asks from frame %d; history starts at %d\n", from, match.History().FirstID())
	// 取快照在 Match 的 goroutine 上进行，Tick 只非阻塞地收结果；按 1ms 的 Tick 周期推进到 B 回到实时。
	head := lockstep.FrameID(0)
	for ticks := 0; match.CatchingUp(2) || b.assembler.Next() <= head || len(b.restored) == 0; ticks++ {
		if ticks > 5000 {
			panic("B did not recover")
		}
		time.Sleep(time.Millisecond)
		head = tick()
	}
	fmt.Println("B restored from one checkpoint:", len(b.restored) == 1 && b.restored[0]%checkpointEvery == 0)
	fmt.Println("B matches the reference at the head:", b.state == *reference)
	fmt.Println("A matches the reference at the head:", a.state == *reference)
	// Output:
	// B asks from frame 21; history starts at 137
	// B restored from one checkpoint: true
	// B matches the reference at the head: true
	// A matches the reference at the head: true
}
