package nestwal_test

// RR-20260928-11：pipelined handler 在 broadcast（没有提前放锁的闭包）或带 Remote 批次时不走 Enqueue，回退到 strict 提交路径，
// 经 committer.Commit（kit 装配为 Projector.Commit，另有 nestwal.Committer.Commit）进入 WAL.Append，记录的 Durability 仍是 3。
// 修前 Append 只对 DurabilityStrict 置 requireSync：Durability 3 的记录与 async 一样在 fsync 之前返回，于是锁释放、Sync Confirm、
// AfterCommit 都可能先于 fsync 对外可见，违反 pipelined “成功只在持久之后对外可见”的契约（NEST_PIPELINED_COMMIT.md §5 写明这些路径
// “表现等同 Strict（锁内等待 durable）”）。承诺：
//   - WAL 层：Append 对 strict 与 pipelined 都在所在批 fsync 之后才返回；async 不等 fsync（不变）；Enqueue 仍在 fsync 之前交出票据（不变）。
//   - broadcast pipelined（真实 Nest + 正式 Projector + 真实 WAL）：fsync 完成之前实体锁不释放、Sync 门不放行、AfterCommit 不执行，与 strict 相同。
//   - 快路径（单目标 Request，Enqueue 两阶段）不变：fsync 还没完成时实体锁已经释放，回复、AfterCommit、Sync 门仍等到持久之后。
// 放在 nestwal 目录的外部测试包里：要用 RR-33 的段文件 fsync 测试缝（SetSyncFileForTest）把 fsync 停住，同时装配 Nest 与 engine.Projector。
// 修前没有 fsync 可停：用 GroupCommitInterval=1h 关掉后台刷盘，已完成的 fsync 次数在每个观察点仍为 0，即“对外可见早于 fsync”。

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	engine "github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
)

// fallbackKind 是本测试二进制里唯一的本地 kind：nestwal 目录下 remote_codec_test.go 已占用 125、197（Remote managed），不能撞号。
const fallbackKind entity.EntityKind = 198

// fsyncGate 包住段文件 fsync：arm 之后每次 fsync 先通知 entered、再停到 release。只在测试编译（RR-33 的 Options.syncFile 缝）。
// completed 是已完成的段文件 fsync 次数，免锁读取：写线程在 fsync 期间持有 stateMu，fsync 停住时 WAL.Stats() 会阻塞。
type fsyncGate struct {
	armed     atomic.Bool
	entered   chan struct{}
	proceed   chan struct{}
	released  sync.Once
	completed atomic.Uint64
}

func newFsyncGate() *fsyncGate {
	return &fsyncGate{entered: make(chan struct{}, 16), proceed: make(chan struct{})}
}

func (g *fsyncGate) sync(file *os.File) error {
	if g.armed.Load() {
		select {
		case g.entered <- struct{}{}:
		default:
		}
		<-g.proceed
	}
	err := file.Sync()
	g.completed.Add(1)
	return err
}

func (g *fsyncGate) release() { g.released.Do(func() { close(g.proceed) }) }

func openGatedWAL(t *testing.T, gate *fsyncGate) *nestwal.WAL {
	t.Helper()
	opts := nestwal.DefaultOptions(t.TempDir())
	opts.WriterVersion = nestwal.WriterVersionV2
	// 关掉后台组提交刷盘：只剩 requireSync 的批次会 fsync，Syncs 的变化只来自被测的提交。
	opts.GroupCommitInterval = time.Hour
	nestwal.SetSyncFileForTest(&opts, gate.sync)
	wal, err := nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	return wal
}

func fallbackRecord(sequence byte, durability coredata.Durability) coredata.CommitRecord {
	var id coredata.TransactionID
	id[15] = sequence
	return coredata.CommitRecord{
		ID: id, Handler: "rr11.handler", Durability: durability,
		Mutations: []coredata.Mutation{{
			Key:  coredata.DocumentKey{Database: "game", Resource: "players", ID: int64(sequence)},
			Kind: coredata.MutationPut, NextVersion: 1, Mask: 1, Schema: 1, Codec: "json", Data: []byte(`{}`),
		}},
	}
}

func waitGate(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// WAL 层：Append 的返回时机按 Durability 区分，Enqueue 不受影响。
func TestAppendWaitsForFsyncOnStrictPathForStrictAndPipelined(t *testing.T) {
	for i, tc := range []struct {
		durability coredata.Durability
		waitsSync  bool
	}{
		{corenest.DurabilityAsync, false},
		{corenest.DurabilityStrict, true},
		{corenest.DurabilityPipelined, true},
	} {
		t.Run(tc.durability.String(), func(t *testing.T) {
			gate := newFsyncGate()
			defer gate.release()
			wal := openGatedWAL(t, gate)
			defer func() { gate.release(); _ = wal.Close(context.Background()) }()
			gate.armed.Store(true)
			base := gate.completed.Load()
			returned := make(chan error, 1)
			go func() {
				_, err := wal.Append(context.Background(), fallbackRecord(byte(10+i), tc.durability))
				returned <- err
			}()
			if !tc.waitsSync {
				select {
				case err := <-returned:
					if err != nil {
						t.Fatal(err)
					}
				case <-gate.entered:
					t.Fatal("an async Append fsynced before returning; async only waits for the write")
				case <-time.After(5 * time.Second):
					t.Fatal("async Append never returned")
				}
				if got := gate.completed.Load() - base; got != 0 {
					t.Fatalf("async Append fsynced %d time(s), want 0", got)
				}
				return
			}
			select {
			case <-gate.entered:
			case err := <-returned:
				t.Fatalf("Append(Durability %s) returned (err=%v) before its batch was fsynced: syncs=+%d; a record on the strict commit path must be durable when Append returns",
					tc.durability, err, gate.completed.Load()-base)
			case <-time.After(5 * time.Second):
				t.Fatal("Append neither fsynced nor returned")
			}
			select {
			case err := <-returned:
				t.Fatalf("Append(Durability %s) returned (err=%v) while its fsync was still in progress", tc.durability, err)
			case <-time.After(20 * time.Millisecond):
			}
			gate.release()
			select {
			case err := <-returned:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Append never returned after the fsync completed")
			}
			if got := gate.completed.Load() - base; got != 1 {
				t.Fatalf("Append(Durability %s) fsynced %d time(s), want exactly 1", tc.durability, got)
			}
		})
	}

	// 快路径不变：Enqueue 在 fsync 之前交出票据，票据在 fsync 之后才完成。
	t.Run("enqueue", func(t *testing.T) {
		gate := newFsyncGate()
		defer gate.release()
		wal := openGatedWAL(t, gate)
		defer func() { gate.release(); _ = wal.Close(context.Background()) }()
		gate.armed.Store(true)
		ticket, err := wal.Enqueue(context.Background(), fallbackRecord(20, corenest.DurabilityPipelined))
		if err != nil {
			t.Fatal(err)
		}
		waitGate(t, gate.entered, "the Enqueue batch fsync")
		select {
		case <-ticket.Done():
			t.Fatal("pipelined ticket resolved while its fsync was still in progress")
		default:
		}
		gate.release()
		waitGate(t, ticket.Done(), "the pipelined ticket")
		if err := ticket.Err(); err != nil {
			t.Fatal(err)
		}
		if wal.DurableLSN() < ticket.LSN() {
			t.Fatalf("DurableLSN=%d < ticket LSN=%d after resolution", wal.DurableLSN(), ticket.LSN())
		}
	})
}

type fallbackDao struct {
	id      int64
	Tracker coredata.Tracker
	Value   int
}

func (d *fallbackDao) Id() int64                       { return d.id }
func (d *fallbackDao) SetId(id int64)                  { d.id = id }
func (d *fallbackDao) DbName() string                  { return "test" }
func (d *fallbackDao) CollName() string                { return "rr11_fallback" }
func (d *fallbackDao) Dirty() entity.IDirty            { return &d.Tracker }
func (d *fallbackDao) CleanDirty()                     { d.Tracker.SelfClean() }
func (d *fallbackDao) DirtyTracker() *coredata.Tracker { return &d.Tracker }

func (d *fallbackDao) marshal() []byte {
	raw, _ := json.Marshal(struct {
		ID    int64 `json:"id"`
		Value int   `json:"value"`
	}{d.id, d.Value})
	return raw
}

func (d *fallbackDao) CaptureRollbackState() ([]byte, error) { return d.marshal(), nil }
func (d *fallbackDao) RestoreRollbackState(raw []byte) error {
	var doc struct {
		Value int `json:"value"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	d.Value = doc.Value
	return nil
}

func (d *fallbackDao) PrepareMutation(change corenest.PersistChange) (coredata.Mutation, error) {
	version := d.Tracker.Version()
	return coredata.Mutation{
		Key:  coredata.DocumentKey{Database: "test", Resource: d.CollName(), ID: d.id},
		Kind: coredata.MutationPut, ExpectedVersion: version, NextVersion: version + 1,
		Mask: change.Mask, Schema: 1, Codec: "json", Data: d.marshal(),
	}, nil
}

func (d *fallbackDao) AcceptMutation(mutation coredata.Mutation) error {
	return d.Tracker.AcceptVersion(mutation.ExpectedVersion, mutation.NextVersion)
}

type fallbackEntity struct {
	*entity.EntityBase
	dao *fallbackDao
}

func (e *fallbackEntity) Base() *entity.EntityBase { return e.EntityBase }
func (e *fallbackEntity) RangeDao(f func(entity.DaoInterface)) {
	if f != nil {
		f(e.dao)
	}
}

type discardProjectionStore struct{}

func (discardProjectionStore) Project(context.Context, coredata.CommitRecord) error { return nil }

// fallbackFixture：真实 Nest（1 快 worker）+ 正式 engine.Projector（kit 装配的 committer，ManualReplay 不回放、不 Ack，
// 不产生额外 fsync）+ 真实 WAL（fsync 可停）+ entitysync Manager（Sync 门）。
type fallbackFixture struct {
	gate        *fsyncGate
	wal         *nestwal.WAL
	mgr         *corenest.NestMgr
	live        *fallbackEntity
	name        corenest.HandlerName
	base        atomic.Uint64
	started     chan struct{}
	afterCommit chan uint64 // AfterCommit 运行时 WAL 已完成的 fsync 次数（相对 base）
}

func (f *fallbackFixture) syncs() uint64 { return f.gate.completed.Load() - f.base.Load() }

func newFallbackFixture(t *testing.T, unique int64, handler string, meta corenest.HandlerMeta) *fallbackFixture {
	t.Helper()
	f := &fallbackFixture{gate: newFsyncGate(), started: make(chan struct{}, 1), afterCommit: make(chan uint64, 4)}
	t.Cleanup(f.gate.release)
	f.wal = openGatedWAL(t, f.gate)
	projector, err := engine.NewProjector(f.wal, discardProjectionStore{}, engine.ProjectorOptions{CloseWAL: true, ManualReplay: true})
	if err != nil {
		_ = f.wal.Close(context.Background())
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.gate.release()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = projector.Close(ctx)
	})

	entity.MustRegisterEntityKindCategory(fallbackKind, entity.EntityCategoryWorld)
	id, err := entity.BuildEntityID(unique, fallbackKind)
	if err != nil {
		t.Fatal(err)
	}
	manager := entity.NewEntityManager()
	f.live = &fallbackEntity{EntityBase: entity.NewEntityBase(id, entity.EntityCategoryWorld, false, fallbackKind), dao: &fallbackDao{id: id, Value: 1}}
	if err := manager.TryAdd(f.live); err != nil {
		t.Fatal(err)
	}
	syncMgr, err := entitysync.NewManager(entitysync.ManagerConfig{Mode: entitysync.ModePeriodic, Interval: time.Hour,
		Transport: entitysync.TransportFunc(func(context.Context, entitysync.SessionID, []byte) error { return nil })})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syncMgr.Close(context.Background()) })
	packed := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		return entity.CopyFrozenSyncPayload(1, []byte("rr11")), nil
	}
	f.live.EnableSync(entity.EntitySyncCreateParam{Enabled: true, EntityID: id, Namespace: "test",
		Packer: entity.SubjectSyncPackFunc{Snapshot: packed, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return packed(p) }}})
	if err := syncMgr.Register(f.live.Sync()); err != nil {
		t.Fatal(err)
	}

	f.mgr = corenest.NewEngine(corenest.NestOptionWithGetter(entity.NewManagerAccess(manager)), corenest.NestOptionWithTransactionCommitter(projector),
		corenest.NestOptionWithWorkerNumAndMsgCap(1, 1, 16), corenest.NestOptionWithEntitySync(syncMgr))
	f.name = corenest.NewHandlerName(handler)
	f.mgr.MustRegisterHandlerWithMeta(f.name, func(es []entity.IThreadSafeEntity, _ []any, _ ...corenest.HandlerOption) (any, error) {
		e := es[0].(*fallbackEntity)
		old := e.dao.Value
		corenest.RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil })
		e.dao.Value++
		e.MarkSyncDirty(1)
		corenest.AfterCommit(func() { f.afterCommit <- f.syncs() })
		select {
		case f.started <- struct{}{}:
		default:
		}
		return "ok", corenest.MarkPersist(e.dao, 1)
	}, meta)
	if err := f.mgr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.gate.release()
		_ = f.mgr.Shutdown(context.Background())
	})
	return f
}

// visibility 是 handler 开始之后第一次观察到“实体锁可取得”“Sync 门放行”时 WAL 已完成的 fsync 次数（相对 base）。
type visibility struct {
	lockFree, syncReady           bool
	lockFreeSyncs, syncReadySyncs uint64
}

// watch 在 handler 开始之后轮询实体锁与 Sync 门：Sync 门在事务开始时（BeginSyncMutation）已换成本事务的门，放行要等全部锁释放且 Confirm。
func (f *fallbackFixture) watch() <-chan visibility {
	done := make(chan visibility, 1)
	go func() {
		var v visibility
		select {
		case <-f.started:
		case <-time.After(5 * time.Second):
			done <- v
			return
		}
		mu := f.live.GetMutex()
		deadline := time.Now().Add(5 * time.Second)
		for !(v.lockFree && v.syncReady) && time.Now().Before(deadline) {
			if !v.lockFree && mu.TryLock() {
				v.lockFree, v.lockFreeSyncs = true, f.syncs()
				mu.Unlock()
			}
			if !v.syncReady && f.live.Sync().SyncCommitReady() {
				v.syncReady, v.syncReadySyncs = true, f.syncs()
			}
			time.Sleep(20 * time.Microsecond)
		}
		done <- v
	}()
	return done
}

// broadcast 上 pipelined 与 strict 一样锁内等 durable：fsync 完成之前锁不释放、Sync 门不放行、AfterCommit 不执行。
func TestBroadcastPipelinedIsInvisibleUntilFsync(t *testing.T) {
	for i, meta := range []corenest.HandlerMeta{
		{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityStrict},
		{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityPipelined},
	} {
		t.Run(meta.Durability.String(), func(t *testing.T) {
			f := newFallbackFixture(t, 53000+int64(i), "rr11_broadcast_"+meta.Durability.String(), meta)
			f.gate.armed.Store(true)
			f.base.Store(f.gate.completed.Load())
			seen := f.watch()
			if err := f.mgr.DispatchBroadcast(context.Background(), f.name, []int64{f.live.GUId()}, nil); err != nil {
				t.Fatal(err)
			}
			select {
			case <-f.gate.entered:
				// fsync 进行中：worker 仍持锁，提交后的一切都还不可见。
				if f.live.GetMutex().TryLock() {
					f.live.GetMutex().Unlock()
					t.Errorf("broadcast %s: entity lock was released while the commit fsync was in progress", meta.Durability)
				}
				if f.live.Sync().SyncCommitReady() {
					t.Errorf("broadcast %s: Sync gate released while the commit fsync was in progress", meta.Durability)
				}
				select {
				case n := <-f.afterCommit:
					t.Errorf("broadcast %s: AfterCommit ran (syncs=+%d) while the commit fsync was in progress", meta.Durability, n)
				default:
				}
				f.gate.release()
			case v := <-seen:
				ac := "not run"
				select {
				case n := <-f.afterCommit:
					ac = "ran at syncs=+" + strconv.FormatUint(n, 10)
				case <-time.After(time.Second):
				}
				t.Fatalf("broadcast %s became externally visible before its WAL record was fsynced: lock free=%v at syncs=+%d, Sync gate released=%v at syncs=+%d, AfterCommit %s; the commit fsync never ran",
					meta.Durability, v.lockFree, v.lockFreeSyncs, v.syncReady, v.syncReadySyncs, ac)
			case <-time.After(5 * time.Second):
				t.Fatal("broadcast neither fsynced nor became visible")
			}
			v := <-seen
			if !v.lockFree || !v.syncReady || v.lockFreeSyncs < 1 || v.syncReadySyncs < 1 {
				t.Fatalf("broadcast %s after fsync: lock free=%v at syncs=+%d, Sync gate released=%v at syncs=+%d; want both, each after the fsync",
					meta.Durability, v.lockFree, v.lockFreeSyncs, v.syncReady, v.syncReadySyncs)
			}
			select {
			case n := <-f.afterCommit:
				if n < 1 {
					t.Fatalf("broadcast %s: AfterCommit ran at syncs=+%d, before the fsync", meta.Durability, n)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("AfterCommit never ran")
			}
			if f.live.dao.Value != 2 {
				t.Fatalf("value=%d, want 2 (committed)", f.live.dao.Value)
			}
		})
	}
}

// 快路径不变（NEST_PIPELINED_COMMIT.md §5）：Enqueue 之后、fsync 完成之前实体锁已释放；回复、AfterCommit、Sync 门等到持久之后。
func TestPipelinedFastPathStillReleasesLockBeforeFsync(t *testing.T) {
	f := newFallbackFixture(t, 53010, "rr11_fast_path", corenest.HandlerMeta{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityPipelined})
	f.gate.armed.Store(true)
	f.base.Store(f.gate.completed.Load())
	type reply struct {
		ret   any
		err   error
		syncs uint64
	}
	replied := make(chan reply, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ret, err := f.mgr.Request(ctx, f.name, f.live.GUId(), nil)
		replied <- reply{ret, err, f.syncs()}
	}()
	waitGate(t, f.gate.entered, "the pipelined batch fsync")
	mu := f.live.GetMutex()
	deadline := time.Now().Add(5 * time.Second)
	for !mu.TryLock() {
		if time.Now().After(deadline) {
			t.Fatal("pipelined fast path held the entity lock across the fsync (early release lost)")
		}
		time.Sleep(20 * time.Microsecond)
	}
	mu.Unlock()
	if n := f.syncs(); n != 0 {
		t.Fatalf("premise: fsync already completed (syncs=+%d) when the released lock was observed", n)
	}
	if f.live.Sync().SyncCommitReady() {
		t.Fatal("pipelined fast path released the Sync gate before durability")
	}
	select {
	case r := <-replied:
		t.Fatalf("pipelined fast path replied (%v, %v) before durability", r.ret, r.err)
	case n := <-f.afterCommit:
		t.Fatalf("pipelined fast path ran AfterCommit (syncs=+%d) before durability", n)
	default:
	}
	f.gate.release()
	r := <-replied
	if r.err != nil || r.ret != "ok" || r.syncs < 1 {
		t.Fatalf("reply=%v err=%v at syncs=+%d, want ok after the fsync", r.ret, r.err, r.syncs)
	}
	select {
	case n := <-f.afterCommit:
		if n < 1 {
			t.Fatalf("AfterCommit ran at syncs=+%d, before the fsync", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AfterCommit never ran")
	}
	deadline = time.Now().Add(5 * time.Second)
	for !f.live.Sync().SyncCommitReady() {
		if time.Now().After(deadline) {
			t.Fatal("Sync gate never released after durability")
		}
		time.Sleep(time.Millisecond)
	}
	if got := f.live.Base().LastCommitLSN(); got == 0 {
		t.Fatal("fast path did not record the commit LSN (Enqueue was not used)")
	}
}
