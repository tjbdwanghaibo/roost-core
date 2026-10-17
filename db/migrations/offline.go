package migrations

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/migration"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 离线迁移执行器：在**已停写**的集合上把一个 DAO 的文档从 From 转到 To。
//
// 它只做框架不做的那一段（转换旧文档），其余边界沿用 DataEngine 的现行规则：
//   - 停写由运维保证：所有可能写这个集合的进程已经按正常停机流程退出（DataEngine 停机先把 WAL 全部
//     投影进 Mongo），新程序还没启动。执行器不能替你停写，但会发现“没停干净”：每次写回都带
//     `_version` 与 `_schema` 条件，条件不成立就整次中止。
//   - 写回把 `_version` 加 1、`_last_tx` 记为 "schema-migration:<Name>"：旧程序残留的条件写
//     （按旧 `_version`）因此必然冲突，而不是悄悄把旧 schema 写回去；新程序从加 1 后的版本继续。
//   - 墓碑（`_deleted: true`）不转换：冷加载把墓碑当作不存在，不检查它的 schema。
//   - Remote 版本信封（带 `_ver` 的文档）拒绝：Remote 托管实体的版本、所有权与 fence 由 Remote
//     链路维护，不能用这个执行器改写。
//   - 出现 From、To 之外的 schema（更旧的、未来的）拒绝，不猜。
//
// 每份文档的原样副本先写进 Backup 集合再改写，所以 Rollback 可以在新程序写入之前原样恢复；
// 中断后重跑 Apply 会从剩下的文档继续（已备份、已转换的按幂等处理）。

// Document 是交给 migration.Step 的一份文档：Fields 是可改写的字段，版本取自 `_schema`。
type Document struct {
	Fields  bson.M
	version int32
}

func (d *Document) DataVersion() int32     { return d.version }
func (d *Document) SetDataVersion(v int32) { d.version = v }

var _ migration.Versioned = (*Document)(nil)

// Job 是一次集合迁移。
type Job struct {
	// Name 进入 `_last_tx` 标记，用来区分“迁移写的版本”和“业务写的版本”。
	Name string
	// Collection 是被迁移的集合；Backup 保存每份被改写文档的原样副本（建议单独的集合名，例如
	// player_schema1_backup），Rollback 从它恢复。
	Collection fmongo.ICollection
	Backup     fmongo.ICollection
	Steps      *migration.Registry
	From, To   int32
	// Check 在写入前校验每份转换结果；返回错误时 Apply 一份都不写。
	Check func(before, after bson.M) error
	// BatchSize 是每次读取的文档数，0 取 200。
	BatchSize int64
}

// Report 是一次执行的结果；JSON 字段供 cmd/schemamigrate 打印。
type Report struct {
	Mode       string `json:"mode"`
	Collection string `json:"collection,omitempty"`
	From       int32  `json:"from"`
	To         int32  `json:"to"`
	// Pending 是仍处于 From 的文档数；Apply 成功后为 0。
	Pending int `json:"pending"`
	// AtTarget 是已经处于 To 的活文档数（含本次转换的）。
	AtTarget   int `json:"at_target"`
	Tombstones int `json:"tombstones"`
	// Converted 是本次转换并通过 Check 的份数（plan 只转换不写）。
	Converted int `json:"converted"`
	// Written 是本次写回的份数；Resumed 是此前中断时已经写好、本次按幂等跳过的份数。
	Written int `json:"written"`
	Resumed int `json:"resumed"`
	// RolledBack 与 Refused 只用于 rollback：Refused 是迁移后已被业务写过、不能原样恢复的文档。
	RolledBack int      `json:"rolled_back,omitempty"`
	Refused    []string `json:"refused,omitempty"`
}

// ErrNotStopped 表示执行期间有别的写入者改了文档：停写没有做完，整次中止。
var ErrNotStopped = errors.New("schema migration: the collection is still being written; stop every writer first")

// ErrUnexpectedDocument 表示集合里有执行器不处理的文档（未知 schema、Remote 信封、坏元数据）。
var ErrUnexpectedDocument = errors.New("schema migration: unexpected document")

func (job Job) marker() string { return "schema-migration:" + job.Name }

func (job Job) validate() error {
	switch {
	case job.Name == "" || job.Collection == nil || job.Steps == nil || job.Check == nil:
		return errors.New("schema migration: name, collection, steps and check are required")
	case job.From <= 0 || job.To <= job.From:
		return fmt.Errorf("schema migration: invalid versions %d -> %d", job.From, job.To)
	}
	return nil
}

// Plan 是 dry-run：读取全部文档、转换并校验，不写任何东西。Apply 写入前也先完整跑一遍它。
func (job Job) Plan(ctx context.Context) (Report, error) {
	report := Report{Mode: "plan", From: job.From, To: job.To}
	if err := job.validate(); err != nil {
		return report, err
	}
	err := job.scan(ctx, &report, func(doc bson.M) error {
		_, err := job.convert(ctx, doc)
		if err == nil {
			report.Converted++
		}
		return err
	})
	return report, err
}

// Apply 先完整 Plan（任何一份转换或校验失败都不写），再逐份“备份 → 条件改写”。
func (job Job) Apply(ctx context.Context) (Report, error) {
	if job.Backup == nil {
		return Report{Mode: "apply"}, errors.New("schema migration: a backup collection is required to apply")
	}
	plan, err := job.Plan(ctx)
	if err != nil {
		plan.Mode = "apply"
		return plan, fmt.Errorf("dry run refused, nothing written: %w", err)
	}
	report := Report{Mode: "apply", From: job.From, To: job.To, Converted: plan.Converted}
	err = job.scan(ctx, &report, func(doc bson.M) error {
		after, err := job.convert(ctx, doc)
		if err != nil {
			return err
		}
		resumed, err := job.write(ctx, doc, after)
		if err != nil {
			return err
		}
		if resumed {
			report.Resumed++
		} else {
			report.Written++
		}
		return nil
	})
	if err != nil {
		return report, err
	}
	// 写完再数一遍：新程序启动前，集合里不能还有 From 版本的活文档。
	final, err := job.Verify(ctx)
	report.Pending, report.AtTarget, report.Tombstones = final.Pending, final.AtTarget, final.Tombstones
	return report, err
}

// Verify 是切换版本前的验收：没有 From 版本的活文档，每份 To 版本的活文档都能被 Check 接受
// （对已经是 To 的文档，before 与 after 相同）。
func (job Job) Verify(ctx context.Context) (Report, error) {
	report := Report{Mode: "verify", From: job.From, To: job.To}
	if err := job.validate(); err != nil {
		return report, err
	}
	var failures []error
	err := job.each(ctx, func(doc bson.M) error {
		if deleted, _ := doc["_deleted"].(bool); deleted {
			report.Tombstones++
			return nil
		}
		schema, err := job.schemaOf(doc)
		if err != nil {
			return err
		}
		if schema == job.From {
			report.Pending++
			return nil
		}
		report.AtTarget++
		if err := job.Check(doc, doc); err != nil {
			failures = append(failures, fmt.Errorf("_id=%v: %w", doc["_id"], err))
		}
		return nil
	})
	if err != nil {
		return report, err
	}
	if report.Pending != 0 {
		failures = append(failures, fmt.Errorf("%d documents are still at schema %d", report.Pending, job.From))
	}
	return report, errors.Join(failures...)
}

// Rollback 用备份原样恢复迁移写过的文档。只恢复“仍是迁移写的那一版”的文档：迁移之后被业务写过的
// 文档（`_version` 或 `_last_tx` 已变）列进 Refused，不覆盖——那会丢掉之后的写入，需要业务决定。
func (job Job) Rollback(ctx context.Context) (Report, error) {
	report := Report{Mode: "rollback", From: job.From, To: job.To}
	if err := job.validate(); err != nil {
		return report, err
	}
	if job.Backup == nil {
		return report, errors.New("schema migration: a backup collection is required to roll back")
	}
	var backups []bson.M
	if err := job.Backup.Find(ctx, bson.M{}, &backups, fmongo.FindOption{Sort: bson.D{{Key: "_id", Value: 1}}}); err != nil {
		return report, err
	}
	for _, original := range backups {
		version, ok := toInt64(original["_version"])
		if !ok {
			return report, fmt.Errorf("%w: backup _id=%v has no _version", ErrUnexpectedDocument, original["_id"])
		}
		filter := bson.M{"_id": original["_id"], "_schema": job.To, "_version": version + 1, "_last_tx": job.marker()}
		result, err := job.Collection.ReplaceOne(ctx, filter, original)
		if err != nil {
			return report, err
		}
		if result.MatchedCount == 1 {
			report.RolledBack++
			continue
		}
		var current bson.M
		if err := job.Collection.FindOne(ctx, bson.M{"_id": original["_id"]}, &current); err != nil {
			return report, fmt.Errorf("rollback _id=%v: %w", original["_id"], err)
		}
		if sameDocument(current, original) {
			report.Resumed++ // 上一次回退已经恢复了它
			continue
		}
		report.Refused = append(report.Refused, fmt.Sprintf("_id=%v _version=%v _last_tx=%v", current["_id"], current["_version"], current["_last_tx"]))
	}
	if len(report.Refused) != 0 {
		return report, fmt.Errorf("schema migration: %d documents were written after the migration and were not rolled back", len(report.Refused))
	}
	return report, nil
}

// convert 用登记的步骤把一份 From 文档转成 To，并补上迁移自己的元数据，最后交给 Check。
func (job Job) convert(ctx context.Context, before bson.M) (bson.M, error) {
	fields := bson.M{}
	for key, value := range before {
		fields[key] = value
	}
	document := &Document{Fields: fields, version: job.From}
	if err := job.Steps.RunFrom(ctx, document, job.From, job.To); err != nil {
		return nil, fmt.Errorf("_id=%v: %w", before["_id"], err)
	}
	version, _ := toInt64(before["_version"])
	after := document.Fields
	after["_id"] = before["_id"]
	after["_schema"] = int64(job.To)
	after["_version"] = version + 1
	after["_last_tx"] = job.marker()
	if err := job.Check(before, after); err != nil {
		return nil, fmt.Errorf("_id=%v: check: %w", before["_id"], err)
	}
	return after, nil
}

// write 备份一份 From 文档并条件改写成 after。resumed=true 表示上一次中断前已经写好。
func (job Job) write(ctx context.Context, before, after bson.M) (resumed bool, err error) {
	if _, err := job.Backup.InsertOne(ctx, before); err != nil {
		if !errors.Is(err, fmongo.ErrDuplicateKey) {
			return false, fmt.Errorf("backup _id=%v: %w", before["_id"], err)
		}
		var saved bson.M
		if err := job.Backup.FindOne(ctx, bson.M{"_id": before["_id"]}, &saved); err != nil {
			return false, fmt.Errorf("backup _id=%v: %w", before["_id"], err)
		}
		if !sameDocument(saved, before) {
			return false, fmt.Errorf("%w: backup _id=%v differs from the document now in the collection", ErrNotStopped, before["_id"])
		}
	}
	filter := bson.M{"_id": before["_id"], "_version": before["_version"], "_schema": before["_schema"]}
	result, err := job.Collection.ReplaceOne(ctx, filter, after)
	if err != nil {
		return false, fmt.Errorf("rewrite _id=%v: %w", before["_id"], err)
	}
	if result.MatchedCount == 1 {
		return false, nil
	}
	var current bson.M
	if err := job.Collection.FindOne(ctx, bson.M{"_id": before["_id"]}, &current); err != nil {
		return false, fmt.Errorf("rewrite _id=%v: %w", before["_id"], err)
	}
	if current["_last_tx"] == job.marker() && sameNumber(current["_version"], after["_version"]) {
		return true, nil
	}
	return false, fmt.Errorf("%w: _id=%v moved to _version=%v _last_tx=%v while migrating", ErrNotStopped, before["_id"], current["_version"], current["_last_tx"])
}

// scan 按 _id 分批读取全部文档，统计各状态，并把 From 版本的活文档交给 visit。
func (job Job) scan(ctx context.Context, report *Report, visit func(bson.M) error) error {
	report.Pending, report.AtTarget, report.Tombstones = 0, 0, 0
	return job.each(ctx, func(doc bson.M) error {
		if deleted, _ := doc["_deleted"].(bool); deleted {
			report.Tombstones++
			return nil
		}
		schema, err := job.schemaOf(doc)
		if err != nil {
			return err
		}
		if schema == job.To {
			report.AtTarget++
			return nil
		}
		report.Pending++
		return visit(doc)
	})
}

func (job Job) schemaOf(doc bson.M) (int32, error) {
	if _, enveloped := doc["_ver"]; enveloped {
		return 0, fmt.Errorf("%w: _id=%v is a Remote version envelope", ErrUnexpectedDocument, doc["_id"])
	}
	schema, ok := toInt64(doc["_schema"])
	if !ok {
		return 0, fmt.Errorf("%w: _id=%v has no numeric _schema", ErrUnexpectedDocument, doc["_id"])
	}
	if _, ok := toInt64(doc["_version"]); !ok {
		return 0, fmt.Errorf("%w: _id=%v has no numeric _version", ErrUnexpectedDocument, doc["_id"])
	}
	if schema != int64(job.From) && schema != int64(job.To) {
		return 0, fmt.Errorf("%w: _id=%v is at schema %d, this job only knows %d and %d", ErrUnexpectedDocument, doc["_id"], schema, job.From, job.To)
	}
	return int32(schema), nil
}

func (job Job) each(ctx context.Context, visit func(bson.M) error) error {
	batch := job.BatchSize
	if batch <= 0 {
		batch = 200
	}
	filter := bson.M{}
	for {
		var docs []bson.M
		if err := job.Collection.Find(ctx, filter, &docs, fmongo.FindOption{Sort: bson.D{{Key: "_id", Value: 1}}, Limit: batch}); err != nil {
			return err
		}
		for _, doc := range docs {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := visit(doc); err != nil {
				return err
			}
		}
		if int64(len(docs)) < batch {
			return nil
		}
		filter = bson.M{"_id": bson.M{"$gt": docs[len(docs)-1]["_id"]}}
	}
}

// sameDocument 比较两份文档的内容（经 BSON 往返，数字类型差异不影响结果）。
func sameDocument(left, right bson.M) bool {
	normalize := func(doc bson.M) bson.M {
		raw, err := bson.Marshal(doc)
		if err != nil {
			return nil
		}
		var out bson.M
		if bson.Unmarshal(raw, &out) != nil {
			return nil
		}
		return out
	}
	a, b := normalize(left), normalize(right)
	return a != nil && b != nil && reflect.DeepEqual(a, b)
}

func sameNumber(left, right any) bool {
	a, okA := toInt64(left)
	b, okB := toInt64(right)
	return okA && okB && a == b
}
