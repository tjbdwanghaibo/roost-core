package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 搬档（单个玩家换服，维护者 2026-11-13：本地实体的 DAO 缺省 dbscope=sid，物理库 <库>_<sid> 每个服一个）。
//
// 换服原来只交接写权（各服共用 global 库）。DAO 按服分库之后，实体的文档要从源服的库搬到目标服的库，目标服才能冷加载它。
// 搬档由源服做，而且只在本进程已经交出这个实体（HandOffEntity 的屏障保持中）时做：交出保证本进程写过的都已投影、
// WAL checkpoint 已盖过、之后不再写，所以源库里的文档就是最终版本，读出来原样写进目标库（_version / _schema /
// _last_tx / 墓碑字段一字不改，目标服冷加载时从这些元数据继续，与 sidmerge 的迁移相同）。只搬 dbscope=sid 的 DAO；
// dbscope=global 的 DAO 各服共用一份，不动。两侧的物理库名按本进程的映射表（D9）换算，前提是两个服把同一个逻辑库
// 映射到同一个 Mongo 上的同一个前缀（与原来“共用 global 库”的前提相同）。
//
// 三个动作，写入各在一个 Mongo 事务里（全有或全无），都可以原样重做：
//
//   - CopyHandedOffEntity(id, to, overwrite)：源库 → 目标库。目标里没有 → 写入（copied）；已有同一版本（_version 与
//     _last_tx 都相同）→ 已在（already_present）；已有别的版本 → overwrite 时整文档覆盖（replaced），否则是冲突
//     ErrEntityCopyConflict、什么都不写。overwrite 只在“account 仍把这名玩家绑在本服、换服在途”时用：那时只有本服
//     写过他，目标库里别的版本只能是以前留下的旧副本（换走又换回、中止后没清掉）。account 已经改绑之后补搬时用
//     overwrite=false：目标服可能已经写过，不能覆盖。
//   - DropEntityCopy(copy, sid)：按复制结果删掉 sid 库里这些文档，条件是 _version 仍是复制时的版本——之后被写过的
//     文档不删（计入 Kept）。改绑成功之后删源库（本服不再预加载、按 ID 也加载不到他）；中止之后删目标库里这次写的副本。
//
// 不做的：源库与目标库的物理库名不同（各服映射到不同 Mongo）时这里写的是本进程映射里的目标库，目标服读不到——
// 与原来 global 库的前提相同，由部署保证（KNOWN-LIMITS“单个玩家换服”）。

var (
	// ErrEntityNotHandedOff：搬档要求本进程正保持这个实体的交出屏障（HandOffEntity 成立、还没 Release）。
	ErrEntityNotHandedOff = errors.New("dataengine copy: the entity is not handed off by this process; its documents may still change")
	// ErrEntityCopyConflict：目标库里已有这个实体的另一个版本，overwrite=false 时不覆盖。
	ErrEntityCopyConflict = errors.New("dataengine copy: the target database already holds a different version of the entity")
)

// 复制结果里每份文档的去向。
const (
	EntityCopyCopied         = "copied"
	EntityCopyAlreadyPresent = "already_present"
	EntityCopyReplaced       = "replaced"
)

// EntityCopy 是一次搬档的结果：源服 From 的库里这个实体的每份 dbscope=sid 文档，以及它写到目标服 To 的哪里。
// Documents 为空表示这个 kind 没有 dbscope=sid 的 DAO（或源库里没有它的文档），没有东西要搬。
type EntityCopy struct {
	EntityID  int64
	From, To  int32
	Documents []EntityCopyDocument
}

// EntityCopyDocument 是一份被复制的文档：Database 是物理库名的前缀（不含 _<sid>），Version 是复制的版本。
type EntityCopyDocument struct {
	Database   string
	Collection string
	Version    uint64
	Outcome    string
}

// copyMeta 是比较两份文档是否同一版本用到的元数据。
type copyMeta struct {
	Version uint64 `bson:"_version"`
	LastTx  string `bson:"_last_tx"`
	Deleted bool   `bson:"_deleted"`
}

// CopyHandedOffEntity 把本进程已交出的实体 id 的 dbscope=sid 文档复制到 sid=to 的库（见本文件开头）。
// 会等待，只能在业务池之外调用。实体没有处于交出状态返回 ErrEntityNotHandedOff，什么都不读写。
func (runtime *Runtime) CopyHandedOffEntity(ctx context.Context, id int64, to int32, overwrite bool) (result EntityCopy, err error) {
	if err := fctx.BlockingError("dataengine.CopyHandedOffEntity"); err != nil {
		return EntityCopy{}, err
	}
	store, keys, err := runtime.handedOffDocuments(id)
	if err != nil {
		return EntityCopy{}, err
	}
	if to <= 0 || to == store.cfg.ServerID {
		return EntityCopy{}, fmt.Errorf("dataengine copy: entity %d: target server %d is not another server (this is %d)", id, to, store.cfg.ServerID)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	defer func() {
		metrics.IncCounter("dataengine.entity_copy.total", metrics.Labels{"result": copyOutcomeLabel(err)}, 1)
	}()
	result = EntityCopy{EntityID: id, From: store.cfg.ServerID, To: to}
	type source struct {
		physical string
		key      coredata.DocumentKey
		doc      bson.M
		meta     copyMeta
	}
	var sources []source
	for _, key := range keys {
		physical, err := store.physicalDatabase(key)
		if err != nil {
			return EntityCopy{}, err
		}
		collection, err := store.collection(key)
		if err != nil {
			return EntityCopy{}, err
		}
		var raw bson.Raw
		if err := collection.FindOne(ctx, bson.M{"_id": id}, &raw); err != nil {
			if errors.Is(err, fmongo.ErrNotFound) {
				continue
			}
			return EntityCopy{}, fmt.Errorf("dataengine copy: read %s/%s/%d: %w", physical, key.Resource, id, err)
		}
		item := source{physical: physical, key: key}
		if err := bson.Unmarshal(raw, &item.doc); err != nil {
			return EntityCopy{}, err
		}
		if err := bson.Unmarshal(raw, &item.meta); err != nil {
			return EntityCopy{}, err
		}
		sources = append(sources, item)
	}
	if len(sources) == 0 {
		return result, nil
	}
	err = store.ReadConsistent(ctx, func(txCtx context.Context) error {
		// 事务可能被驱动重调：每次从头算结果。
		result.Documents = result.Documents[:0]
		for _, item := range sources {
			target := store.client.DatabaseForSid(item.physical, to).Collection(item.key.Resource)
			outcome := EntityCopyCopied
			var existing bson.Raw
			err := target.FindOne(txCtx, bson.M{"_id": id}, &existing)
			switch {
			case errors.Is(err, fmongo.ErrNotFound):
				if _, err := target.InsertOne(txCtx, item.doc); err != nil {
					return fmt.Errorf("dataengine copy: write %s_%d/%s/%d: %w", item.physical, to, item.key.Resource, id, err)
				}
			case err != nil:
				return fmt.Errorf("dataengine copy: read %s_%d/%s/%d: %w", item.physical, to, item.key.Resource, id, err)
			default:
				var meta copyMeta
				if err := bson.Unmarshal(existing, &meta); err != nil {
					return err
				}
				switch {
				case meta == item.meta:
					outcome = EntityCopyAlreadyPresent
				case !overwrite:
					return fmt.Errorf("%w: %s_%d/%s/%d holds version %d (last tx %q), this server has version %d (last tx %q)", ErrEntityCopyConflict,
						item.physical, to, item.key.Resource, id, meta.Version, meta.LastTx, item.meta.Version, item.meta.LastTx)
				default:
					if _, err := target.ReplaceOne(txCtx, bson.M{"_id": id}, item.doc); err != nil {
						return fmt.Errorf("dataengine copy: overwrite %s_%d/%s/%d: %w", item.physical, to, item.key.Resource, id, err)
					}
					outcome = EntityCopyReplaced
				}
			}
			result.Documents = append(result.Documents, EntityCopyDocument{
				Database: item.physical, Collection: item.key.Resource, Version: item.meta.Version, Outcome: outcome,
			})
		}
		return nil
	})
	if err != nil {
		return EntityCopy{}, err
	}
	slog.Info("dataengine: copied a handed-off entity to another server's database", "entity", id, "from", result.From, "to", to,
		"documents", len(result.Documents), "overwrite", overwrite)
	return result, nil
}

// DropEntityCopy 删掉 sid 库里 copy 记下的文档，条件是 _version 仍是复制时的版本（之后被写过的保留）。返回删掉的份数。
// 要求本进程正保持实体的交出屏障（ErrEntityNotHandedOff）。会等待，只能在业务池之外调用。
func (runtime *Runtime) DropEntityCopy(ctx context.Context, copied EntityCopy, sid int32) (dropped int, err error) {
	if err := fctx.BlockingError("dataengine.DropEntityCopy"); err != nil {
		return 0, err
	}
	store, _, err := runtime.handedOffDocuments(copied.EntityID)
	if err != nil {
		return 0, err
	}
	if sid <= 0 {
		return 0, fmt.Errorf("dataengine copy: server %d is not valid", sid)
	}
	if len(copied.Documents) == 0 {
		return 0, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	err = store.ReadConsistent(ctx, func(txCtx context.Context) error {
		dropped = 0
		for _, doc := range copied.Documents {
			collection := store.client.DatabaseForSid(doc.Database, sid).Collection(doc.Collection)
			deleted, err := collection.DeleteOne(txCtx, bson.M{"_id": copied.EntityID, "_version": int64(doc.Version)})
			if err != nil {
				return fmt.Errorf("dataengine copy: drop %s_%d/%s/%d: %w", doc.Database, sid, doc.Collection, copied.EntityID, err)
			}
			dropped += int(deleted)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	metrics.IncCounter("dataengine.entity_copy_drop.total", nil, int64(dropped))
	slog.Info("dataengine: dropped a copy of a handed-off entity", "entity", copied.EntityID, "sid", sid,
		"documents", len(copied.Documents), "dropped", dropped)
	return dropped, nil
}

// handedOffDocuments 确认实体 id 由本进程交出（屏障保持中），返回存储与它的 dbscope=sid DAO 位置。
func (runtime *Runtime) handedOffDocuments(id int64) (*MongoStore, []coredata.DocumentKey, error) {
	if runtime == nil || runtime.Store == nil || runtime.Projector == nil {
		return nil, nil, errors.New("dataengine copy: runtime is not configured")
	}
	if !runtime.Projector.entityHandedOff(id) {
		return nil, nil, fmt.Errorf("%w: entity %d", ErrEntityNotHandedOff, id)
	}
	kind := entity.GetEntityKindFromID(id)
	builder := entity.GetEntityBuilderParam(kind)
	if builder == nil {
		return nil, nil, fmt.Errorf("dataengine copy: entity %d: no builder for kind %d", id, kind)
	}
	var keys []coredata.DocumentKey
	for _, build := range builder.DaoBuilders {
		dao := build()
		if dao == nil || coredata.ResolveDatabaseScope(dao) != coredata.DatabaseServer {
			continue
		}
		keys = append(keys, coredata.DocumentKey{Database: dao.DbName(), Scope: coredata.DatabaseServer, Resource: dao.CollName()})
	}
	return runtime.Store, keys, nil
}

func copyOutcomeLabel(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrEntityCopyConflict):
		return "conflict"
	case errors.Is(err, ErrEntityNotHandedOff):
		return "not_handed_off"
	default:
		return "failed"
	}
}
