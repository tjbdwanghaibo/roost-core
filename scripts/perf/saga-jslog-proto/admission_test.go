package jslogproto

// 按投影积压准入（维护者 2026-10-27 选第三轮 §6 的 A）。参与方投影写本机隔离 Mongo，经“闸门”包装：闸门关上时投影写入阻塞，
// 积压（WALUnacked）确定地涨上去。核对：超阈值时新启动被拒（发起方快速失败 ErrBackpressure；已写进的启动意图终态 Rejected），
// 在途 saga 全部到终态，积压回落后恢复准入（被拒的 ID 可以再发起），积压信号过期时关闭准入。变异 SAGAJS_MUTATE=noadmit 必须变红。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
)

// writeGate 关上时，经 gatedMongo 的写入与会话开启阻塞到重新打开（或 ctx 到期）。
type writeGate struct {
	mu   sync.Mutex
	open chan struct{}
}

func newWriteGate() *writeGate {
	g := &writeGate{open: make(chan struct{})}
	close(g.open)
	return g
}

func (g *writeGate) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.open:
		g.open = make(chan struct{})
	default:
	}
}

func (g *writeGate) Open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.open:
	default:
		close(g.open)
	}
}

func (g *writeGate) wait(ctx context.Context) error {
	g.mu.Lock()
	ch := g.open
	g.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type gatedMongo struct {
	fmongo.IMongo
	gate *writeGate
}

func (m gatedMongo) Database(name string) fmongo.IDatabase {
	return gatedDB{IDatabase: m.IMongo.Database(name), gate: m.gate}
}

func (m gatedMongo) StartSession(ctx context.Context) (fmongo.ISession, error) {
	if err := m.gate.wait(ctx); err != nil {
		return nil, err
	}
	return m.IMongo.StartSession(ctx)
}

type gatedDB struct {
	fmongo.IDatabase
	gate *writeGate
}

func (d gatedDB) Collection(name string) fmongo.ICollection {
	return gatedColl{ICollection: d.IDatabase.Collection(name), gate: d.gate}
}

type gatedColl struct {
	fmongo.ICollection
	gate *writeGate
}

func (c gatedColl) UpdateOne(ctx context.Context, filter any, update any) (*fmongo.UpdateResult, error) {
	if err := c.gate.wait(ctx); err != nil {
		return nil, err
	}
	return c.ICollection.UpdateOne(ctx, filter, update)
}

func (c gatedColl) FindOneAndUpdate(ctx context.Context, filter any, update any, result any, opts ...fmongo.FindOneAndUpdateOption) error {
	if err := c.gate.wait(ctx); err != nil {
		return err
	}
	return c.ICollection.FindOneAndUpdate(ctx, filter, update, result, opts...)
}

func (c gatedColl) BulkWrite(ctx context.Context, models []fmongo.WriteModel) (*fmongo.BulkWriteResult, error) {
	if err := c.gate.wait(ctx); err != nil {
		return nil, err
	}
	return c.ICollection.BulkWrite(ctx, models)
}

func (c gatedColl) InsertOne(ctx context.Context, doc any) (string, error) {
	if err := c.gate.wait(ctx); err != nil {
		return "", err
	}
	return c.ICollection.InsertOne(ctx, doc)
}

func waitUntil(t *testing.T, timeout time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func admissionEnv(t *testing.T, mod func(*ParticipantOptions)) (*env, *Participant, *Coordinator, *writeGate) {
	e := newEnv(t, 2)
	e.withMongo()
	gate := newWriteGate()
	part := e.participant(func(o *ParticipantOptions) {
		o.Mongo = gatedMongo{IMongo: e.mongo, gate: gate}
		o.LoadTick = 50 * time.Millisecond
		if mod != nil {
			mod(o)
		}
	})
	t.Cleanup(gate.Open) // 关闭参与方前放开闸门，免得投影器卡在写上
	coord := e.coordinator("A", func(o *CoordinatorOptions) {
		o.Admission = &AdmissionOptions{HighSagas: 40, LowSagas: 5, StaleAfter: time.Second, Tick: 50 * time.Millisecond}
	})
	e.takeAll(coord, false)
	return e, part, coord, gate
}

func TestAdmissionClosesOnProjectionBacklogAndReopens(t *testing.T) {
	e, part, coord, gate := admissionEnv(t, func(o *ParticipantOptions) {
		o.DelayResult = func(cmd *Command) time.Duration {
			if strings.HasPrefix(cmd.SagaID, "slow") {
				return 2 * time.Second
			}
			return 0
		}
	})
	data := map[string]TransferData{}
	var all []string
	add := func(prefix string, n int) []string {
		ids := ids(prefix, n)
		for i, id := range ids {
			data[id] = e.transfer(len(all)+i, false)
		}
		all = append(all, ids...)
		return ids
	}
	e.startAll(add("warm", 6), data, 30*time.Second)

	// 投影写入卡住，积压随后续负载涨过阈值（40 笔）、准入关闭。“在途”的 8 笔 slow 先发起、每步结果晚 2s 才发，
	// 准入关闭时它们还没到终态，必须照常推进到 Completed。负载的 60 笔里，准入关闭之前处理的被接纳，之后的被拒；
	// 被接纳的必须全部完成。
	gate.Close()
	slow := add("slow", 8)
	ctx0, cancel0 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel0()
	var slowChans []<-chan struct{}
	for _, id := range slow {
		ch, err := e.client.StartIntent(ctx0, id, data[id])
		if err != nil {
			t.Fatal(err)
		}
		slowChans = append(slowChans, ch)
	}
	load := add("load", 60)
	e.startIntents(load, data, 60*time.Second)
	waitUntil(t, 10*time.Second, "admission closed by backlog", func() bool {
		open, reason, _ := coord.AdmissionState()
		closed, _ := e.client.AdmissionClosed()
		return !open && reason == ReasonBacklog && closed
	})
	inflightAtClose := 0
	for _, id := range slow {
		if s := e.client.Latest()[id]; s == nil || !s.Status.Terminal() {
			inflightAtClose++
		}
	}
	if inflightAtClose == 0 {
		t.Fatalf("premise: no slow saga was in flight when admission closed")
	}
	for _, ch := range slowChans {
		<-ch
	}
	admitted, refused := 0, 0
	for _, id := range append(append([]string{}, slow...), load...) {
		switch s := e.client.Latest()[id]; s.Status {
		case StatusCompleted:
			admitted++
		case StatusRejected:
			refused++
		default:
			t.Fatalf("%s: %s, want completed (admitted) or rejected", id, s)
		}
	}
	for _, id := range slow {
		if s := e.client.Latest()[id]; s.Status != StatusCompleted {
			t.Fatalf("in-flight %s when admission closed: %s, want completed", id, s)
		}
	}
	t.Logf("%d slow sagas in flight when admission closed, all completed; load: %d admitted+completed, %d rejected", inflightAtClose, admitted, refused)
	_, _, sagas := coord.AdmissionState()
	t.Logf("backlog %d records (%.1f sagas) with the projection gate closed; admission closed", part.ProjectionBacklog(), sagas)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// 发起方快速失败：不写启动意图。
	if _, err := e.client.Start(ctx, "fast-0", e.transfer(0, false)); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("Start while admission is closed: err=%v, want ErrBackpressure", err)
	}
	// 绕过提示直接写意图：协调器权威拒绝，终态 Rejected（可重试），不派发任何步骤。
	rejected := add("rej", 3)
	e.startIntents(rejected, data, 30*time.Second)
	for _, id := range rejected {
		s := e.client.Latest()[id]
		if s.Status != StatusRejected || !strings.HasPrefix(s.Reason, "backpressure") || len(s.Completed) != 0 {
			t.Fatalf("start of %s while backlogged: %s reason=%q, want rejected/backpressure", id, s, s.Reason)
		}
	}

	// 放开投影：积压回落到低水位以下，准入重新打开；被拒的 ID 再发起成功。
	gate.Open()
	waitUntil(t, 30*time.Second, "admission reopened", func() bool {
		open, _, _ := coord.AdmissionState()
		closed, _ := e.client.AdmissionClosed()
		return open && !closed
	})
	var again []string
	for _, id := range append(append([]string{}, load...), rejected...) {
		if e.client.Latest()[id].Status == StatusRejected {
			again = append(again, id)
		}
	}
	again = append(again, add("after", 6)...)
	e.startAll(again, data, 30*time.Second)
	final := e.client.Latest()
	for _, id := range again {
		if final[id].Status != StatusCompleted {
			t.Fatalf("%s after admission reopened: %s", id, final[id])
		}
	}
	e.checkBalances(balanceOf(part), final, all)
	e.checkMongoAccounts(part)
	if v := e.chainViolations(); len(v) > 0 {
		t.Fatalf("event chain violations:\n%s", strings.Join(v, "\n"))
	}
	t.Logf("rejected starts=%d; all %d sagas accounted for", coord.Stats.Rejected.Load(), len(all))
}

// 积压信号过期（参与方停更）：关闭准入（原因 stale），在途照常完成；信号恢复后重新打开。
func TestAdmissionClosesWhenTheBacklogSignalGoesStale(t *testing.T) {
	var paused atomic.Bool
	e, part, coord, _ := admissionEnv(t, func(o *ParticipantOptions) { o.PauseLoad = paused.Load })
	data := map[string]TransferData{}
	var all []string
	add := func(prefix string, n int) []string {
		ids := ids(prefix, n)
		for i, id := range ids {
			data[id] = e.transfer(len(all)+i, false)
		}
		all = append(all, ids...)
		return ids
	}
	e.startAll(add("warm", 4), data, 30*time.Second)
	inflight := add("inflight", 20)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var chans []<-chan struct{}
	for _, id := range inflight {
		ch, err := e.client.Start(ctx, id, data[id])
		if err != nil {
			t.Fatal(err)
		}
		chans = append(chans, ch)
	}
	paused.Store(true)
	for _, ch := range chans {
		<-ch
	}
	waitUntil(t, 10*time.Second, "admission closed by a stale signal", func() bool {
		open, reason, _ := coord.AdmissionState()
		return !open && reason == ReasonStale
	})
	stale := add("stale", 2)
	e.startIntents(stale, data, 30*time.Second)
	for _, id := range stale {
		if s := e.client.Latest()[id]; s.Status != StatusRejected || s.Reason != ReasonStale {
			t.Fatalf("start of %s with a stale signal: %s reason=%q", id, s, s.Reason)
		}
	}
	paused.Store(false)
	waitUntil(t, 10*time.Second, "admission reopened after the signal resumed", func() bool {
		open, _, _ := coord.AdmissionState()
		closed, _ := e.client.AdmissionClosed()
		return open && !closed
	})
	e.startAll(stale, data, 30*time.Second)
	final := e.client.Latest()
	for _, id := range append(inflight, stale...) {
		if final[id].Status != StatusCompleted {
			t.Fatalf("%s: %s", id, final[id])
		}
	}
	e.checkBalances(balanceOf(part), final, all)
	t.Logf("stale-signal rejections=%d", coord.Stats.Rejected.Load())
}

// startIntents 绕过准入提示写启动意图，等每一笔第一次到终态。
func (e *env) startIntents(ids []string, data map[string]TransferData, timeout time.Duration) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for _, id := range ids {
		ch, err := e.client.StartIntent(ctx, id, data[id])
		if err != nil {
			e.t.Fatal(err)
		}
		select {
		case <-ch:
		case <-ctx.Done():
			e.t.Fatal(fmt.Errorf("%s not terminal within %v", id, timeout))
		}
	}
}

// 最后一道硬上限：参与方 MaxUnackedRecords（DataEngine 在 WAL 前拒绝）。没有准入时积压顶到硬上限，步骤事务被拒、命令 Nak 重投；
// 投影恢复后积压回落，重投成功，saga 全部完成——前提是步骤预算覆盖积压回落的时间（这里放开闸门只用 1s，远小于 5s × 5 次）。
func TestHardLimitRejectsStepsUntilProjectionCatchesUp(t *testing.T) {
	e := newEnv(t, 2)
	e.withMongo()
	e.pairs = 8 // 开户 16 条记录，低于硬上限
	gate := newWriteGate()
	part := e.participant(func(o *ParticipantOptions) {
		o.Mongo = gatedMongo{IMongo: e.mongo, gate: gate}
		o.HardLimit = 20
	})
	t.Cleanup(gate.Open)
	coord := e.coordinator("A", nil)
	e.takeAll(coord, false)
	all := ids("hard", 30)
	data := map[string]TransferData{}
	for i, id := range all {
		data[id] = e.transfer(i, false)
	}
	gate.Close()
	go func() {
		time.Sleep(time.Second)
		gate.Open()
	}()
	e.startAll(all, data, 60*time.Second)
	final := e.client.Latest()
	for _, id := range all {
		if final[id].Status != StatusCompleted {
			t.Fatalf("%s: %s", id, final[id])
		}
	}
	if part.Stats.NestErrors.Load() == 0 {
		t.Fatalf("premise: the hard limit never rejected a step (nest errors 0)")
	}
	e.checkBalances(balanceOf(part), final, all)
	e.checkMongoAccounts(part)
	t.Logf("steps refused by the hard limit and retried: %d", part.Stats.NestErrors.Load())
}
