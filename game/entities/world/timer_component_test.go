package world

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	db "example.com/planet/db"
	"github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// A deadline that does not survive a restart is not a deadline.
//
// The World's timer heap lives in memory and is written through to the DAO by
// the scheduler's change hook, so these assert the two halves that make it a
// durable schedule: arming one writes it, and an entity rebuilt from that
// document fires it. The second half is the one nothing else covers — the
// process that armed the timer is precisely the one that is gone.

type worldCommitter struct{ records []dataengine.CommitRecord }

func (c *worldCommitter) Commit(_ context.Context, record dataengine.CommitRecord) error {
	c.records = append(c.records, dataengine.CloneCommitRecord(record))
	return nil
}

const testWorldUniqueID = int64(9001)

func newWorld(t *testing.T) *World {
	t.Helper()
	RegisterEntity()
	id, err := entity.BuildEntityID(testWorldUniqueID, EntityKindWorld)
	if err != nil {
		t.Fatal(err)
	}
	built, err := entity.BuildEntity(&entity.EntityCreateParam{IsCreate: true, Kind: EntityKindWorld, Id: id})
	if err != nil {
		t.Fatalf("build world: %v", err)
	}
	return built.(*World)
}

// restartWorld is the load path in miniature: take what the first World would
// have stored, hand it to a fresh DAO, and build a second World on it.
func restartWorld(t *testing.T, previous *World) *World {
	t.Helper()
	restored := db.NewWorldDao()
	if err := restored.RestorePersisted(previous.Dao().Marshal(), db.WorldDaoSchemaVersion, 1); err != nil {
		t.Fatalf("restore the stored world: %v", err)
	}
	id, err := entity.BuildEntityID(testWorldUniqueID, EntityKindWorld)
	if err != nil {
		t.Fatal(err)
	}
	built, err := entity.BuildEntity(&entity.EntityCreateParam{
		Kind: EntityKindWorld, Id: id,
		Dao: map[string]entity.DaoInterface{"world": restored},
	})
	if err != nil {
		t.Fatalf("rebuild world: %v", err)
	}
	return built.(*World)
}

func TestArmingADeadlineWritesIt(t *testing.T) {
	subject := newWorld(t)
	committer := &worldCommitter{}
	now := time.Unix(1_800_000_000, 0)

	if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "arm", func() (any, error) {
		if _, armed := subject.TimerComp().ScheduleActivityPhase("race-1800000000", now.Add(time.Minute), now); !armed {
			t.Error("the first arm reported that it changed nothing")
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("arm: %v", err)
	}
	if len(committer.records) != 1 {
		t.Fatalf("arming a deadline produced %d commit records, want 1: the heap and the transaction that armed it are one record", len(committer.records))
	}
	if got := subject.Dao().TimersLen(); got != 1 {
		t.Fatalf("the World stores %d timer nodes, want 1", got)
	}
	if subject.Dao().GetTimerSeed() <= 0 {
		t.Error("the node id seed was not stored; a restart could mint an id a stored node already has")
	}

	// Arming the same window again must not add a second node: every process
	// that loads this World tries to arm the window it computed from the
	// clock, and they all compute the same one.
	if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "arm_again", func() (any, error) {
		if _, armed := subject.TimerComp().ScheduleActivityPhase("race-1800000000", now.Add(time.Minute), now); armed {
			t.Error("arming the same window twice armed a second deadline")
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("arm again: %v", err)
	}
	if got := subject.Dao().TimersLen(); got != 1 {
		t.Fatalf("the World stores %d timer nodes after a repeat arm, want 1", got)
	}
}

// The process that armed the deadline is gone; the deadline is not.
func TestADeadlineSurvivesARestartAndFires(t *testing.T) {
	first := newWorld(t)
	committer := &worldCommitter{}
	now := time.Unix(1_800_000_000, 0)
	const activityID = "race-1800000000"

	if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "arm", func() (any, error) {
		first.TimerComp().ScheduleActivityPhase(activityID, now.Add(time.Minute), now)
		return nil, nil
	}); err != nil {
		t.Fatalf("arm: %v", err)
	}

	restarted := restartWorld(t, first)
	if _, armed := restarted.TimerComp().PendingActivityPhase(activityID); !armed {
		t.Fatal("the restarted World has no deadline; a stored node was not rebuilt into the heap")
	}

	// Before it is due, nothing happens and nothing is written.
	committer.records = nil
	if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "early_tick", func() (any, error) {
		if remaining := restarted.TimerComp().Tick(now.Add(30 * time.Second)); remaining != 1 {
			t.Errorf("an early tick left %d nodes, want the deadline still armed", remaining)
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("early tick: %v", err)
	}
	if len(committer.records) != 0 {
		t.Errorf("a tick that fired nothing wrote %d commit records", len(committer.records))
	}

	// Due: it fires, records its effect and retires — in one record.
	committer.records = nil
	if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "tick", func() (any, error) {
		if remaining := restarted.TimerComp().Tick(now.Add(2 * time.Minute)); remaining != 0 {
			t.Errorf("the deadline fired but %d nodes remain", remaining)
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(committer.records) != 1 {
		t.Fatalf("firing produced %d commit records, want 1", len(committer.records))
	}
	if effects := committer.records[0].Effects; len(effects) != 1 {
		t.Fatalf("the firing record carries %d effects, want the phase notification", len(effects))
	} else if effects[0].Topic != "activity.phase_due" {
		t.Errorf("the effect is %q", effects[0].Topic)
	}
	if restarted.Dao().TimersLen() != 0 {
		t.Error("the fired node is still stored; it would fire again after the next restart")
	}
}

// rejectingWorldCommitter refuses every record, the way a failed WAL
// admission does: the handler body already ran and Nest takes it all back.
type rejectingWorldCommitter struct{ err error }

func (c rejectingWorldCommitter) Commit(context.Context, dataengine.CommitRecord) error { return c.err }

// RR-20261005-NC-140：定时器堆是组件内存，Nest 的 undo 只撤回 DAO。事务失败或提交被拒绝后，
// 堆与 DAO 必须一起回到事务开始时的样子：武装被撤回就不能还“已武装”，触发被撤回就必须还能再触发。
func TestTheTimerHeapRollsBackWithTheTransaction(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	const activityID = "race-1800000000"
	failures := []struct {
		name      string
		committer nest.TransactionCommitter
		failAfter bool
	}{
		{name: "the commit is rejected", committer: rejectingWorldCommitter{err: errors.New("admission refused")}},
		{name: "a later step fails", committer: &worldCommitter{}, failAfter: true},
	}
	for _, tc := range failures {
		t.Run("arm/"+tc.name, func(t *testing.T) {
			subject := newWorld(t)
			_, err := nest.RunIsolatedTransaction(context.Background(), tc.committer, "arm_then_fail", func() (any, error) {
				subject.TimerComp().ScheduleActivityPhase(activityID, now.Add(time.Minute), now)
				if tc.failAfter {
					return nil, errors.New("a later step of the same handler failed")
				}
				return nil, nil
			})
			if err == nil {
				t.Fatal("the transaction was expected to fail")
			}
			if got := subject.Dao().TimersLen(); got != 0 {
				t.Fatalf("the DAO kept %d timer nodes after the rollback", got)
			}
			if id, armed := subject.TimerComp().PendingActivityPhase(activityID); armed {
				t.Fatalf("rolled back, but the heap still holds node %d: every later arm is refused as a repeat and the stored World has no deadline", id)
			}
			committer := &worldCommitter{}
			if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "arm", func() (any, error) {
				if _, armed := subject.TimerComp().ScheduleActivityPhase(activityID, now.Add(time.Minute), now); !armed {
					t.Error("the arm after a rolled-back one was refused as a repeat")
				}
				return nil, nil
			}); err != nil {
				t.Fatalf("arm: %v", err)
			}
			if got := subject.Dao().TimersLen(); got != 1 {
				t.Fatalf("after a committed arm the World stores %d timer nodes, want 1", got)
			}
		})
		t.Run("tick/"+tc.name, func(t *testing.T) {
			subject := newWorld(t)
			if _, err := nest.RunIsolatedTransaction(context.Background(), &worldCommitter{}, "arm", func() (any, error) {
				subject.TimerComp().ScheduleActivityPhase(activityID, now.Add(time.Minute), now)
				return nil, nil
			}); err != nil {
				t.Fatalf("arm: %v", err)
			}
			_, err := nest.RunIsolatedTransaction(context.Background(), tc.committer, "tick_then_fail", func() (any, error) {
				subject.TimerComp().Tick(now.Add(2 * time.Minute))
				if tc.failAfter {
					return nil, errors.New("a later step of the same handler failed")
				}
				return nil, nil
			})
			if err == nil {
				t.Fatal("the transaction was expected to fail")
			}
			if got := subject.Dao().TimersLen(); got != 1 {
				t.Fatalf("the DAO holds %d timer nodes after the rolled-back tick, want the deadline back", got)
			}
			if _, armed := subject.TimerComp().PendingActivityPhase(activityID); !armed {
				t.Fatal("the rolled-back tick took the deadline out of the heap: its effect was rolled back too, so nothing reports the phase until a restart")
			}
			committer := &worldCommitter{}
			if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "tick", func() (any, error) {
				subject.TimerComp().Tick(now.Add(3 * time.Minute))
				return nil, nil
			}); err != nil {
				t.Fatalf("tick: %v", err)
			}
			if len(committer.records) != 1 || len(committer.records[0].Effects) != 1 {
				t.Fatalf("the retried tick did not fire the deadline: %d records", len(committer.records))
			}
			if got := subject.Dao().TimersLen(); got != 0 {
				t.Fatalf("the fired node is still stored (%d)", got)
			}
		})
	}
}

// A deadline already in the past when the process starts still fires,
// rather than being refused for being late: the window it closes is the one
// nobody closed while the process was down. It fires on the next tick.
//
// RR-20261005-NC-142：这里原来只断言 armed 与 Tick 后“剩余 0 个节点”——节点根本没建出来时它也通过。
// 现在断言节点存在、被存储，并且下一次 Tick 带着 effect 触发。
func TestAnOverdueDeadlineIsArmedAndFires(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{name: "an hour late", at: now.Add(-time.Hour)},
		{name: "due this instant", at: now},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subject := newWorld(t)
			const activityID = "race-1799999000"
			committer := &worldCommitter{}
			var id int64
			if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "arm_late", func() (any, error) {
				var armed bool
				id, armed = subject.TimerComp().ScheduleActivityPhase(activityID, tc.at, now)
				if !armed {
					t.Error("the overdue deadline was refused")
				}
				return nil, nil
			}); err != nil {
				t.Fatalf("arm: %v", err)
			}
			if id == 0 {
				t.Error("ScheduleActivityPhase reported armed with node id 0")
			}
			if _, armed := subject.TimerComp().PendingActivityPhase(activityID); !armed {
				t.Fatal("reported armed, but no node is pending: the window this deadline closes is never reported")
			}
			if got := subject.Dao().TimersLen(); got != 1 {
				t.Fatalf("the World stores %d timer nodes, want 1", got)
			}
			committer.records = nil
			if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "tick", func() (any, error) {
				subject.TimerComp().Tick(now.Add(time.Second))
				return nil, nil
			}); err != nil {
				t.Fatalf("tick: %v", err)
			}
			if len(committer.records) != 1 || len(committer.records[0].Effects) != 1 {
				t.Fatalf("the next tick did not fire the overdue deadline: %d records", len(committer.records))
			}
		})
	}
}

// A1（回滚统一走 DAO，docs/feature/REFACTOR-2026-10-05-dao-unified-rollback.md）：定时器的回滚就是 DAO 的回滚。
// 经真实 Nest 派发，两种回滚策略 × 两条失败路径，分别撤回一次武装和一次触发。承诺：回滚后 DAO 与组件看到的
// 待触发节点一致（撤回的武装不再“已武装”，撤回的触发还能再触发），并且之后的一次成功操作照常写一条记录。
// 组件里没有任何逆操作登记，能通过只因为定时器的全部状态都在 DAO 里。
func TestTimerRollbackIsTheDaoRollback(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	const activityID = "race-1800000000"
	policies := []struct {
		name   string
		policy nest.RollbackPolicy
	}{
		{name: "undo", policy: nest.RollbackUndo},
		{name: "state", policy: nest.RollbackState},
	}
	failures := []struct {
		name      string
		committer nest.TransactionCommitter
		failAfter bool
	}{
		{name: "handler fails", committer: &worldCommitter{}, failAfter: true},
		{name: "commit rejected", committer: rejectingWorldCommitter{err: errors.New("admission refused")}},
	}
	for _, policy := range policies {
		meta := nest.HandlerMeta{Rollback: policy.policy, Durability: nest.DurabilityStrict}
		for _, failure := range failures {
			t.Run("arm/"+policy.name+"/"+failure.name, func(t *testing.T) {
				subject := newWorld(t)
				err := runWorldHandler(t, subject, meta, failure.committer, func(w *World) error {
					w.TimerComp().ScheduleActivityPhase(activityID, now.Add(time.Minute), now)
					if failure.failAfter {
						return errors.New("a later step of the same handler failed")
					}
					return nil
				})
				if err == nil {
					t.Fatal("the transaction was expected to fail")
				}
				if got := subject.Dao().TimersLen(); got != 0 {
					t.Fatalf("the DAO kept %d timer nodes after the rollback", got)
				}
				if id, armed := subject.TimerComp().PendingActivityPhase(activityID); armed {
					t.Fatalf("rolled back, but node %d is still pending", id)
				}
				// Nothing is due: a tick after the rolled-back arm fires nothing.
				if remaining := subject.TimerComp().Tick(now.Add(2 * time.Minute)); remaining != 0 {
					t.Fatalf("a tick after the rolled-back arm reports %d nodes", remaining)
				}
				committer := &worldCommitter{}
				if err := runWorldHandler(t, subject, meta, committer, func(w *World) error {
					if _, armed := w.TimerComp().ScheduleActivityPhase(activityID, now.Add(time.Minute), now); !armed {
						return errors.New("the arm after a rolled-back one was refused as a repeat")
					}
					return nil
				}); err != nil {
					t.Fatalf("arm: %v", err)
				}
				if got := subject.Dao().TimersLen(); got != 1 || len(committer.records) != 1 {
					t.Fatalf("after a committed arm: %d nodes, %d records; want 1 and 1", got, len(committer.records))
				}
			})
			t.Run("tick/"+policy.name+"/"+failure.name, func(t *testing.T) {
				subject := newWorld(t)
				if err := runWorldHandler(t, subject, meta, &worldCommitter{}, func(w *World) error {
					w.TimerComp().ScheduleActivityPhase(activityID, now.Add(time.Minute), now)
					return nil
				}); err != nil {
					t.Fatalf("arm: %v", err)
				}
				err := runWorldHandler(t, subject, meta, failure.committer, func(w *World) error {
					w.TimerComp().Tick(now.Add(2 * time.Minute))
					if failure.failAfter {
						return errors.New("a later step of the same handler failed")
					}
					return nil
				})
				if err == nil {
					t.Fatal("the transaction was expected to fail")
				}
				if got := subject.Dao().TimersLen(); got != 1 {
					t.Fatalf("the DAO holds %d timer nodes after the rolled-back tick, want the deadline back", got)
				}
				if _, armed := subject.TimerComp().PendingActivityPhase(activityID); !armed {
					t.Fatal("the rolled-back tick took the deadline away")
				}
				committer := &worldCommitter{}
				if err := runWorldHandler(t, subject, meta, committer, func(w *World) error {
					w.TimerComp().Tick(now.Add(3 * time.Minute))
					return nil
				}); err != nil {
					t.Fatalf("tick: %v", err)
				}
				if len(committer.records) != 1 || len(committer.records[0].Effects) != 1 {
					t.Fatalf("the retried tick did not fire the deadline: %d records", len(committer.records))
				}
				if got := subject.Dao().TimersLen(); got != 0 {
					t.Fatalf("the fired node is still stored (%d)", got)
				}
			})
		}
	}
}

// A1：定时器只把节点与种子写进提交记录（也就是 WAL 收到的记录）；组件为空转 Tick 记的最早到期时间是
// 非持久字段，不在记录里。重启后从存储构建的 World 照样按时触发（TestADeadlineSurvivesARestartAndFires）。
func TestTimerBookkeepingStaysOutOfTheCommitRecord(t *testing.T) {
	subject := newWorld(t)
	committer := &worldCommitter{}
	now := time.Unix(1_800_000_000, 0)
	for _, activityID := range []string{"race-a", "race-b"} {
		if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "arm", func() (any, error) {
			subject.TimerComp().ScheduleActivityPhase(activityID, now.Add(time.Minute), now)
			return nil, nil
		}); err != nil {
			t.Fatalf("arm %s: %v", activityID, err)
		}
	}
	if len(committer.records) != 2 {
		t.Fatalf("%d records, want 2", len(committer.records))
	}
	for index, record := range committer.records {
		for _, mutation := range record.Mutations {
			raw := mutation.Data
			if len(raw) == 0 {
				raw = mutation.Patch.SetBSON
			}
			var doc bson.M
			if err := bson.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			if _, leaked := doc["timer_next_due"]; leaked {
				t.Fatalf("record %d carries the non-persistent timer_next_due: %v", index, doc)
			}
		}
	}
}

// worldGetter hands Nest the one World under test.
type worldGetter struct{ subject *World }

func (g worldGetter) Get(_ context.Context, id int64, _ entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	if id != g.subject.ID() {
		return nil, nest.ErrEntityNotFound
	}
	return g.subject, nil
}

func (g worldGetter) GetMany(ctx context.Context, ids []int64, categories []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	out := make([]entity.IThreadSafeEntity, len(ids))
	for i, id := range ids {
		value, err := g.Get(ctx, id, categories[i])
		if err != nil {
			return nil, err
		}
		out[i] = value
	}
	return out, nil
}

// runWorldHandler runs body as one Nest handler on subject: admission,
// capture, rollback and commit are the engine's own.
func runWorldHandler(t *testing.T, subject *World, meta nest.HandlerMeta, committer nest.TransactionCommitter, body func(*World) error) error {
	t.Helper()
	engine := nest.NewEngine(
		nest.NestOptionWithGetter(worldGetter{subject: subject}),
		nest.NestOptionWithTransactionCommitter(committer),
		nest.NestOptionWithWorkerNumAndMsgCap(1, 1, 16),
	)
	name := nest.NewHandlerName("world_timer_rollback_probe")
	if err := engine.RegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		return nil, body(es[0].(*World))
	}, meta); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := engine.Start(); err != nil {
		t.Fatalf("start nest: %v", err)
	}
	defer func() { _ = engine.Shutdown(context.Background()) }()
	_, err := engine.Request(context.Background(), name, subject.ID(), nil)
	return err
}

// D-L1（维护者决定，2026-10-06）：同一期限的截止按 (期限, priority, 登记顺序) 触发。World 每次调用都从 DAO
// 的 map 重建调度器，map 的遍历顺序是随机的；修前同期限的顺序由 map 顺序与堆的形状决定，修后按登记顺序（节点 ID）。
// 触发顺序就是 effect 在提交记录里的顺序。
func TestDeadlinesDueAtTheSameMomentFireInArmOrder(t *testing.T) {
	subject := newWorld(t)
	now := time.Unix(1_800_000_000, 0)
	var armed []string
	for i := range 8 {
		activityID := "race-" + string(rune('a'+i))
		if _, err := nest.RunIsolatedTransaction(context.Background(), &worldCommitter{}, "arm", func() (any, error) {
			subject.TimerComp().ScheduleActivityPhase(activityID, now.Add(time.Minute), now)
			return nil, nil
		}); err != nil {
			t.Fatalf("arm %s: %v", activityID, err)
		}
		armed = append(armed, activityID)
	}
	committer := &worldCommitter{}
	if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "tick", func() (any, error) {
		subject.TimerComp().Tick(now.Add(time.Minute))
		return nil, nil
	}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(committer.records) != 1 {
		t.Fatalf("%d records, want 1", len(committer.records))
	}
	var fired []string
	for _, effect := range committer.records[0].Effects {
		fired = append(fired, effect.Key)
	}
	if !slices.Equal(fired, armed) {
		t.Fatalf("deadlines due at the same moment fired in order %v, want the order they were armed in %v", fired, armed)
	}
}

// D-L2（维护者决定，2026-10-06）：存储里有一种这个版本没有 handler 的定时器（例如下线了一种类型）。
// 加载时每种类型告警一次；到期时节点照旧删除，但每个被删的节点打一条 Warn 并计
// timer.unhandled_dropped_total{kind=<类型号>}。修前加载与删除都没有日志、没有计数。
func TestAStoredTimerOfATypeWithNoHandlerIsReportedAndCountedWhenDropped(t *testing.T) {
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	registry := metrics.NewRegistry()
	previousRegistry := metrics.DefaultRegistry()
	metrics.SetDefaultRegistry(registry)
	t.Cleanup(func() { metrics.SetDefaultRegistry(previousRegistry) })

	now := time.Unix(1_800_000_000, 0)
	first := newWorld(t)
	if _, err := nest.RunIsolatedTransaction(context.Background(), &worldCommitter{}, "store_unknown_types", func() (any, error) {
		for id, timerType := range map[int64]int32{1: 99, 2: 99, 3: 98} {
			stored := &db.TimerNode{}
			stored.SetType(timerType)
			stored.SetEndUnixMilli(now.Add(time.Minute).UnixMilli())
			stored.SetDelayMillis(time.Minute.Milliseconds())
			first.Dao().SetTimers(id, stored)
		}
		first.Dao().SetTimerSeed(3)
		return nil, nil
	}); err != nil {
		t.Fatalf("store: %v", err)
	}

	restarted := restartWorld(t, first)
	loaded := logs.String()
	if n := strings.Count(loaded, "level=WARN"); n != 2 {
		t.Errorf("loading stored timers of two unhandled types logged %d warnings, want one per type:\n%s", n, loaded)
	}
	if !strings.Contains(loaded, "type=99") || !strings.Contains(loaded, "type=98") {
		t.Errorf("the load warnings do not name the types:\n%s", loaded)
	}

	logs.Reset()
	committer := &worldCommitter{}
	if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "tick", func() (any, error) {
		if remaining := restarted.TimerComp().Tick(now.Add(time.Minute)); remaining != 0 {
			t.Errorf("%d nodes remain; a due node with no handler is still removed", remaining)
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 3 {
		t.Errorf("dropping three unhandled nodes logged %d warnings, want 3:\n%s", n, logs.String())
	}
	for kind, want := range map[string]int64{"99": 2, "98": 1} {
		got := int64(0)
		for _, metric := range registry.Snapshot() {
			if metric.Name == "timer.unhandled_dropped_total" && metric.Labels["kind"] == kind {
				got = metric.Value
			}
		}
		if got != want {
			t.Errorf("timer.unhandled_dropped_total{kind=%q} = %d, want %d", kind, got, want)
		}
	}
}

// D-L1：priority 随节点存储，重启后照样决定同期限的顺序；加字段之前存下的节点没有 priority 键，按 0 读回。
func TestTimerPriorityIsStoredAndOrdersAfterARestart(t *testing.T) {
	first := newWorld(t)
	now := time.Unix(1_800_000_000, 0)
	var late, plain, early int64
	if _, err := nest.RunIsolatedTransaction(context.Background(), &worldCommitter{}, "arm", func() (any, error) {
		scheduler := first.TimerComp().scheduler(now)
		late = scheduler.NewTimerWithPriority(time.Minute, TimerTypeActivityPhase, 5, 0, 0, []byte("race-late"))
		plain = scheduler.NewTimer(time.Minute, TimerTypeActivityPhase, 0, 0, []byte("race-plain"))
		early = scheduler.NewTimerWithPriority(time.Minute, TimerTypeActivityPhase, -1, 0, 0, []byte("race-early"))
		first.TimerComp().settle(scheduler)
		return nil, nil
	}); err != nil {
		t.Fatalf("arm: %v", err)
	}
	if stored, ok := first.Dao().GetTimers(late); !ok || stored.GetPriority() != 5 {
		t.Fatalf("stored node %d = %+v, want priority 5", late, stored)
	}

	// The stored document, as bson.D all the way down (the v2 driver's default
	// for nested documents), so it can be edited and stored back.
	var doc bson.D
	if err := bson.Unmarshal(first.Dao().Marshal(), &doc); err != nil {
		t.Fatal(err)
	}
	var timers bson.D
	for _, field := range doc {
		if field.Key == "timers" {
			timers, _ = field.Value.(bson.D)
		}
	}
	storedPriority := map[string]any{}
	for _, node := range timers {
		fields, _ := node.Value.(bson.D)
		for _, field := range fields {
			if field.Key == "priority" {
				storedPriority[node.Key] = field.Value
			}
		}
	}
	if got := storedPriority[strconv.FormatInt(early, 10)]; got != int32(-1) {
		t.Errorf("the stored priority of node %d is %v, want -1 (stored: %v)", early, got, storedPriority)
	}

	restarted := restartWorld(t, first)
	committer := &worldCommitter{}
	if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "tick", func() (any, error) {
		restarted.TimerComp().Tick(now.Add(time.Minute))
		return nil, nil
	}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	var fired []string
	for _, record := range committer.records {
		for _, effect := range record.Effects {
			fired = append(fired, effect.Key)
		}
	}
	if want := []string{"race-early", "race-plain", "race-late"}; !slices.Equal(fired, want) {
		t.Fatalf("after a restart the same-deadline nodes fired in order %v, want %v", fired, want)
	}

	// A document stored before the field existed: strip every priority key and
	// load it. The nodes come back at priority 0 and fire in the order armed.
	for i, node := range timers {
		fields, _ := node.Value.(bson.D)
		kept := bson.D{}
		for _, field := range fields {
			if field.Key != "priority" {
				kept = append(kept, field)
			}
		}
		timers[i].Value = kept
	}
	for i := range doc {
		if doc[i].Key == "timers" {
			doc[i].Value = timers
		}
	}
	legacy, err := bson.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	restored := db.NewWorldDao()
	if err := restored.RestorePersisted(legacy, db.WorldDaoSchemaVersion, 1); err != nil {
		t.Fatalf("restore a document without priority: %v", err)
	}
	for _, id := range []int64{late, plain, early} {
		if stored, ok := restored.GetTimers(id); !ok || stored.GetPriority() != 0 {
			t.Fatalf("legacy node %d = %+v, want it loaded with priority 0", id, stored)
		}
	}
}
