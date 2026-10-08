package nest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20261006-41（框架文档发现 F02-1 / F03-1，维护者 2026-10-07 选 A）：durability=memory 的事务不持久化任何东西，
// 契约是“memory handler 不能改持久字段”。承诺：rollback=state|undo 加 durability=memory 的 handler 改了持久字段时，
// 整笔事务失败（errors.Is ErrMemoryTransactionPersistentWrite，错误点名实体与字段），按事务的回滚策略撤销内存修改；
// 只改非持久字段的 memory 事务照常成功。
// 旧行为：durableCommit 对“memory 且无 effect”直接返回 nil——回复成功，committer 与 PrepareMutation 都调用 0 次，
// 内存里是新值、库里是旧值，实体重新加载后回到旧值（静默丢数据）。rollback=none 的 memory 快路径没有事务，持久 setter
// 照旧以 ErrTransactionClosed panic，不在本用例范围（见 TestMemoryFastPathPersistentSetterStillPanics）。

// memoryWriteDao 有一个持久字段 Value（bit 1）和一个非持久字段 Scratch（只标同步脏位，不进 MarkPersist）。
type memoryWriteDao struct {
	id       int64
	tracker  dataengine.Tracker
	Value    int
	Scratch  int
	prepares int
}

func (d *memoryWriteDao) Id() int64                         { return d.id }
func (d *memoryWriteDao) SetId(id int64)                    { d.id = id }
func (d *memoryWriteDao) DbName() string                    { return "game" }
func (d *memoryWriteDao) CollName() string                  { return "memory_write" }
func (d *memoryWriteDao) Dirty() entity.IDirty              { return &d.tracker }
func (d *memoryWriteDao) CleanDirty()                       { d.tracker.SelfClean() }
func (d *memoryWriteDao) DirtyTracker() *dataengine.Tracker { return &d.tracker }

// PersistFieldNames 与生成 DAO 同形：按位给出持久字段的存储名。
func (d *memoryWriteDao) PersistFieldNames(mask uint64) []string {
	if mask&1 != 0 {
		return []string{"value"}
	}
	return nil
}

func (d *memoryWriteDao) persisted() []byte {
	raw, _ := json.Marshal(map[string]int{"value": d.Value})
	return raw
}

func (d *memoryWriteDao) CaptureRollbackState() ([]byte, error) {
	return json.Marshal([2]int{d.Value, d.Scratch})
}

func (d *memoryWriteDao) RestoreRollbackState(raw []byte) error {
	var state [2]int
	if err := json.Unmarshal(raw, &state); err != nil {
		return err
	}
	d.Value, d.Scratch = state[0], state[1]
	return nil
}

func (d *memoryWriteDao) PrepareMutation(change PersistChange) (dataengine.Mutation, error) {
	d.prepares++
	version := d.tracker.Version()
	return dataengine.Mutation{
		Key:  dataengine.DocumentKey{Database: "game", Resource: d.CollName(), ID: d.id},
		Kind: dataengine.MutationPut, ExpectedVersion: version, NextVersion: version + 1,
		Mask: change.Mask, Schema: 1, Codec: "json", Data: d.persisted(),
	}, nil
}

func (d *memoryWriteDao) AcceptMutation(mutation dataengine.Mutation) error {
	return d.tracker.AcceptVersion(mutation.ExpectedVersion, mutation.NextVersion)
}

// setValue / setScratch 模拟生成 setter：持久字段登记 MarkPersist，非持久字段只标同步脏位。
func (d *memoryWriteDao) setValue(v int) {
	if err := MarkPersist(d, 1); err != nil {
		panic(err)
	}
	d.Value = v
	d.tracker.MarkSync(1)
}

func (d *memoryWriteDao) setScratch(v int) {
	d.Scratch = v
	d.tracker.MarkSync(2)
}

type memoryWriteEntity struct {
	*entity.EntityBase
	dao *memoryWriteDao
}

func (e *memoryWriteEntity) Base() *entity.EntityBase { return e.EntityBase }
func (e *memoryWriteEntity) RangeDao(f func(entity.DaoInterface)) {
	if f != nil {
		f(e.dao)
	}
}

// storeCommitter 把记录里的 Put 写进一个“库”，用来验证重新加载后的值。
type storeCommitter struct {
	mu      sync.Mutex
	commits int
	store   map[int64][]byte
}

func (c *storeCommitter) Commit(_ context.Context, record CommitRecord) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.commits++
	for _, m := range record.Mutations {
		c.store[m.Key.ID] = append([]byte(nil), m.Data...)
	}
	return nil
}

func (c *storeCommitter) reload(id int64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	var doc map[string]int
	_ = json.Unmarshal(c.store[id], &doc)
	return doc["value"]
}

func (c *storeCommitter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.commits
}

func newMemoryWriteEngine(t *testing.T, unique int64) (*NestMgr, int64, *memoryWriteDao, *storeCommitter) {
	t.Helper()
	getter := newMockGetter()
	id := mustBuildCastID(t, unique, 1, nestLocalKind)
	dao := &memoryWriteDao{id: id, Value: 10}
	dao.tracker.SetVersion(1)
	getter.Add(&memoryWriteEntity{EntityBase: entity.NewEntityBase(id, 1, false, nestLocalKind), dao: dao})
	committer := &storeCommitter{store: map[int64][]byte{id: dao.persisted()}}
	mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerNumAndMsgCap(1, 16))
	return mgr, id, dao, committer
}

func TestMemoryTransactionPersistentWriteFailsAndRollsBack(t *testing.T) {
	for _, policy := range []RollbackPolicy{RollbackState, RollbackUndo} {
		t.Run(policy.String(), func(t *testing.T) {
			mgr, id, dao, committer := newMemoryWriteEngine(t, 41100+int64(policy))
			name := NewHandlerName("rr20261006_41_memory_persistent_" + policy.String())
			mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				current := es[0].(*memoryWriteEntity).dao
				if policy == RollbackUndo {
					old := current.Value
					if !RecordUndo(current, 1, func() error { current.Value = old; return nil }) {
						return nil, errors.New("missing undo transaction")
					}
				}
				current.setValue(20)
				return "ok", nil
			}, HandlerMeta{Rollback: policy, Durability: DurabilityMemory})
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()

			ret, err := mgr.Request(context.Background(), name, id, nil)
			reloaded := committer.reload(id)
			t.Logf("ret=%v err=%v committer=%d PrepareMutation=%d memory=%d reloaded=%d", ret, err, committer.count(), dao.prepares, dao.Value, reloaded)
			if !errors.Is(err, ErrMemoryTransactionPersistentWrite) {
				t.Fatalf("err=%v, want errors.Is ErrMemoryTransactionPersistentWrite: a memory transaction changed a persistent field and the change reached no commit record (memory=%d, reloaded=%d)", err, dao.Value, reloaded)
			}
			if !errors.Is(err, ErrCommitRejected) {
				t.Fatalf("err=%v, want ErrCommitRejected (nothing was committed, the transaction rolled back)", err)
			}
			for _, want := range []string{"memory_write", "value"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("err=%v does not name %q", err, want)
				}
			}
			if dao.Value != 10 || reloaded != 10 {
				t.Fatalf("memory=%d reloaded=%d, want both 10 (rolled back, nothing persisted)", dao.Value, reloaded)
			}
			if committer.count() != 0 || dao.prepares != 0 {
				t.Fatalf("committer=%d PrepareMutation=%d, want 0/0", committer.count(), dao.prepares)
			}
		})
	}
}

func TestMemoryTransactionNonPersistentWriteSucceeds(t *testing.T) {
	for _, policy := range []RollbackPolicy{RollbackState, RollbackUndo} {
		t.Run(policy.String(), func(t *testing.T) {
			mgr, id, dao, committer := newMemoryWriteEngine(t, 41110+int64(policy))
			name := NewHandlerName("rr20261006_41_memory_scratch_" + policy.String())
			mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				es[0].(*memoryWriteEntity).dao.setScratch(5)
				return "ok", nil
			}, HandlerMeta{Rollback: policy, Durability: DurabilityMemory})
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()

			ret, err := mgr.Request(context.Background(), name, id, nil)
			if err != nil || ret != "ok" {
				t.Fatalf("ret=%v err=%v, want ok/nil for a memory transaction that only changes non-persistent fields", ret, err)
			}
			if dao.Scratch != 5 || dao.Value != 10 || committer.count() != 0 || dao.prepares != 0 {
				t.Fatalf("scratch=%d value=%d committer=%d prepares=%d", dao.Scratch, dao.Value, committer.count(), dao.prepares)
			}
		})
	}
}

// rollback=none 的 memory 快路径没有 RollbackTx：持久 setter 在 MarkPersist 处以 ErrTransactionClosed panic，行为不变。
func TestMemoryFastPathPersistentSetterStillPanics(t *testing.T) {
	dao := &memoryWriteDao{id: 1, Value: 10}
	defer func() {
		r := recover()
		err, _ := r.(error)
		if !errors.Is(err, ErrTransactionClosed) {
			t.Fatalf("recover=%v, want a panic with ErrTransactionClosed", r)
		}
	}()
	dao.setValue(20)
}

// handler 内新建实体按同一规则：新实体的持久字段要靠首个 Put 落库，memory 事务不写记录，所以新建并改了持久字段时整笔拒绝，
// 新实体随回滚撤销（CaptureCreatedEntity 登记的 revoke）；只新建、不改持久字段时照常成功（新实体只在内存，与修复前相同）。
func TestMemoryTransactionCreatedEntityPersistentWriteFails(t *testing.T) {
	for i, entry := range lifecycleEntries {
		t.Run(entry, func(t *testing.T) {
			unique := int64(41200 + 10*i)
			f := newCreateInScopeFixture(t, unique, nil)
			committer := &recordingCommitter{}
			mgr := NewEngine(NestOptionWithGetter(f.access), NestOptionWithTransactionCommitter(committer), NestOptionWithEntitySync(f.sync), NestOptionWithWorkerNumAndMsgCap(1, 16))
			name := NewHandlerName("rr20261006_41_memory_create_" + entry)
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				if _, err := f.createViaLifecycle(entry, unique+1, 5); err != nil {
					return nil, err
				}
				return "ok", nil
			}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityMemory})
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer mgr.Shutdown(context.Background())
			ret, err := mgr.Request(context.Background(), name, f.existID, nil)
			t.Logf("ret=%v err=%v published=%v existing=%d", ret, err, f.manager.Get(f.createdID) != nil, f.existing.dao.Value)
			if !errors.Is(err, ErrMemoryTransactionPersistentWrite) || !errors.Is(err, ErrCommitRejected) {
				t.Fatalf("err=%v, want ErrMemoryTransactionPersistentWrite and ErrCommitRejected", err)
			}
			if f.manager.Get(f.createdID) != nil {
				t.Fatal("entity created inside a rejected memory transaction is still published")
			}
			if f.existing.dao.Value == 11 {
				t.Fatal("existing entity's in-memory change was not rolled back")
			}
		})
	}
}

// 带 Remote 批次的 memory handler（rollback=none 也有 RollbackTx）：Remote 实体的 DAO 改动由批次认领、随批次提交；本地实体的
// 持久字段改动没有任何去处，同样整笔拒绝——Remote 批次 Abort、不 Commit，回复带 ErrMemoryTransactionPersistentWrite 与
// ErrCommitRejected。rollback=none 不撤销内存修改（与 RR-20260927-32、RR-20260930-12 同）。
// 旧行为：回复成功，Remote 批次照常 Commit，本地持久字段改动静默丢失。
func TestMemoryRemoteBatchLocalPersistentWriteRefused(t *testing.T) {
	localID, e := newAsyncPilotEntity(t, 41300, 10)
	getter := newMockGetter()
	remoteID := mustBuildCastID(t, 41301, entity.EntityCategoryRemote, nestRemoteManagedKind)
	getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
	getter.Add(e)
	recording := &recordingRemoteBatch{}
	manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return recording, nil }}
	committer := &recordsCommitter{}
	mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerNumAndMsgCap(1, 16))
	name := NewHandlerName("rr20261006_41_memory_remote_local_persist")
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		e.dao.Value = 77
		if err := MarkPersist(e.dao, 1); err != nil {
			return nil, err
		}
		return "ok", nil
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	msg, ch := GenSyncMsg(MsgTypeMulti)
	msg.Name, msg.Tids, msg.Cost, msg.HasRemote = name.String(), []int64{remoteID, localID}, true, true
	if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
		t.Fatal(err)
	}
	reply := stagedWait(t, ch)
	err, _ := reply.(error)
	t.Logf("reply=%v committer=%d remote commits=%d aborts=%d value=%d", reply, committer.count(), recording.commits.Load(), recording.aborts.Load(), e.dao.Value)
	if !errors.Is(err, ErrMemoryTransactionPersistentWrite) || !errors.Is(err, ErrCommitRejected) {
		t.Fatalf("reply=%v, want ErrMemoryTransactionPersistentWrite and ErrCommitRejected", reply)
	}
	if c := recording.commits.Load(); c != 0 {
		t.Fatalf("Remote batch committed %d time(s) although the local part was refused", c)
	}
	if recording.aborts.Load() == 0 {
		t.Fatal("Remote batch was not aborted")
	}
	if committer.count() != 0 {
		t.Fatalf("committer received %d record(s)", committer.count())
	}
}

// 同一提交点的其他本地持久内容：AddMutation 加的本地 mutation、AddReceipt 绑定的 receipt 同样被拒；Remote 批次 finalize 加的
// Remote mutation（随批次提交）、Remote 批次认领的 DAO 改动不算。没有任何登记的 memory 事务照常返回 nil。
func TestMemoryTransactionOtherLocalPersistenceRefused(t *testing.T) {
	newTx := func() *RollbackTx {
		tx := NewRollbackTx(RollbackState)
		tx.durability = DurabilityMemory
		tx.handler = "rr20261006_41_direct"
		return tx
	}
	if err := newTx().durableCommit(context.Background(), nil); err != nil {
		t.Fatalf("empty memory transaction: %v", err)
	}

	raw := newTx()
	if err := raw.AddMutation(EntityMutation{Key: dataengine.DocumentKey{ID: 9, Database: "test", Resource: "raw_res"}, Kind: dataengine.MutationPut, ExpectedVersion: 0, NextVersion: 1, Data: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	if err := raw.durableCommit(context.Background(), nil); !errors.Is(err, ErrMemoryTransactionPersistentWrite) || !strings.Contains(err.Error(), "raw_res/9") {
		t.Fatalf("local AddMutation: err=%v", err)
	}

	receipt := newTx()
	if err := receipt.AddReceipt(dataengine.Receipt{Namespace: "ns", ID: "r1", Digest: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	if err := receipt.durableCommit(context.Background(), nil); !errors.Is(err, ErrMemoryTransactionPersistentWrite) || !strings.Contains(err.Error(), "receipt ns/r1") {
		t.Fatalf("receipt: err=%v", err)
	}

	remote := newTx()
	if err := remote.AddMutation(EntityMutation{Key: dataengine.DocumentKey{ID: 9, Resource: "remote_entity"}, Kind: dataengine.MutationPut, ExpectedVersion: 0, NextVersion: 1, Codec: "remote", Remote: &entity.RemoteCommit{
		TransactionID: entity.RemoteTransactionID{1}, EntityID: 9, Kind: 1, NextVersion: 1, MarkerEpoch: 1, RouteEpoch: 1,
		Mutations: []entity.RemoteDataMutation{{Database: "test", Collection: "remote", ID: 9, Version: 1, Data: []byte{1}}},
	}}); err != nil {
		t.Fatal(err)
	}
	claimed := &memoryWriteDao{id: 5}
	if err := remote.MarkPersist(claimed, 1); err != nil {
		t.Fatal(err)
	}
	if _, ok := remote.RemotePersistChangeFor(claimed); !ok {
		t.Fatal("remote batch did not claim the DAO change")
	}
	if err := remote.durableCommit(context.Background(), nil); err != nil {
		t.Fatalf("remote mutation and remote-claimed DAO change must ride the batch: %v", err)
	}
}
