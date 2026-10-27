package jslogproto

// 投影打开（维护者 2026-10-27 决定 1A 的补测）：参与方经 DataEngine 把实体投影进 Mongo，协调器事件经冷副本投影写进 Mongo，
// 两者都不在关键路径上。核对：冷副本与观察到的终态逐笔一致；Mongo 里的账户与参与方内存逐账户一致。

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// checkMongoAccounts 等参与方投影追平，再核对 Mongo 账户余额 = 内存余额（逐账户）。
func (e *env) checkMongoAccounts(p *Participant) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := p.FlushProjection(ctx); err != nil {
		e.t.Fatal(err)
	}
	mongo, err := p.MongoBalances(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	var bad []string
	for n := int64(0); n < int64(e.pairs*2); n++ {
		mem, err := p.Balance(ctx, n)
		if err != nil {
			e.t.Fatal(err)
		}
		if got := mongo[accountEntityID(n)]; got != mem {
			bad = append(bad, fmt.Sprintf("account %d: mongo %d, memory %d", n, got, mem))
		}
	}
	if len(bad) > 0 {
		e.t.Fatalf("Mongo projection differs from the participant's entities:\n%s", strings.Join(bad, "\n"))
	}
}

// 冷副本开启（可选配置）：全部事件与只投影终态两种。
func TestProjectedEndToEnd(t *testing.T) {
	t.Run("all_events", func(t *testing.T) { projectedEndToEnd(t, false) })
	t.Run("terminal_only", func(t *testing.T) { projectedEndToEnd(t, true) })
}

// 缺省配置：参与方投影开、冷副本关；清理守卫不等冷副本。
func TestParticipantProjectionWithoutColdCopy(t *testing.T) {
	e := newEnv(t, 4)
	e.withMongo()
	part := e.participant(nil)
	coord := e.coordinator("A", func(o *CoordinatorOptions) { o.SnapshotEvery = 10 })
	e.takeAll(coord, false)
	all := ids("pp", 80)
	data := map[string]TransferData{}
	for i, id := range all {
		data[id] = e.transfer(i, i%4 == 0)
	}
	e.startAll(all, data, 60*time.Second)
	e.checkBalances(balanceOf(part), e.client.Latest(), all)
	e.checkMongoAccounts(part)
	if coord.Stats.Purges.Load() == 0 {
		t.Fatalf("with the cold copy off the purge guard must not wait for it: purges=%d", coord.Stats.Purges.Load())
	}
}

func projectedEndToEnd(t *testing.T, terminalOnly bool) {
	e := newEnv(t, 4)
	e.withMongo()
	part := e.participant(nil)
	sp := NewSagaProjector(e.js, e.l, e.mongo, e.db, quietLog())
	sp.TerminalOnly = terminalOnly
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sp.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sp.Stop)
	e.proj = sp
	coord := e.coordinator("A", nil)
	e.takeAll(coord, false)
	all := ids("prj", 100)
	data := map[string]TransferData{}
	for i, id := range all {
		data[id] = e.transfer(i, i%4 == 0)
	}
	e.startAll(all, data, 60*time.Second)
	e.checkBalances(balanceOf(part), e.client.Latest(), all)
	caught := e.waitColdCopy(sp, all, 30*time.Second)
	e.checkMongoAccounts(part)
	t.Logf("cold copy caught up %v after the last terminal state; saga projection events=%d writes=%d batches=%d; participant WAL unacked=%d",
		caught, sp.Events.Load(), sp.Writes.Load(), sp.Batches.Load(), part.ProjectionBacklog())
}
