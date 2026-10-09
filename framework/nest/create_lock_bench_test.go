package nest

import (
	"context"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// BenchmarkHandlerCreateEntity 测量 handler 内经 ManagerAccess.Create（生成 Lifecycle 的入口）新建实体的
// 整条请求：真实 EntityManager / ManagerAccess，单快 worker，每次新建一个新 ID（RR-20260926-48 取锁路径）。
func BenchmarkHandlerCreateEntity(b *testing.B) {
	for _, tc := range []struct {
		name string
		meta HandlerMeta
	}{
		{"memory", HandlerMeta{}},
		{"state_strict", HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			manager := entity.NewEntityManager()
			entity.MustRegisterEntityKindCategory(nestLocalKind, entity.EntityCategory(1))
			entity.MustRegisterEntityKindCategory(createdInScopeKind, entity.EntityCategory(1))
			targetID, err := entity.BuildEntityID(9700, nestLocalKind)
			if err != nil {
				b.Fatal(err)
			}
			target := &rollbackTestEntity{EntityBase: entity.NewEntityBase(targetID, entity.EntityCategory(1), false, nestLocalKind), dao: &rollbackTestDao{id: targetID, Value: 1}}
			if err := manager.TryAdd(target); err != nil {
				b.Fatal(err)
			}
			access := entity.NewManagerAccess(manager)
			engine := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 1024}, WorkerPoolConfig{}))
			name := NewHandlerName("bench_handler_create_" + tc.name)
			next := int64(200000)
			engine.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				next++
				_, err := access.Create(&entity.EntityCreateParam{IsCreate: true, UniqueID: next, Kind: createdInScopeKind, Category: entity.EntityCategory(1)})
				return nil, err
			}, tc.meta)
			if err := engine.Start(); err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				if _, err := engine.Request(ctx, name, targetID, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
