package engine

import (
	"context"
	"errors"
	"sync"
	"testing"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// RR-20260926-71：kind 经 kind 定义声明 remote=managed，手写 builder 省略 RemotePolicy（注册表按“部分重复声明”接受）。
// 全部 Remote 路径都按托管处理，冷加载也必须按 kind 的实际策略（entity.GetEntityKindRemotePolicy）恢复 Remote 版本信封，
// 与 RR-20260926-60 一致；不能只看 builder 自带的字段。
const kindDefManagedRepositoryKind entity.EntityKind = 247

var kindDefManagedRepositoryOnce sync.Once

// registerKindDefManagedRepositoryKind：先经 kind 定义声明 managed，再注册省略 RemotePolicy 与 Lifetime 的手写 builder。
func registerKindDefManagedRepositoryKind() {
	kindDefManagedRepositoryOnce.Do(func() {
		entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kindDefManagedRepositoryKind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1, Kind: kindDefManagedRepositoryKind, // RemotePolicy 与 Lifetime 都省略
			DaoBuilders: []entity.DaoBuilderFunc{func() entity.DaoInterface {
				return &dataEngineRepositoryDAO{collection: "rr71_remote"}
			}},
			Builder: func(param *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
				return &dataEngineRemoteRepositoryEntity{RemoteEntityBase: entity.NewRemoteEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind)}, nil
			},
		})
	})
}

// REPRO-2026-09-26-06 §7 探针改写：修前向量 {StateVersion:0 …}、ownership unknown，“无版本信封视为损坏”检查被跳过。
func TestRepositoryRestoresRemoteEnvelopeByKindPolicy(t *testing.T) {
	registerKindDefManagedRepositoryKind()
	if !entity.IsEntityKindRemoteManaged(kindDefManagedRepositoryKind) {
		t.Fatal("setup: kind is not managed")
	}
	if entity.GetEntityBuilderParam(kindDefManagedRepositoryKind).RemotePolicy.RemoteManaged() {
		t.Fatal("setup: the builder itself must omit RemotePolicy")
	}
	id, _ := entity.BuildEntityID(9971, kindDefManagedRepositoryKind)
	inner, _ := bson.Marshal(bson.M{"_id": id, "_schema": uint32(1), "name": "remote"})
	outer, _ := bson.Marshal(bson.M{"_id": id, "_ver": uint64(12), "_marker_epoch": uint64(3), "_lock_fence": uint64(8), "_route_epoch": uint64(5), "data": inner})
	enveloped := coredata.RawDocument{
		Key:     coredata.DocumentKey{Database: "game", Resource: "rr71_remote", ID: id},
		Version: 12, MarkerEpoch: 3, LockFence: 8, RouteEpoch: 5, Enveloped: true, Data: outer,
	}
	store := &repositoryStore{docs: map[string][]coredata.RawDocument{"rr71_remote": {enveloped}}}
	repository, err := newEntityRepository(entity.NewEntityManager(), store, nil, repositoryGate(true))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := repository.LoadEntity(context.Background(), id, kindDefManagedRepositoryKind)
	if err != nil {
		t.Fatal(err)
	}
	remote := loaded.(*dataEngineRemoteRepositoryEntity)
	want := entity.RemoteVersionVector{StateVersion: 12, MarkerEpoch: 3, LockFence: 8, RouteEpoch: 5}
	if vector := remote.RemoteVersionVector(); vector != want {
		t.Fatalf("kind is remote-managed in the registry, but the cold load dropped the stored Remote envelope: vector=%+v ownership=%v (want %+v, recovering)", vector, remote.RemoteOwnershipState(), want)
	}
	if got := remote.RemoteOwnershipState(); got != entity.RemoteOwnershipRecovering {
		t.Fatalf("ownership=%v, want recovering after a cold load", got)
	}
}

// 没有版本信封的记录对托管 kind 视为损坏，同样按 kind 策略判断（修前检查被跳过，按普通实体加载成功）。
func TestRepositoryRejectsManagedKindRecordWithoutEnvelope(t *testing.T) {
	registerKindDefManagedRepositoryKind()
	otherID, _ := entity.BuildEntityID(9972, kindDefManagedRepositoryKind)
	bare := &repositoryStore{docs: map[string][]coredata.RawDocument{"rr71_remote": {repositoryRaw(t, "rr71_remote", otherID, 4)}}}
	repository, err := newEntityRepository(entity.NewEntityManager(), bare, nil, repositoryGate(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.LoadEntity(context.Background(), otherID, kindDefManagedRepositoryKind); !errors.Is(err, ErrEntityAggregateCorrupt) {
		t.Fatalf("a remote-managed kind loaded a record without a version envelope: err=%v, want ErrEntityAggregateCorrupt", err)
	}
}
