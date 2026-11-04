// 只读进程（装 Mirror、不装 remote_entity 写能力）里 Nest 的 RemoteViewRef 预取：快照来源接到 MirrorSource。

package nest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	coreremote "github.com/tjbdwanghaibo/roost-core/framework/remoteentity"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// mirrorReadKind 是本包测试注册的 Remote 托管 kind、mirrorPlayerKind 是发起读取的本地实体 kind（本包其他测试不注册
// kind，两者不能撞号）；mirrorReadSchema 是它的快照 schema
// （快照解码表是进程级的，登记一次，-count>1 安全）。
const (
	mirrorReadKind   entity.EntityKind = 202
	mirrorPlayerKind entity.EntityKind = 203
	mirrorReadSchema uint32            = 9202
)

var registerMirrorRead = sync.OnceFunc(func() {
	entity.MustRegisterEntityKindDefs(
		entity.EntityKindDef{Kind: mirrorReadKind, Category: 1, RemotePolicy: entity.RemotePolicyManaged},
		entity.EntityKindDef{Kind: mirrorPlayerKind, Category: 1},
	)
	entity.MustRegisterRemoteSnapshotDecoder(mirrorReadSchema, func(data []byte) (any, error) { return string(data), nil })
})

// mirrorAuthority 是只读进程看到的权威：按当前版本给出快照，记下每次回源。
type mirrorAuthority struct {
	mu      sync.Mutex
	version uint64
	loads   int
}

func (a *mirrorAuthority) set(version uint64) {
	a.mu.Lock()
	a.version = version
	a.mu.Unlock()
}

func (a *mirrorAuthority) load(_ context.Context, key entity.RemoteSnapshotKey, _ entity.RemoteReadConsistency, _ uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.loads++
	data := []byte{byte('0' + a.version)}
	return entity.RemoteSnapshotEnvelope{
		Key: key, StateVersion: a.version, BaseVersion: a.version - 1, MarkerEpoch: 1, RouteEpoch: 1,
		Schema: mirrorReadSchema, Codec: 1, Full: true, Checksum: entity.RemoteSnapshotChecksum(data),
		Payload: entity.CopyFrozenRemoteSnapshotPayload(data),
	}, true, nil
}

type viewRequest struct{ access corenest.RemoteAccess }

func (r viewRequest) RemoteAccess() []corenest.RemoteAccess { return []corenest.RemoteAccess{r.access} }

type localEntity struct{ *entity.EntityBase }

func (e *localEntity) Base() *entity.EntityBase { return e.EntityBase }

type oneEntityGetter struct{ e entity.IThreadSafeEntity }

func (g oneEntityGetter) Get(_ context.Context, id int64, _ entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	if id != g.e.GUId() {
		return nil, corenest.ErrEntityNotFound
	}
	return g.e, nil
}
func (g oneEntityGetter) GetMany(ctx context.Context, ids []int64, categories []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	out := make([]entity.IThreadSafeEntity, 0, len(ids))
	for i, id := range ids {
		e, err := g.Get(ctx, id, categories[i])
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// 联盟服务方案 §2.4 / §2.10 第 3 项：只读进程只装 Mirror（mods.ModRemoteMirror），没有 remote_entity 写能力。
// 玩家 handler 的 RemoteViewRef 预取经 Nest Mod 接到 MirrorSource：Cached 读到 Mirror 快照；权威前进后 Cached 仍可以
// 是缓存里的旧版本，Monotonic 带 after（写 RPC 返回的版本）读到不低于它的新版本。
func TestReadOnlyProcessPrefetchesRemoteViewThroughMirror(t *testing.T) {
	registerMirrorRead()
	corenest.ResetHandlersForTest()
	t.Cleanup(corenest.ResetHandlersForTest)

	authority := &mirrorAuthority{version: 1}
	client, err := coreremote.NewSnapshotClient(nil, coreremote.SnapshotClientDeps{Loader: authority.load, ConsumerSID: 4200})
	if err != nil {
		t.Fatal(err)
	}
	cfg := viper.New()
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModDataEngine, dataEngineNestProvider{committer: noOpCommitter{}}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(mods.ModRemoteMirror, entity.RemoteSnapshotReadOnly(client)); err != nil {
		t.Fatal(err)
	}
	playerID, err := entity.BuildEntityID(7301, mirrorPlayerKind)
	if err != nil {
		t.Fatal(err)
	}
	player := &localEntity{entity.NewEntityBase(playerID, entity.EntityCategory(1), false, mirrorPlayerKind)}
	mod := NewMod(oneEntityGetter{player})
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mod.StopWithContext(context.Background()) })

	allianceID, err := entity.BuildEntityID(7302, mirrorReadKind)
	if err != nil {
		t.Fatal(err)
	}
	scope := uint64(entity.RemoteSnapshotScope("alliance"))
	key := corenest.RemoteKey[string]{Alias: "alliance"}
	name := corenest.NewHandlerName("test_read_only_mirror_prefetch")
	corenest.MustRegisterMemoryHandler(name, func([]entity.IThreadSafeEntity, []any, ...corenest.HandlerOption) (any, error) {
		view, ok := corenest.Remote(key)
		if !ok {
			return nil, errors.New("alliance snapshot was not prefetched")
		}
		return view, nil
	})
	read := func(consistency entity.RemoteReadConsistency, after uint64) (any, error) {
		return mod.Engine().Request(context.Background(), name, playerID, corenest.NewParams(viewRequest{corenest.RemoteAccess{
			Alias: "alliance", Ref: entity.RemoteViewRef{EntityID: allianceID, Kind: mirrorReadKind, RouteEpoch: 1},
			Scope: scope, Consistency: consistency, MinVersion: after, Required: true,
		}}))
	}

	// 本用例没有共享 L2：Cached 冷读按设计是未命中（不回源），先用 Monotonic 装入 L1，Cached 再读同一份。
	if got, err := read(entity.RemoteReadMonotonic, 0); err != nil || got != "1" {
		t.Fatalf("first Monotonic read through the mirror = %v, %v; want the mirror snapshot at version 1", got, err)
	}
	if got, err := read(entity.RemoteReadCached, 0); err != nil || got != "1" {
		t.Fatalf("Cached read through the mirror = %v, %v; want the mirror snapshot at version 1", got, err)
	}
	authority.set(3) // 写 RPC 提交了版本 3，并把 3 返回给调用方
	if got, err := read(entity.RemoteReadCached, 0); err != nil || got != "1" {
		t.Fatalf("Cached read after the authority moved = %v, %v; want the cached version 1 within the staleness bound", got, err)
	}
	if got, err := read(entity.RemoteReadMonotonic, 3); err != nil || got != "3" {
		t.Fatalf("Monotonic read after version 3 = %v, %v; want version 3", got, err)
	}
	if authority.loads != 2 {
		t.Fatalf("authority loads = %d, want 2 (first fill, then one Monotonic refill)", authority.loads)
	}
}
