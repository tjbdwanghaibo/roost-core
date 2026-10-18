package entitytimer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

const nestTestKind entity.EntityKind = 241

// nestOwner 是装了一个 Host 的实体；store 由 memStore 扮演 DAO（写入登记逆操作，随事务回滚）。
type nestOwner struct {
	*entity.EntityBase
	host *Host
}

func (o *nestOwner) Base() *entity.EntityBase { return o.EntityBase }

type nullCommitter struct{}

func (nullCommitter) Commit(context.Context, dataengine.CommitRecord) error { return nil }

var errBusiness = errors.New("business refused")

// 经真实 Nest 引擎：事务提交后（AfterCommit）索引才变；回滚的事务不改索引；驱动投递的 Tick 走 Dispatch
// 正式准入，在实体锁内执行，提交后的报告清掉“待报告”。
func TestHostReportsThroughNestTransactions(t *testing.T) {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: nestTestKind, Category: entity.EntityCategoryOther})
	id, err := entity.BuildEntityID(9, nestTestKind)
	if err != nil {
		t.Fatal(err)
	}
	manager := entity.NewEntityManager()
	access := entity.NewManagerAccess(manager)
	owner := &nestOwner{EntityBase: entity.NewEntityBase(id, entity.EntityCategoryOther, false, nestTestKind)}
	manager.Add(owner)

	nest.ResetHandlersForTest()
	t.Cleanup(nest.ResetHandlersForTest)
	tickName := nest.NewHandlerName("handlerTickNestTestTimer")
	d := NewDriver(Config{Name: "nest.timer", Handler: tickName})
	clk := newFakeClock()
	d.SetClock(clk.Now)
	store := newMemStore()
	f := &fires{}
	owner.host = d.NewHost(owner, store, f.register)
	if err := owner.host.OnInitFinish(true); err != nil {
		t.Fatal(err)
	}

	meta := nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict}
	ticked := make(chan int, 4)
	nest.MustRegisterHandlerWithMeta(tickName, func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		remaining := es[0].(*nestOwner).host.Tick(time.UnixMilli(params[0].(int64)))
		ticked <- remaining
		return nil, nil
	}, meta)
	arm := nest.NewHandlerName("test_entitytimer_arm")
	nest.MustRegisterHandlerWithMeta(arm, func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		es[0].(*nestOwner).host.NewTimer(params[0].(time.Duration), typeBuff, 0, 0, nil)
		if params[1].(bool) {
			return nil, errBusiness
		}
		return nil, nil
	}, meta)

	engine := nest.NewEngine(nest.NestOptionWithGetter(access), nest.NestOptionWithTransactionCommitter(nullCommitter{}),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 2, QueueCap: 64}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}))
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	if err := d.Bind(engine, func(id int64) entity.IThreadSafeEntity { return manager.Get(id) }); err != nil {
		t.Fatal(err)
	}

	// 回滚：节点随 DAO 撤销，索引也没有动。
	if _, err := engine.Request(context.Background(), arm, id, nest.NewParams(5*time.Second, true)); !errors.Is(err, errBusiness) {
		t.Fatalf("failing arm = %v", err)
	}
	if len(store.nodes) != 0 || d.Pending() != 0 {
		t.Fatalf("after a rolled-back arm: nodes %d, pending %d", len(store.nodes), d.Pending())
	}
	// 提交：Request 在提交（和提交后回调）之后才返回，索引已经是新期限。
	if _, err := engine.Request(context.Background(), arm, id, nest.NewParams(5*time.Second, false)); err != nil {
		t.Fatal(err)
	}
	if due, ok := d.DueOf(owner); !ok || !due.Equal(clk.Now().Add(5*time.Second)) || len(store.nodes) != 1 {
		t.Fatalf("after a committed arm: due %v %v, nodes %d", due, ok, len(store.nodes))
	}

	clk.Advance(5 * time.Second)
	if n := d.DeliverDue(context.Background()); n != 1 {
		t.Fatalf("delivered %d", n)
	}
	select {
	case remaining := <-ticked:
		if remaining != 0 {
			t.Fatalf("remaining after the tick = %d", remaining)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the delivered tick never ran")
	}
	if len(f.got) != 1 || f.got[0].Overdue {
		t.Fatalf("fires = %+v", f.got)
	}
	// 报告经 AfterCommit 在 handler 释放锁之后到达；等它把“待报告”清掉（期限为空，条目离开索引）。
	deadline := time.Now().Add(10 * time.Second)
	for d.Pending() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the tick's report never cleared the in-flight entry (pending %d)", d.Pending())
		}
		time.Sleep(time.Millisecond)
	}
}
