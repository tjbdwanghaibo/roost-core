package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 清理旧持久键（DESIGN-TODO D13 方案 A 的显式出口）。
//
// 生成的 DAO 把加载时读到、自己不认识的键（字段改名没保留 bson 名、删字段、改 nopersist 留下的旧键）原样留在
// 内存里，每次整文档写回都带回去，框架从不自动删除它们。迁移把旧键的值搬走（或确认不要了）之后，运维用这里
// 显式删除指定的键。规则与离线 schema 迁移一致（docs/framework/SCHEMA-MIGRATION.md）：
//   - 只在停写之后执行：所有写这个集合的进程已按正常停机退出（WAL 已全部投影），新程序还没启动。正在运行的
//     进程内存里仍持有旧键，下一次整文档写回会把它写回去。
//   - 每份被清理的文档带 `_version` 条件写回，`_version` 加 1、`_last_tx` 记为 "stored-key-cleanup:<Name>"：
//     没停干净的写入者按旧版本的条件写必然冲突，而不是悄悄把旧键写回；条件不成立时整次中止（ErrCleanupNotStopped）。
//   - 只删除 DAO 当前不声明的键（Declared 判定），以 _ 开头的框架元数据键、带 * 的路径拒绝。
//   - 墓碑（`_deleted: true`）不改：冷加载不读它的内容，重建时整文档替换。
//   - Remote 版本信封（带 `_ver` 的文档）拒绝：它的 DAO 数据在 data 二进制里，版本、所有权与 fence 由 Remote
//     链路维护，这个入口不改写它们（与离线迁移执行器同一规则）。

// StoredKeyCleanup 是一次清理。
type StoredKeyCleanup struct {
	// Name 进入 `_last_tx` 标记，用来区分清理写的版本。
	Name string
	// Collection 是 DAO 的集合。
	Collection fmongo.ICollection
	// Declared 是 DAO 当前的键集合（生成代码里的 <dao>StoredKeys）；仍被声明的键拒绝删除。
	Declared *coredata.StoredKeySet
	// Keys 是要删除的键：顶层键，或进入结构体子文档的点分路径（与加载期 WARN 报出的路径同形，不含 *）。
	Keys []string
	// DryRun 只统计，不写。
	DryRun bool
}

// StoredKeyCleanupReport 是一次清理的结果。
type StoredKeyCleanupReport struct {
	// Matched 是含有任一指定键的活文档数；Cleaned 是本次写回的份数（DryRun 为 0）。
	Matched int `json:"matched"`
	Cleaned int `json:"cleaned"`
	// Tombstones 是含有指定键、按规则跳过的墓碑数。
	Tombstones int `json:"tombstones"`
}

var (
	// ErrCleanupInvalid 表示请求本身不合法（缺集合 / 键集合、键为空、是元数据、仍被 DAO 声明）。
	ErrCleanupInvalid = errors.New("dataengine stored key cleanup: invalid request")
	// ErrCleanupNotStopped 表示执行期间有别的写入者改了文档：停写没有做完，整次中止。
	ErrCleanupNotStopped = errors.New("dataengine stored key cleanup: the collection is still being written; stop every writer first")
	// ErrCleanupRemoteEnvelope 表示集合里有 Remote 版本信封文档，本入口不改写它们。
	ErrCleanupRemoteEnvelope = errors.New("dataengine stored key cleanup: remote-managed documents (with _ver) are not rewritten here")
)

// CleanupStoredKeys 在停写的集合上删除 job.Keys。先检查全部条件（键、Remote 信封）再写，写入逐份带版本条件。
func CleanupStoredKeys(ctx context.Context, job StoredKeyCleanup) (StoredKeyCleanupReport, error) {
	var report StoredKeyCleanupReport
	if err := validateStoredKeyCleanup(job); err != nil {
		return report, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	anyKey := make(bson.A, 0, len(job.Keys))
	for _, key := range job.Keys {
		anyKey = append(anyKey, bson.M{key: bson.M{"$exists": true}})
	}
	filter := bson.M{"$or": anyKey}
	envelopes, err := job.Collection.CountDocuments(ctx, bson.M{"_ver": bson.M{"$exists": true}})
	if err != nil {
		return report, err
	}
	if envelopes > 0 {
		return report, fmt.Errorf("%w: %d document(s)", ErrCleanupRemoteEnvelope, envelopes)
	}
	var docs []struct {
		ID      int64  `bson:"_id"`
		Version *int64 `bson:"_version"`
		Deleted bool   `bson:"_deleted"`
	}
	if err := job.Collection.Find(ctx, filter, &docs); err != nil {
		return report, err
	}
	unset := bson.M{}
	for _, key := range job.Keys {
		unset[key] = ""
	}
	for _, doc := range docs {
		if doc.Deleted {
			report.Tombstones++
			continue
		}
		if doc.Version == nil {
			return report, fmt.Errorf("%w: document %d has no _version", ErrCleanupInvalid, doc.ID)
		}
		report.Matched++
		if job.DryRun {
			continue
		}
		result, err := job.Collection.UpdateOne(ctx,
			bson.M{"_id": doc.ID, "_version": *doc.Version, "_deleted": bson.M{"$ne": true}},
			bson.M{"$unset": unset, "$set": bson.M{"_version": *doc.Version + 1, "_last_tx": "stored-key-cleanup:" + job.Name}})
		if err != nil {
			return report, err
		}
		if result == nil || result.MatchedCount != 1 {
			return report, fmt.Errorf("%w: document %d changed after it was read (version %d)", ErrCleanupNotStopped, doc.ID, *doc.Version)
		}
		report.Cleaned++
	}
	return report, nil
}

func validateStoredKeyCleanup(job StoredKeyCleanup) error {
	if job.Collection == nil || job.Declared == nil || job.Name == "" || len(job.Keys) == 0 {
		return fmt.Errorf("%w: Name, Collection, Declared and Keys are required", ErrCleanupInvalid)
	}
	for _, key := range job.Keys {
		segments := strings.Split(key, ".")
		for _, segment := range segments {
			if segment == "" || segment == "*" || strings.ContainsAny(segment, "$\x00") {
				return fmt.Errorf("%w: %q is not a plain key path (no empty segment, *, $)", ErrCleanupInvalid, key)
			}
		}
		if strings.HasPrefix(key, "_") {
			return fmt.Errorf("%w: %q is framework metadata", ErrCleanupInvalid, key)
		}
		if job.Declared.Declares(key) {
			return fmt.Errorf("%w: %q is still declared by the DAO; only keys it no longer reads can be cleaned up", ErrCleanupInvalid, key)
		}
	}
	return nil
}
