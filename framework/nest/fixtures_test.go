// 包内测试共用的实体、Getter、DAO、committer 替身与测试专用引擎入口。

package nest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// Tests exercise dispatch internals through an explicitly constructed engine;
// these names exist only in test builds and are not part of the public API.
var Nest *NestMgr

func InitNest(opts ...NestOption) {
	Nest = NewEngine(opts...)
	if err := Nest.Start(); err != nil {
		panic(err)
	}
}

// stopNestTimeout 是 StopNest 等引擎停下的上限。Shutdown 要排空已接受的事务，而事务可能在等测试提交器上的票据；
// 用例在落定票据之前失败退出（Fatal 在 defer StopNest 之前返回）时，原来的无限等待会把整个包挂到 go test -timeout
// （2026-11-18 v1.55.0 pretag 挂了 47 分钟）。
const stopNestTimeout = 20 * time.Second

// errStopNestCleanup 是 StopNest 超时后替用例落定未决票据时用的结果。
var errStopNestCleanup = errors.New("test cleanup: StopNest resolved a ticket the test left pending")

// stopNestStuck 记录 StopNest 等不到引擎停下的次数；TestMain 据此让整个包失败，避免“用例通过、关停卡住”被替用例落定票据后掩盖。
var stopNestStuck atomic.Int64

// StopNest 关停测试引擎。等 stopNestTimeout 还停不下时，落定本包测试提交器上所有未决票据（errStopNestCleanup）再等一次，
// 仍停不下就 panic：任何情况下都不会无限等待。
func StopNest() {
	if Nest == nil {
		return
	}
	mgr := Nest
	defer func() {
		Nest = nil
		forgetPipelinedTestCommitters()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), stopNestTimeout)
	err := mgr.Shutdown(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		return
	}
	stopNestStuck.Add(1)
	resolved := resolvePipelinedTestCommitters(errStopNestCleanup)
	fmt.Fprintf(os.Stderr, "StopNest: shutdown did not finish within %s; resolved %d pending test commit tickets with %q\n", stopNestTimeout, resolved, errStopNestCleanup)
	ctx, cancel = context.WithTimeout(context.Background(), stopNestTimeout)
	defer cancel()
	if err := mgr.Shutdown(ctx); errors.Is(err, context.DeadlineExceeded) {
		panic(fmt.Sprintf("StopNest: shutdown still not finished %s after resolving %d pending test commit tickets", stopNestTimeout, resolved))
	}
}

// TestMain：任何一次 StopNest 超时都让本包失败（哪怕触发它的用例自己报了通过）。
func TestMain(m *testing.M) {
	code := m.Run()
	if n := stopNestStuck.Load(); n > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: StopNest timed out %d time(s): a test left commit tickets pending or shutdown is stuck (see the StopNest lines above)\n", n)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// 旧动态 Cast 回归继续覆盖 Guard 的锁序、销毁竞态及回滚捕获防御。
// 这些用例刻意制造如今正式准入不可达的争锁交错，因此在测试中补声明后调用真实 Cast。
// 本适配器不代表合法的生产改目标方式、不验证 tail；正式拒绝及共享 tail 由 await_test 覆盖。
// 所有修改只作用于当前测试 goroutine 的 Msg，调用结束恢复，不改变生产准入实现。
func declareGuardFixtureTargets(ids ...int64) func() {
	msg := currentNestDispatchMsg()
	if msg == nil {
		return func() {}
	}
	old := msg.Tids
	msg.Tids = append(append([]int64(nil), old...), ids...)
	return func() { msg.Tids = old }
}
func guardFixtureCastOne[E entity.IThreadSafeEntity](id int64) (E, error) {
	defer declareGuardFixtureTargets(id)()
	return CastOne[E](id)
}
func guardFixtureCastTargetOne[E entity.IThreadSafeEntity](target CastTarget) (E, error) {
	defer declareGuardFixtureTargets(target.ID)()
	return CastTargetOne[E](target)
}
func guardFixtureCastMulti(targets ...CastTarget) ([]entity.IThreadSafeEntity, error) {
	ids := make([]int64, len(targets))
	for i, target := range targets {
		ids[i] = target.ID
	}
	defer declareGuardFixtureTargets(ids...)()
	return CastMulti(targets...)
}
func guardFixtureCastTwo[A, B entity.IThreadSafeEntity](a, b CastTarget) (A, B, error) {
	defer declareGuardFixtureTargets(a.ID, b.ID)()
	return CastTwo[A, B](a, b)
}
func guardFixtureCastThree[A, B, C entity.IThreadSafeEntity](a, b, c CastTarget) (A, B, C, error) {
	defer declareGuardFixtureTargets(a.ID, b.ID, c.ID)()
	return CastThree[A, B, C](a, b, c)
}

// GuardFixtureCastOneForTest 仅导出给 nest_test 的真实 WAL 防御回归，不进入生产构建。
func GuardFixtureCastOneForTest[E entity.IThreadSafeEntity](id int64) (E, error) {
	return guardFixtureCastOne[E](id)
}

const (
	nestLocalKind         entity.EntityKind = 248
	nestRemoteCapableKind entity.EntityKind = 250
	nestRemoteManagedKind entity.EntityKind = 251
	nestUnknownKind       entity.EntityKind = 249
)

func init() {
	// Remote-capable but not managed. RemotePolicyCapable used to say this;
	// Mirror says it now, and says something real besides: a local read-only
	// replica of state another service owns (M-04).
	entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
		Category:     entity.EntityCategoryRemote,
		Kind:         nestRemoteCapableKind,
		RemotePolicy: entity.RemotePolicyMirror,
		NoPersist:    true,
		Builder:      func(*entity.EntityCreateParam) (entity.IThreadSafeEntity, error) { return nil, nil },
	})
	entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
		Category:     entity.EntityCategoryRemote,
		Kind:         nestRemoteManagedKind,
		RemotePolicy: entity.RemotePolicyManaged,
		NoPersist:    true,
		Builder:      func(*entity.EntityCreateParam) (entity.IThreadSafeEntity, error) { return nil, nil },
		LoadPriority: 0,
		DaoBuilders:  nil,
		Lifetime:     entity.EntityLifetimeRemoteManaged,
		Sync:         entity.EntitySyncBuilderParam{},
	})
}

// mockEntity for testing
type mockEntity struct {
	*entity.EntityBase
}

func (m *mockEntity) Base() *entity.EntityBase { return m.EntityBase }

func newMockEntity(id int64, typo entity.EntityCategory) *mockEntity {
	e := &mockEntity{}
	e.EntityBase = entity.NewEntityBase(id, typo, false)
	return e
}

// mockGetter implements entity.Getter
type mockGetter struct {
	mu       sync.RWMutex
	entities map[int64]entity.IThreadSafeEntity
	groups   *entity.EntityManager
}

type rollbackTestDao struct {
	id      int64
	Tracker dataengine.Tracker
	Value   int
}

func (d *rollbackTestDao) Id() int64 { return d.id }

func (d *rollbackTestDao) SetId(id int64) { d.id = id }

func (d *rollbackTestDao) DbName() string { return "test" }

func (d *rollbackTestDao) CollName() string { return "rollback_test" }

func (d *rollbackTestDao) Dirty() entity.IDirty { return &d.Tracker }

func (d *rollbackTestDao) CleanDirty() { d.Tracker.SelfClean() }

func (d *rollbackTestDao) DirtyTracker() *dataengine.Tracker {
	return &d.Tracker
}

func (d *rollbackTestDao) Marshal() []byte {
	raw, _ := json.Marshal(struct {
		ID    int64 `json:"id"`
		Value int   `json:"value"`
	}{ID: d.id, Value: d.Value})
	return raw
}

func (d *rollbackTestDao) Unmarshal(raw []byte) error {
	var doc struct {
		ID    int64 `json:"id"`
		Value int   `json:"value"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	d.id = doc.ID
	d.Value = doc.Value
	return nil
}

func (d *rollbackTestDao) CaptureRollbackState() ([]byte, error) {
	return append([]byte(nil), d.Marshal()...), nil
}

func (d *rollbackTestDao) RestoreRollbackState(raw []byte) error {
	return d.Unmarshal(raw)
}

func (d *rollbackTestDao) PrepareMutation(change PersistChange) (dataengine.Mutation, error) {
	version := d.Tracker.Version()
	return dataengine.Mutation{
		Key:  dataengine.DocumentKey{Database: "test", Resource: d.CollName(), ID: d.id},
		Kind: dataengine.MutationPut, ExpectedVersion: version, NextVersion: version + 1,
		Mask: change.Mask, Schema: 1, Codec: "json", Data: d.Marshal(),
	}, nil
}

func (d *rollbackTestDao) AcceptMutation(mutation dataengine.Mutation) error {
	return d.Tracker.AcceptVersion(mutation.ExpectedVersion, mutation.NextVersion)
}

type recordingCommitter struct {
	record   CommitRecord
	released TransactionID
	err      error
}

func (c *recordingCommitter) Commit(_ context.Context, record CommitRecord) error {
	c.record = record
	return c.err
}

func (c *recordingCommitter) TransactionReleased(id TransactionID) {
	c.released = id
}

type rollbackTestEntity struct {
	*entity.EntityBase
	dao *rollbackTestDao
}

func (e *rollbackTestEntity) Base() *entity.EntityBase { return e.EntityBase }

func (e *rollbackTestEntity) RangeDao(f func(entity.DaoInterface)) {
	if f != nil {
		f(e.dao)
	}
}

func newMockGetter() *mockGetter {
	return &mockGetter{entities: make(map[int64]entity.IThreadSafeEntity), groups: entity.NewEntityManager()}
}

func (g *mockGetter) Add(e entity.IThreadSafeEntity) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.entities[e.ID()] = e
}

func (g *mockGetter) Get(_ context.Context, id int64, _ entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	e, ok := g.entities[id]
	if !ok {
		return nil, ErrEntityNotFound
	}
	return e, nil
}

func (g *mockGetter) GetMany(_ context.Context, ids []int64, _ []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	ret := make([]entity.IThreadSafeEntity, len(ids))
	for i, id := range ids {
		ret[i] = g.entities[id]
	}
	return ret, nil
}

func (g *mockGetter) GetGroupEntity(groupID, entityID int64) entity.IThreadSafeEntity {
	if g.groups == nil {
		return nil
	}
	return g.groups.GetGroupEntity(groupID, entityID)
}

func (g *mockGetter) GetGroupEntities(groupID int64) []entity.IThreadSafeEntity {
	if g.groups == nil {
		return nil
	}
	return g.groups.GetGroupEntities(groupID)
}

func (g *mockGetter) UpdateEntityGroup(value entity.IThreadSafeEntity, groupID int64) error {
	if g.groups == nil {
		return entity.ErrEntityNotManaged
	}
	return g.groups.UpdateEntityGroup(value, groupID)
}
