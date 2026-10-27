package jslogproto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

func ids(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s-%d", prefix, i)
	}
	return out
}

func balanceOf(p *Participant) func(int64) (int64, error) {
	return func(n int64) (int64, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return p.Balance(ctx, n)
	}
}

// 正向与补偿：两步转账 60 笔正向、40 笔第二步被业务拒绝；四个分区、一个协调器、strict WAL 参与方。
func TestEndToEndForwardAndCompensate(t *testing.T) {
	e := newEnv(t, 4)
	part := e.participant(nil)
	coord := e.coordinator("A", nil)
	e.takeAll(coord, false)
	all := ids("fc", 100)
	data := map[string]TransferData{}
	for i, id := range all {
		data[id] = e.transfer(i, i >= 60)
	}
	e.startAll(all, data, 60*time.Second)
	final := e.client.Latest()
	for i, id := range all {
		want := StatusCompleted
		if i >= 60 {
			want = StatusCompensated
		}
		if final[id].Status != want {
			t.Fatalf("%s: %s, want %s", id, final[id], want)
		}
	}
	e.checkBalances(balanceOf(part), final, all)
	if v := e.chainViolations(); len(v) > 0 {
		t.Fatalf("event chain violations:\n%s", strings.Join(v, "\n"))
	}
	t.Logf("coordinator: ev=%d pubs=%d batches=%d accepted=%d; participant executed=%d refused=%d results=%d WAL syncs=%d",
		coord.Stats.EvMsgs.Load(), coord.Stats.PubCalls.Load(), coord.Stats.BatchCalls.Load(), coord.Stats.Accepted.Load(),
		part.Stats.Executed.Load(), part.Stats.Refused.Load(), part.Stats.ResultPubs.Load(), part.WALSyncs())
}

// 重复命令与重复结果：参与方发完结果后 30% 概率 Nak（命令重投 → 读回执回放，不重复扣款），
// 30% 概率换 MsgID 再发一次结果（协调器按操作 tombstone 判重复）。变异 noreceipt 下余额核对必须失败。
func TestEndToEndDuplicateCommandsAndResults(t *testing.T) {
	e := newEnv(t, 2)
	part := e.participant(func(o *ParticipantOptions) { o.RedeliverRate, o.DupResultRate = 0.3, 0.3 })
	coord := e.coordinator("A", nil)
	e.takeAll(coord, false)
	all := ids("dup", 80)
	data := map[string]TransferData{}
	for i, id := range all {
		data[id] = e.transfer(i, i%4 == 0)
	}
	e.startAll(all, data, 60*time.Second)
	// 重投的命令可能在终态之后才被处理完：等参与方安静下来再核对。
	time.Sleep(time.Second)
	final := e.client.Latest()
	e.checkBalances(balanceOf(part), final, all)
	if v := e.chainViolations(); len(v) > 0 {
		t.Fatalf("event chain violations:\n%s", strings.Join(v, "\n"))
	}
	if part.Stats.Redelivered.Load() == 0 || part.Stats.Replayed.Load() == 0 || coord.Stats.Duplicate.Load() == 0 {
		t.Fatalf("premise: redelivered=%d replayed=%d duplicate results=%d, want all > 0",
			part.Stats.Redelivered.Load(), part.Stats.Replayed.Load(), coord.Stats.Duplicate.Load())
	}
	t.Logf("redelivered=%d receipt replays=%d duplicate results judged=%d", part.Stats.Redelivered.Load(), part.Stats.Replayed.Load(), coord.Stats.Duplicate.Load())
}

// 迟到成功：第一步只给一次尝试、300ms 截止；参与方在截止前执行（扣款生效），结果晚 800ms 才发。协调器放弃后
// saga 先到 Failed，迟到的成功把它重开、补偿这一步，最后 Compensated，余额复原（现行契约第 4 条）。
func TestEndToEndLateSuccessReopensAndCompensates(t *testing.T) {
	e := newEnv(t, 1)
	part := e.participant(func(o *ParticipantOptions) {
		o.DelayResult = func(cmd *Command) time.Duration {
			if strings.HasPrefix(cmd.SagaID, "late") && cmd.Phase == PhaseForward && cmd.Step == 0 {
				return 800 * time.Millisecond
			}
			return 0
		}
	})
	coord := e.coordinator("A", func(o *CoordinatorOptions) {
		o.Budget = Budget{TimeoutMs: 300, MaxAttempts: 1, BackoffMinMs: 50, BackoffMaxMs: 50}
	})
	e.takeAll(coord, false)
	late, normal := ids("late", 5), ids("ok", 5)
	all := append(append([]string{}, late...), normal...)
	data := map[string]TransferData{}
	for i, id := range all {
		data[id] = e.transfer(i, false)
	}
	e.startAll(all, data, 30*time.Second)
	defer func() {
		if t.Failed() {
			for _, id := range late {
				t.Logf("%s: %v", id, e.client.Latest()[id])
			}
			t.Logf("coord accepted=%d late=%d ignored=%d dup=%d; part executed=%d replayed=%d expired=%d",
				coord.Stats.Accepted.Load(), coord.Stats.LateCompensate.Load(), coord.Stats.Ignored.Load(), coord.Stats.Duplicate.Load(),
				part.Stats.Executed.Load(), part.Stats.Replayed.Load(), part.Stats.Expired.Load())
		}
	}()
	final := e.waitFor(10*time.Second, "late sagas compensated", func(m map[string]*Saga) bool {
		for _, id := range late {
			if m[id] == nil || m[id].Status != StatusCompensated {
				return false
			}
		}
		return true
	})
	for _, id := range late {
		if final[id].Reopened != 1 {
			t.Fatalf("%s: %s reopened=%d, want reopened once", id, final[id], final[id].Reopened)
		}
	}
	for _, id := range normal {
		if final[id].Status != StatusCompleted {
			t.Fatalf("%s: %s", id, final[id])
		}
	}
	e.checkBalances(balanceOf(part), final, all)
	if coord.Stats.LateCompensate.Load() != uint64(len(late)) {
		t.Fatalf("late compensations=%d, want %d", coord.Stats.LateCompensate.Load(), len(late))
	}
}

// 期望序号隔离旧写者：A 算完一轮（启动 s2）后被冻结在发布前；B 强占分区、重放、把 s2 做完；A 解冻后发布那一轮，
// 必须被 wrong last sequence 拒掉、分区停下，事件链没有分叉。变异 nofence 下 A 的旧事件落库，链核对必须失败。
func TestFenceRejectsStaleWriter(t *testing.T) {
	e := newEnv(t, 1)
	part := e.participant(nil)
	var mu sync.Mutex
	freeze := false
	release := make(chan struct{})
	frozen := make(chan struct{}, 1)
	a := e.coordinator("A", func(o *CoordinatorOptions) {
		o.LeaseTTL = time.Hour // 让租约在用例期间不过期、不续约失败：只看期望序号
		o.BeforePublish = func(int) {
			mu.Lock()
			f := freeze
			mu.Unlock()
			if f {
				select {
				case frozen <- struct{}{}:
				default:
				}
				<-release
			}
		}
	})
	e.takeAll(a, false)
	warm := ids("warm", 3)
	data := map[string]TransferData{}
	for i, id := range warm {
		data[id] = e.transfer(i, false)
	}
	e.startAll(warm, data, 30*time.Second)

	mu.Lock()
	freeze = true
	mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	data["s2"] = e.transfer(7, false)
	done, err := e.client.Start(ctx, "s2", data["s2"])
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-frozen:
	case <-ctx.Done():
		t.Fatal("A never reached the publish point")
	}
	b := e.coordinator("B", nil)
	e.takeAll(b, true)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("B did not finish s2")
	}
	mu.Lock()
	freeze = false
	mu.Unlock()
	close(release)
	deadline := time.Now().Add(10 * time.Second)
	for a.PartitionErr(0) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	errA := a.PartitionErr(0)
	time.Sleep(500 * time.Millisecond) // 让 A 的旧事件（若落库）有机会被参与方处理
	all := append(warm, "s2")
	final := e.client.Latest()
	e.checkBalances(balanceOf(part), final, all)
	if v := e.chainViolations(); len(v) > 0 {
		t.Fatalf("event chain forked (A err=%v):\n%s", errA, strings.Join(v, "\n"))
	}
	if !errors.Is(errA, ErrFenced) {
		t.Fatalf("stale writer A: %v, want ErrFenced", errA)
	}
	t.Logf("stale writer A stopped with: %v (fenced=%d)", errA, a.Stats.Fenced.Load())
}

// 快照 + 清理之后重放：A 每 10 条事件快照并清理；停 A，B 从“快照 + 尾巴”重建的状态与 A 的内存逐笔相同；
// 清理后 B 继续服务（期望序号仍成立：保留了最后一条事件）。
func TestSnapshotPurgeReplayMatches(t *testing.T) {
	e := newEnv(t, 2)
	part := e.participant(nil)
	snapOpts := func(o *CoordinatorOptions) { o.SnapshotEvery, o.Retention = 10, time.Hour }
	a := e.coordinator("A", snapOpts)
	e.takeAll(a, false)
	first := ids("snap", 120)
	data := map[string]TransferData{}
	for i, id := range first {
		data[id] = e.transfer(i, i%5 == 0)
	}
	e.startAll(first, data, 60*time.Second)
	time.Sleep(300 * time.Millisecond)
	a.Stop()
	want := map[int]map[string]*Saga{}
	for k := 0; k < e.l.Partitions; k++ {
		want[k] = a.SnapshotState(k)
	}
	if a.Stats.Snapshots.Load() == 0 || a.Stats.Purges.Load() == 0 {
		t.Fatalf("premise: snapshots=%d purges=%d", a.Stats.Snapshots.Load(), a.Stats.Purges.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, _ := e.js.Stream(ctx, e.l.StreamOf(0))
	info, _ := stream.Info(ctx)
	b := e.coordinator("B", snapOpts)
	e.takeAll(b, true)
	more := ids("after", 20)
	for i, id := range more {
		data[id] = e.transfer(i, false)
	}
	e.startAll(more, data, 60*time.Second)
	b.Stop()
	for k := 0; k < e.l.Partitions; k++ {
		got := b.SnapshotState(k)
		for id, s := range want[k] {
			g := got[id]
			wantJSON, _ := json.Marshal(s)
			gotJSON, _ := json.Marshal(g)
			if string(wantJSON) != string(gotJSON) {
				t.Fatalf("partition %d saga %s after replay:\n got %s\nwant %s", k, id, gotJSON, wantJSON)
			}
		}
	}
	all := append(first, more...)
	e.checkBalances(balanceOf(part), e.client.Latest(), all)
	t.Logf("A: ev=%d snapshots=%d purges=%d; stream msgs after purge=%d (first %d); B replayed %d msgs in %v",
		a.Stats.EvMsgs.Load(), a.Stats.Snapshots.Load(), a.Stats.Purges.Load(), info.State.Msgs, info.State.FirstSeq,
		b.Stats.ReplayMsgs.Load(), time.Duration(b.Stats.ReplayNanos.Load()))
}

// 发布结果未知：30% 的发布照常发出但不等 PubAck、按超时返回。协调器用同一 MsgID、同一期望序号重发：
// 单条判 Duplicate、批判 10201 或读回最后一条是自己的 → 已提交，内存与日志一致、不重复派发。
func TestPublishOutcomeUnknownIsResolved(t *testing.T) {
	e := newEnv(t, 2)
	part := e.participant(nil)
	var n atomic.Int64
	coord := e.coordinator("A", func(o *CoordinatorOptions) {
		o.DropAck = func(int) bool { return n.Add(1)%3 == 0 }
	})
	e.takeAll(coord, false)
	all := ids("unk", 80)
	data := map[string]TransferData{}
	for i, id := range all {
		data[id] = e.transfer(i, i%4 == 0)
	}
	e.startAll(all, data, 60*time.Second)
	final := e.client.Latest()
	for i, id := range all {
		want := StatusCompleted
		if i%4 == 0 {
			want = StatusCompensated
		}
		if final[id].Status != want {
			t.Fatalf("%s: %s, want %s", id, final[id], want)
		}
	}
	e.checkBalances(balanceOf(part), final, all)
	if v := e.chainViolations(); len(v) > 0 {
		t.Fatalf("event chain violations:\n%s", strings.Join(v, "\n"))
	}
	if coord.Stats.Unknown.Load() == 0 || coord.PartitionErr(0) != nil || coord.PartitionErr(1) != nil {
		t.Fatalf("premise: unknown=%d errs=%v/%v", coord.Stats.Unknown.Load(), coord.PartitionErr(0), coord.PartitionErr(1))
	}
	if coord.Stats.DupAcks.Load() == 0 || coord.Stats.ResolvedByLast.Load() == 0 {
		t.Fatalf("premise: both branches should be hit: single Duplicate=%d, batch resolved by last event=%d", coord.Stats.DupAcks.Load(), coord.Stats.ResolvedByLast.Load())
	}
	t.Logf("unknown outcomes=%d: single resolved as Duplicate=%d, batch resolved by duplicate-id / last event=%d (publish calls %d, batches %d)",
		coord.Stats.Unknown.Load(), coord.Stats.DupAcks.Load(), coord.Stats.ResolvedByLast.Load(), coord.Stats.PubCalls.Load(), coord.Stats.BatchCalls.Load())
}

// 分区分到两条流（各自的 leader）：布局与单写者检查不变，参与方 / 发起方在每条流上各有一个消费者。
func TestEndToEndAcrossStreamGroups(t *testing.T) {
	e := newEnvGroups(t, 4, 2)
	part := e.participant(nil)
	coord := e.coordinator("A", nil)
	e.takeAll(coord, false)
	all := ids("grp", 40)
	data := map[string]TransferData{}
	for i, id := range all {
		data[id] = e.transfer(i, i%3 == 0)
	}
	e.startAll(all, data, 60*time.Second)
	e.checkBalances(balanceOf(part), e.client.Latest(), all)
	if v := e.chainViolations(); len(v) > 0 {
		t.Fatalf("event chain violations:\n%s", strings.Join(v, "\n"))
	}
	if got := e.l.Streams(); len(got) != 2 {
		t.Fatalf("streams %v", got)
	}
}

// 参与方用 pipelined：锁内只入队，回复（结果发布）在 WAL 落盘之后（外化闸门），语义与 strict 相同。
func TestEndToEndPipelinedParticipant(t *testing.T) {
	e := newEnv(t, 2)
	part := e.participant(func(o *ParticipantOptions) { o.Durability = nest.DurabilityPipelined; o.RedeliverRate = 0.2 })
	coord := e.coordinator("A", nil)
	e.takeAll(coord, false)
	all := ids("pl", 60)
	data := map[string]TransferData{}
	for i, id := range all {
		data[id] = e.transfer(i, i%4 == 0)
	}
	e.startAll(all, data, 60*time.Second)
	time.Sleep(500 * time.Millisecond)
	e.checkBalances(balanceOf(part), e.client.Latest(), all)
	if v := e.chainViolations(); len(v) > 0 {
		t.Fatalf("event chain violations:\n%s", strings.Join(v, "\n"))
	}
}
