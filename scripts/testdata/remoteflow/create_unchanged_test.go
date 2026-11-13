package remoteflow

import (
	"errors"
	"os"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// KNOWN-LIMITS“Remote 托管 kind 的‘新建不改字段’”：新建的 Remote 托管实体由 Remote 批次写齐每个 DAO（Remote 信封：
// `_ver` / `data` / 所有权代际），真实 Mongo / Redis / NATS 上验收两种新建位置：
//   - outside_nest：Nest 之外新建（生成 lifecycle 的 GetOrCreate；game-demo 联盟 EnsureAlliance 同此），随后第一条声明它的
//     消息只改一个 DAO 的字段——另一个 DAO 没改，也要随这笔 Remote 批次以整文档写出。第一条声明它的消息一个字段都不改时
//     没有提交、只留所有权认领（REMOTE_ENTITY“空认领”：本实例拥有、业务上还不存在），这里记录为观察，不作断言；
//   - in_handler：handler 里新建一个没有声明进本消息的 Remote 托管实体（本消息声明的是另一个 Remote 实体，所以带
//     Remote 批次）。新实体不在批次里，没有它的所有权保护；Cast 对未声明的 Remote 目标拒绝（cast.go
//     refuseUndeclaredRemoteTargets），新建应同样拒绝，或者经 Remote 批次写信封——不能由本地投影以普通 DAO 文档写进
//     Remote 集合。
func TestGeneratedRemoteCreatedEntityWithoutFieldChangesIsPersisted(t *testing.T) {
	if os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI") == "" {
		t.Skip("run scripts/test-remote-generated.sh with isolated Mongo, Redis and NATS")
	}
	ctx, mongo, redis, database := rejectTestEnv(t)
	noop := nest.NewHandlerName("remote_create_unchanged_noop")
	balanceOnly := nest.NewHandlerName("remote_create_unchanged_balance_only")
	createOther := nest.NewHandlerName("remote_create_unchanged_create_other")
	var createID int64
	var createErr error
	rig := newRejectRig(t, ctx, mongo, redis, database, nest.DurabilityStrict, nil, func(rig *rejectRig) {
		meta := nest.HandlerMeta{Rollback: nest.RollbackState, Durability: nest.DurabilityStrict}
		rig.scheduler.MustRegisterHandlerWithMeta(noop, func([]entity.IThreadSafeEntity, []any, ...nest.HandlerOption) (any, error) {
			return nil, nil
		}, meta)
		rig.scheduler.MustRegisterHandlerWithMeta(balanceOnly, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
			vault := es[0].(*SyncedVault)
			vault.balance.SetValue(vault.balance.GetValue() + 1)
			return nil, nil
		}, meta)
		rig.scheduler.MustRegisterHandlerWithMeta(createOther, func([]entity.IThreadSafeEntity, []any, ...nest.HandlerOption) (any, error) {
			_, createErr = rig.access.Create(&entity.EntityCreateParam{IsCreate: true, Kind: EntityKindSyncedVault, Id: createID})
			return nil, createErr
		}, meta)
	})
	// documents 读两个 DAO 集合里该实体的原始文档（不存在为 nil）。
	documents := func(id int64) []bson.M {
		t.Helper()
		var out []bson.M
		for _, collection := range []string{"remote_balances", "remote_items"} {
			var raw bson.M
			if err := rig.mongo.Database(rig.database).Collection(collection).FindOne(rig.ctx, bson.M{"_id": id}, &raw); err != nil {
				raw = nil
			}
			out = append(out, raw)
		}
		return out
	}
	// prepareRemoteOwnership 在 t 已失败时 FailNow，所以先建好 in_handler 的调用方。
	caller := rig.seed(t, 9801)

	outside, err := entity.BuildEntityID(9800, EntityKindSyncedVault)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.access.Create(&entity.EntityCreateParam{IsCreate: true, Kind: EntityKindSyncedVault, Id: outside}); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.scheduler.Request(ctx, noop, outside, nil); err != nil {
		t.Fatalf("outside_nest: unchanged write: %v", err)
	}
	rig.assertNoPending(t)
	t.Logf("outside_nest: after a message that changes no field: %v", documents(outside))
	if _, err := rig.scheduler.Request(ctx, balanceOnly, outside, nil); err != nil {
		t.Fatalf("outside_nest: balance-only write: %v", err)
	}
	rig.assertNoPending(t)
	if version, balance, items, found := rig.stored(t, outside); !found || version != 1 || balance != 1 || items != 0 {
		t.Errorf("outside_nest: after a balance-only write Mongo version=%d balance=%d items=%d found=%v, want both DAO envelopes at version 1 (items written whole although unchanged)", version, balance, items, found)
	}
	if loaded, err := rig.runtime.Repository.LoadEntity(ctx, outside, EntityKindSyncedVault); err != nil || loaded == nil {
		t.Errorf("outside_nest: cold load from the authority: value=%v err=%v", loaded != nil, err)
	}

	if createID, err = entity.BuildEntityID(9802, EntityKindSyncedVault); err != nil {
		t.Fatal(err)
	}
	_, err = rig.scheduler.Request(ctx, createOther, caller, nil)
	rig.assertNoPending(t)
	docs := documents(createID)
	t.Logf("in_handler: reply=%v create=%v published=%v documents=%v", err, createErr, rig.access.Manager().Get(createID) != nil, docs)
	refused := err != nil && (errors.Is(err, entity.ErrRemoteWriteCapabilityDisabled) || errors.Is(err, nest.ErrDurableRemoteWriteUnsupported))
	switch {
	case refused && (docs[0] != nil || docs[1] != nil || rig.access.Manager().Get(createID) != nil):
		t.Errorf("in_handler: refused, yet the new entity was published=%v or written %v", rig.access.Manager().Get(createID) != nil, docs)
	case refused:
	case err != nil:
		t.Errorf("in_handler: reply %v, want success or a definite refusal of the undeclared Remote entity", err)
	default:
		for i, doc := range docs {
			_, envelope := doc["data"]
			_, ver := doc["_ver"]
			_, local := doc["_version"]
			if doc == nil || !envelope || !ver || local {
				t.Errorf("in_handler: DAO %d of a Remote-managed entity created in a message that does not declare it was written as %v, want a Remote envelope (_ver, data, ownership epochs) or a refusal", i, doc)
			}
		}
	}
	select {
	case err := <-rig.fatal:
		t.Fatalf("runtime fatal: %v", err)
	default:
	}
}
