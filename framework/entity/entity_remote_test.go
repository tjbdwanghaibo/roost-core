// Remote 实体基类与契约：版本向量一致且不回退、远端提交字段校验、DAO 作用域整表校验、远端视图引用与读选项。

package entity

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

const (
	testRemotePlayerKind   EntityKind = 101
	testRemoteAllianceKind EntityKind = 102
)

// testRemoteEntity is a minimal remote entity for testing.
type testRemoteEntity struct {
	RemoteEntityBase
	cleared bool
}

func (t *testRemoteEntity) Base() *EntityBase              { return &t.EntityBase }
func (t *testRemoteEntity) OnDataChange(_ []byte, _ int64) {}

func mustBuildRemoteEntityID(uniqueID int64, category EntityCategory, kind EntityKind) int64 {
	if kind == EntityKindNone {
		kind = remoteTestKind(category)
	}
	MustRegisterEntityKindCategory(kind, category)
	id, err := BuildEntityID(uniqueID, kind)
	if err != nil {
		panic(err)
	}
	return id
}

func remoteTestKind(category EntityCategory) EntityKind {
	if category == testEntityCategoryAlliance {
		return testRemoteAllianceKind
	}
	return testRemotePlayerKind
}

func newTestRemoteEntity(id int64, typo EntityCategory) *testRemoteEntity {
	e := &testRemoteEntity{}
	kind := remoteTestKind(typo)
	e.EntityBase = *NewEntityBase(mustBuildRemoteEntityID(id, typo, kind), typo, false, kind)
	e.EntityBase.SetHooks(func() { e.cleared = true }, nil)
	return e
}

func newMarkedTestRemoteEntity(id int64, typo EntityCategory) *testRemoteEntity {
	e := &testRemoteEntity{}
	kind := remoteTestKind(typo)
	MustRegisterEntityKindCategory(kind, typo)
	e.EntityBase = *NewEntityBase(makeEntityID(id, typo, kind, true), typo, false, kind)
	e.EntityBase.SetHooks(func() { e.cleared = true }, nil)
	return e
}

func TestRemoteEntityBase_Interface(t *testing.T) {
	e := newTestRemoteEntity(100, testEntityCategoryPlayer)

	// Verify IThreadSafeRemoteEntity contract
	var _ IThreadSafeRemoteEntity = e

	if e.EntityVersion() != 0 {
		t.Fatal("initial entity version should be 0")
	}
	if err := e.SetEntityVersion(42); err != nil {
		t.Fatal(err)
	}
	if e.EntityVersion() != 42 {
		t.Fatalf("expected version 42, got %d", e.EntityVersion())
	}

	if e.ExcludeSId() != 0 {
		t.Fatal("initial excludeSId should be 0")
	}
	e.SetExcludeSId(1001)
	if e.ExcludeSId() != 1001 {
		t.Fatalf("expected excludeSId 1001, got %d", e.ExcludeSId())
	}
}

func TestRemoteEntityBase_VersionVectorIsCoherent(t *testing.T) {
	base := NewRemoteEntityBase(1, testEntityCategoryPlayer, false, testRemotePlayerKind)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for generation := uint64(2); generation < 10_000; generation++ {
			if err := base.SetRemoteVersionVector(RemoteVersionVector{
				StateVersion: generation,
				MarkerEpoch:  generation,
				LockFence:    generation,
				RouteEpoch:   generation,
			}); err != nil {
				t.Errorf("set vector: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 10_000; i++ {
			version := base.RemoteVersionVector()
			if version.StateVersion == 0 {
				continue
			}
			if version.StateVersion != version.MarkerEpoch || version.StateVersion != version.LockFence || version.StateVersion != version.RouteEpoch {
				t.Errorf("torn version vector: %+v", version)
				return
			}
		}
	}()
	wg.Wait()
}

func TestRemoteEntityBase_IsRemoteCapable(t *testing.T) {
	e := newTestRemoteEntity(100, testEntityCategoryPlayer)
	if e.IsRemoteCapable() {
		t.Fatal("entity without remote bit should not be marked remote")
	}

	e2 := newMarkedTestRemoteEntity(200, testEntityCategoryPlayer)
	if !e2.IsRemoteCapable() {
		t.Fatal("entity with remote bit should be marked remote")
	}
}

func TestRemoteEntityBase_GUId(t *testing.T) {
	rawID := int64(500)
	e := newTestRemoteEntity(rawID, testEntityCategoryPlayer)

	guid := e.GUId()
	meta := ResolveEntityID(guid)
	if meta.UniqueID != rawID {
		t.Fatalf("expected rawID %d, got %d", rawID, meta.UniqueID)
	}
	if meta.Category != testEntityCategoryPlayer {
		t.Fatalf("expected type %d, got %d", testEntityCategoryPlayer, meta.Category)
	}
}

func TestRemoteSnapshotAllowStaleDoesNotBypassExpiry(t *testing.T) {
	snapshot := RemoteSnapshot{Version: 1, ExpiresAt: 100}
	if snapshot.Accepts(RemoteReadOption{AllowStale: true, NowMillis: 101}) {
		t.Fatal("allow stale must not accept an expired cache entry")
	}
	if snapshot.Accepts(RemoteReadOption{AllowStale: true, NowMillis: 100}) {
		t.Fatal("snapshot must expire at its expiry boundary")
	}
}

// SetRemoteVersionVector 在同一 fence 下拒绝 StateVersion 回退（ErrRemoteVersionConflict），更小的 fence 仍是 ErrRemoteFenced；
// 同 fence 的相等 / 更大版本、更大 fence 的任意版本照常写入。
// 历史：RR-20260927-15。
func TestSetRemoteVersionVectorRejectsSameFenceRegression(t *testing.T) {
	current := RemoteVersionVector{StateVersion: 5, MarkerEpoch: 1, LockFence: 7, RouteEpoch: 1}
	for _, tc := range []struct {
		name string
		next RemoteVersionVector
		want error
	}{
		{"same fence, lower version", RemoteVersionVector{StateVersion: 4, MarkerEpoch: 1, LockFence: 7, RouteEpoch: 1}, ErrRemoteVersionConflict},
		{"lower fence", RemoteVersionVector{StateVersion: 6, MarkerEpoch: 1, LockFence: 6, RouteEpoch: 1}, ErrRemoteFenced},
		{"same fence, same version", current, nil},
		{"same fence, higher version", RemoteVersionVector{StateVersion: 6, MarkerEpoch: 1, LockFence: 7, RouteEpoch: 1}, nil},
		{"same fence, marker change", RemoteVersionVector{StateVersion: 5, MarkerEpoch: 2, LockFence: 7, RouteEpoch: 2}, nil},
		{"higher fence, lower version", RemoteVersionVector{StateVersion: 3, MarkerEpoch: 1, LockFence: 8, RouteEpoch: 1}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := NewRemoteEntityBase(1, EntityCategoryNone, false, 0)
			if err := base.SetRemoteVersionVector(current); err != nil {
				t.Fatal(err)
			}
			err := base.SetRemoteVersionVector(tc.next)
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("SetRemoteVersionVector(%+v) = %v, want %v", tc.next, err, tc.want)
			}
			want := current
			if tc.want == nil {
				want = tc.next
			}
			if got := base.RemoteVersionVector(); got != want {
				t.Fatalf("vector=%+v, want %+v", got, want)
			}
		})
	}
}

// SetEntityVersion 与 SetRemoteVersionVector 同一规则：它只改 StateVersion、沿用当前 fence，所以更小的版本被拒绝
// （ErrRemoteVersionConflict）、向量不变；相等 / 更大照常写入，其余维度不动。
// 历史：RR-20260930-13。
func TestSetEntityVersionRejectsRewindUnderSameFence(t *testing.T) {
	current := RemoteVersionVector{StateVersion: 5, MarkerEpoch: 1, LockFence: 7, RouteEpoch: 1}
	for _, tc := range []struct {
		name    string
		next    int64
		want    uint64 // 调用后的 StateVersion
		wantErr error
	}{
		{"lower version", 4, 5, ErrRemoteVersionConflict},
		{"negative (treated as 0)", -1, 5, ErrRemoteVersionConflict},
		{"same version", 5, 5, nil},
		{"higher version", 6, 6, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := NewRemoteEntityBase(1, EntityCategoryNone, false, 0)
			if err := base.SetRemoteVersionVector(current); err != nil {
				t.Fatal(err)
			}
			// 修前红测试只用旧 API（无返回值）断言向量不回退；修后 SetEntityVersion 返回 error，这里一并钉住哨兵。
			err := base.SetEntityVersion(tc.next)
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("SetEntityVersion(%d) = %v, want %v", tc.next, err, tc.wantErr)
			}
			got := base.RemoteVersionVector()
			want := current
			want.StateVersion = tc.want
			if got != want {
				t.Fatalf("after SetEntityVersion(%d): vector=%+v, want %+v (a lower StateVersion under the same fence must not be written)", tc.next, got, want)
			}
		})
	}
}

const promiseKind EntityKind = 177

var promiseEntityID, promiseOtherEntityID int64

func init() {
	MustRegisterEntityKindDefs(EntityKindDef{Kind: promiseKind, Category: 1, RemotePolicy: RemotePolicyManaged})
	var err error
	if promiseEntityID, err = BuildEntityID(991, promiseKind); err != nil {
		panic(err)
	}
	if promiseOtherEntityID, err = BuildEntityID(992, promiseKind); err != nil {
		panic(err)
	}
}

func validRemoteCommit() RemoteCommit {
	data := []byte(`{"hp":1}`)
	id := promiseEntityID
	return RemoteCommit{
		TransactionID: RemoteTransactionID{1}, EntityID: id, Kind: promiseKind, BaseVersion: 3, NextVersion: 4, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1,
		Mutations: []RemoteDataMutation{{Database: "game", Collection: "hero", ID: id, Version: 4, Data: data}},
		Snapshots: []RemoteSnapshotRecord{{Key: RemoteSnapshotKey{EntityID: id, Kind: promiseKind, Scope: 1}, BaseVersion: 3, StateVersion: 4, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true, Data: data, Checksum: RemoteSnapshotChecksum(data)}},
	}
}

// RemoteCommit.Validate is the admission gate for every remote-entity write:
// identity, version arithmetic, ownership epochs, the mutation / delete
// shape, per-record agreement with the commit header, duplicates, and
// snapshot integrity. Each rule is pinned by sentinel and message with a
// single-field mutation of a valid commit.
func TestRemoteCommitValidateRefusesEachBrokenField(t *testing.T) {
	if err := validRemoteCommit().Validate(); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	cases := []struct {
		name     string
		mutate   func(*RemoteCommit)
		sentinel error
		text     string
	}{
		{"zero transaction id", func(c *RemoteCommit) { c.TransactionID = RemoteTransactionID{} }, ErrRemoteRejected, "invalid identity"},
		{"zero entity id", func(c *RemoteCommit) { c.EntityID = 0 }, ErrRemoteRejected, "invalid identity"},
		{"no kind", func(c *RemoteCommit) { c.Kind = EntityKindNone }, ErrRemoteRejected, "invalid identity"},
		{"version not consecutive", func(c *RemoteCommit) { c.NextVersion = 5 }, ErrRemoteVersionConflict, "base=3 next=5"},
		{"no marker epoch", func(c *RemoteCommit) { c.MarkerEpoch = 0 }, ErrRemoteFenced, "missing ownership epoch"},
		{"no route epoch", func(c *RemoteCommit) { c.RouteEpoch = 0 }, ErrRemoteFenced, "missing ownership epoch"},
		{"update without mutations", func(c *RemoteCommit) { c.Mutations = nil; c.Snapshots = nil }, ErrRemoteRejected, "empty mutation set"},
		{"delete carrying mutations", func(c *RemoteCommit) {
			c.Delete = true
			c.Deletes = []RemoteDataDelete{{Database: "game", Collection: "hero", ID: promiseEntityID}}
		}, ErrRemoteRejected, "delete commit contains data mutations"},
		{"delete without deletes", func(c *RemoteCommit) { c.Delete = true; c.Mutations = nil; c.Snapshots = nil }, ErrRemoteRejected, "delete commit contains data mutations"},
		{"update carrying deletes", func(c *RemoteCommit) {
			c.Deletes = []RemoteDataDelete{{Database: "game", Collection: "hero", ID: promiseEntityID}}
		}, ErrRemoteRejected, "update commit contains data deletes"},
		{"mutation invalid on its own", func(c *RemoteCommit) { c.Mutations[0].Data = nil }, ErrRemoteRejected, "mutation 0"},
		{"mutation for another entity", func(c *RemoteCommit) { c.Mutations[0].ID = promiseOtherEntityID }, ErrRemoteRejected, "mutation identity mismatch"},
		{"mutation at another version", func(c *RemoteCommit) { c.Mutations[0].Version = 5 }, ErrRemoteVersionConflict, "mutation version mismatch"},
		{"duplicate mutation", func(c *RemoteCommit) { c.Mutations = append(c.Mutations, c.Mutations[0]) }, ErrRemoteRejected, "duplicate data mutation"},
		{"delete for another entity", func(c *RemoteCommit) {
			c.Delete, c.Mutations, c.Snapshots = true, nil, nil
			c.Deletes = []RemoteDataDelete{{Database: "game", Collection: "hero", ID: promiseOtherEntityID}}
		}, ErrRemoteRejected, "delete 0"},
		{"duplicate delete", func(c *RemoteCommit) {
			c.Delete, c.Mutations, c.Snapshots = true, nil, nil
			d := RemoteDataDelete{Database: "game", Collection: "hero", ID: promiseEntityID}
			c.Deletes = []RemoteDataDelete{d, d}
		}, ErrRemoteRejected, "duplicate data delete"},
		{"snapshot for another entity", func(c *RemoteCommit) { c.Snapshots[0].Key.EntityID = promiseOtherEntityID }, ErrRemoteRejected, "invalid snapshot 0"},
		{"snapshot at another version", func(c *RemoteCommit) { c.Snapshots[0].StateVersion = 5 }, ErrRemoteRejected, "invalid snapshot 0"},
		{"snapshot with another epoch", func(c *RemoteCommit) { c.Snapshots[0].MarkerEpoch = 2 }, ErrRemoteRejected, "invalid snapshot 0"},
		{"snapshot without schema", func(c *RemoteCommit) { c.Snapshots[0].Schema = 0 }, ErrRemoteRejected, "invalid snapshot 0"},
		{"snapshot without data", func(c *RemoteCommit) { c.Snapshots[0].Data = nil }, ErrRemoteRejected, "invalid snapshot 0"},
		{"snapshot checksum lies", func(c *RemoteCommit) { c.Snapshots[0].Checksum++ }, ErrRemoteRejected, "snapshot checksum mismatch"},
		{"duplicate snapshot", func(c *RemoteCommit) { c.Snapshots = append(c.Snapshots, c.Snapshots[0]) }, ErrRemoteRejected, "duplicate snapshot"},
		{"invalidation for another entity", func(c *RemoteCommit) {
			c.Invalidations = []RemoteSnapshotKey{{EntityID: promiseOtherEntityID, Kind: promiseKind, Scope: 1}}
		}, ErrRemoteRejected, "invalid snapshot invalidation"},
		{"invalidation of a snapshot in the same commit", func(c *RemoteCommit) { c.Invalidations = []RemoteSnapshotKey{c.Snapshots[0].Key} }, ErrRemoteRejected, "duplicate snapshot invalidation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validRemoteCommit()
			tc.mutate(&c)
			err := c.Validate()
			if !errors.Is(err, tc.sentinel) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Validate = %v, want %v containing %q", err, tc.sentinel, tc.text)
			}
		})
	}
}

// ValidateEntityRegistry 与 Remote 装配期同一规则、同一哨兵拒绝“remote=managed + dbscope=sid”（可 errors.Is
// ErrRemoteManagedServerScopedDAO），不装 Remote 的进程也拦得住；每个违规 DAO 只报一次，合法组合不受影响。
// 历史：RR-20260927-09。

type scopedRegistryDao struct {
	factoryTestDao
	scope DatabaseScope
}

func (d *scopedRegistryDao) DbScope() DatabaseScope { return d.scope }

func registerScopedRegistryKind(kind EntityKind, policy RemotePolicy, scope DatabaseScope) {
	MustRegisterEntityKindDefs(EntityKindDef{Kind: kind, Category: EntityCategoryRemote, RemotePolicy: policy})
	RegisterEntityBuilder(&EntityBuilderParam{
		Category: EntityCategoryRemote,
		Kind:     kind,
		Builder: func(*EntityCreateParam) (IThreadSafeEntity, error) {
			return nil, errors.New("never built")
		},
		DaoBuilders: []DaoBuilderFunc{
			func() DaoInterface { return &factoryTestDao{coll: "plain"} },
			func() DaoInterface {
				return &scopedRegistryDao{factoryTestDao: factoryTestDao{coll: "scoped"}, scope: scope}
			},
		},
	})
}

func TestValidateEntityRegistryRejectsRemoteManagedServerScopedDao(t *testing.T) {
	isolateEntityRegistry(t)
	registerScopedRegistryKind(141, RemotePolicyManaged, DatabaseServer)
	registerScopedRegistryKind(142, RemotePolicyManaged, DatabaseGlobal)
	registerScopedRegistryKind(144, RemotePolicyNone, DatabaseServer)

	err := ValidateEntityRegistry()
	if err == nil || !strings.Contains(err.Error(), "dbscope=sid") {
		t.Fatalf("ValidateEntityRegistry accepted remote-managed kind 141 with a dbscope=sid DAO: err=%v", err)
	}
	if !errors.Is(err, ErrRemoteManagedServerScopedDAO) {
		t.Fatalf("err=%v, want errors.Is ErrRemoteManagedServerScopedDAO", err)
	}
	if n := strings.Count(err.Error(), "kind=141 "); n != 1 {
		t.Fatalf("kind 141 reported %d times, want once: %v", n, err)
	}
	for _, legal := range []string{"kind=142 ", "kind=144 "} {
		if strings.Contains(err.Error(), legal) {
			t.Fatalf("legal combination %q reported: %v", legal, err)
		}
	}
}

func TestRemoteViewRefValidatesEntityIDAndKind(t *testing.T) {
	rawID := int64(1001)
	// 只撤销本用例自己的登记：清空整张注册表会让第二轮起 init / sync.Once 注册的 kind 全部丢失。
	isolateEntityRegistry(t)
	kind := EntityKind(7)
	MustRegisterEntityKindCategory(kind, EntityCategory(1))

	id, err := BuildEntityID(rawID, kind)
	if err != nil {
		t.Fatalf("build id: %v", err)
	}
	ref := RemoteViewRef{EntityID: id, Kind: kind, Version: 12}
	if !ref.Valid() {
		t.Fatalf("expected ref to be valid: %+v", ref)
	}
	if ref.UniqueID() != rawID {
		t.Fatalf("unique id = %d, want %d", ref.UniqueID(), rawID)
	}

	bad := RemoteViewRef{EntityID: id, Kind: EntityKind(8)}
	if bad.Valid() {
		t.Fatalf("kind-mismatched ref should be invalid")
	}
}

func TestRemoteReadOptionDefaultsRequireFreshData(t *testing.T) {
	option := NormalizeRemoteReadOption(RemoteReadOption{})
	if option.AllowStale {
		t.Fatalf("default read option should not allow stale data")
	}
	if option.MinVersion != 0 {
		t.Fatalf("default min version = %d, want 0", option.MinVersion)
	}
}

func TestRemoteSnapshotAcceptsVersionAndPreservesImmutableData(t *testing.T) {
	snapshot := RemoteSnapshot{
		EntityID:   1001,
		Kind:       EntityKind(7),
		Scope:      3,
		Version:    12,
		RouteEpoch: 5,
		Source:     RemoteSnapshotSourceCache,
		ReadAt:     100,
		ExpiresAt:  200,
		Data:       "view-data",
	}
	if !snapshot.Accepts(RemoteReadOption{MinVersion: 12}) {
		t.Fatalf("snapshot version should satisfy min version")
	}
	if snapshot.Accepts(RemoteReadOption{MinVersion: 13}) {
		t.Fatalf("snapshot version should reject newer min version")
	}
	if got := snapshot.AsString(); got != "view-data" {
		t.Fatalf("snapshot string = %q, want view-data", got)
	}
	if snapshot.Expired(199) {
		t.Fatalf("snapshot should not expire before ExpiresAt")
	}
	if !snapshot.Expired(201) {
		t.Fatalf("snapshot should expire after ExpiresAt")
	}
	if snapshot.Source != RemoteSnapshotSourceCache {
		t.Fatalf("snapshot source = %v, want cache", snapshot.Source)
	}
}
