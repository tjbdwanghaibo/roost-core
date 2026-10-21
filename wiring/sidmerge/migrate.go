package sidmerge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	engine "github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	coresaga "github.com/tjbdwanghaibo/roost-core/framework/saga"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// 迁移按 sid 独占的数据（设计 §6.1）：每个有 dbscope=sid DAO 的逻辑库，把 A 的 <物理名A>_<A> 里的每个集合
// 原样拷进 B 的 <物理名B>_<B>（两侧各按自己的 D9 映射换算），文档连同 _version / _schema / _last_tx 与墓碑
// 字段一字不改，B 冷加载时从这些元数据继续。不拷贝 DataEngine 自己的控制集合（_dataengine_*，在
// dataengine.database 里，outbox 另查）、不拷贝索引（B 启动时按 DAO 建）。
//
// 冲突（同一 _id 两边内容不同）整体停止、逐条报告，不覆盖、不跳过：在各服取同一常量 ID、或用按服序列发 ID
// 的文档会撞，由业务处置后重跑。重跑幂等：目标里已有、内容相同的文档记为 already_present，不再写。每一份迁入
// （或已在）的文档写进清单（Manifest），开服前核对与回退都按它。源库保留为只读归档，由运维确认后手动删除
// （维护者 2026-10-20 决定）。
//
// 中途失败（网络、权限）时已拷贝的文档留在 B：原样重跑即可补完，清单以最后一次成功的运行为准。

// Report 是 DryRun / Migrate 的结论。
type Report struct {
	From  int32 `json:"from"`
	Into  int32 `json:"into"`
	Ready bool  `json:"ready"`
	// DryRun 为 true 时没有写任何东西。
	DryRun bool    `json:"dry_run"`
	Checks []Check `json:"checks"`
	// ServerScoped 是要迁移的按 sid 分库与每个集合的文档数。
	ServerScoped []ScopedDatabase `json:"server_scoped_databases"`
	Conflicts    []Conflict       `json:"conflicts,omitempty"`
	// RemoteManaged 是落在按 sid 分库里的 remote=managed 集合（所有权元数据另按 Remote 规则处理）。
	RemoteManaged []string       `json:"remote_managed,omitempty"`
	Bus           []BusItem      `json:"bus"`
	Business      []BusinessItem `json:"business"`
	// Copied / AlreadyPresent 是这次运行写入的文档数与目标里本来就有的（重跑）文档数。
	Copied         int `json:"copied"`
	AlreadyPresent int `json:"already_present"`
}

// ScopedDatabase 是一个按 sid 分库的逻辑库在两侧的位置。
type ScopedDatabase struct {
	Logical     string            `json:"logical"`
	Source      string            `json:"source_database"`
	Target      string            `json:"target_database"`
	SourceMongo string            `json:"source_mongo"`
	TargetMongo string            `json:"target_mongo"`
	Collections []CollectionCount `json:"collections"`
}

// CollectionCount 是一个集合的扫描结果：ToCopy 目标里没有、要写的；AlreadyPresent 目标里已有且相同的；Conflicts 冲突数。
type CollectionCount struct {
	Name           string `json:"name"`
	Documents      int    `json:"documents"`
	ToCopy         int    `json:"to_copy"`
	AlreadyPresent int    `json:"already_present"`
	Conflicts      int    `json:"conflicts"`
}

// Conflict 是同一 _id 在源与目标里内容不同的一份文档。
type Conflict struct {
	Logical      string `json:"logical"`
	Collection   string `json:"collection"`
	ID           string `json:"_id"`
	SourceDigest string `json:"source_digest"`
	TargetDigest string `json:"target_digest"`
}

// Manifest 是一次迁移的清单：开服前按它核对，回退按它把文档搬回。
type Manifest struct {
	Version   int                `json:"version"`
	From      int32              `json:"from"`
	Into      int32              `json:"into"`
	CreatedAt time.Time          `json:"created_at"`
	Databases []ManifestDatabase `json:"databases"`
	Documents []ManifestDocument `json:"documents"`
}

// ManifestVersion 是清单格式的版本；Revert 只接受这个版本（线上未部署，不兼容旧格式）。
const ManifestVersion = 1

type ManifestDatabase struct {
	Logical     string `json:"logical"`
	Source      string `json:"source_database"`
	Target      string `json:"target_database"`
	SourceMongo string `json:"source_mongo"`
	TargetMongo string `json:"target_mongo"`
}

// ManifestDocument 是迁入（copied）或目标里已在（already_present，上次运行写入）的一份文档。ID 是 _id 的 canonical
// Extended JSON（{"_id": ...}），Digest 是内容摘要（与键顺序无关）。
type ManifestDocument struct {
	Logical    string `json:"logical"`
	Collection string `json:"collection"`
	ID         string `json:"_id"`
	Version    int64  `json:"_version,omitempty"`
	Digest     string `json:"digest"`
	Outcome    string `json:"outcome"`
}

const (
	OutcomeCopied         = "copied"
	OutcomeAlreadyPresent = "already_present"
)

// controlCollectionPrefix 是 DataEngine 自己的集合名前缀；按 sid 分库里出现也不拷贝。
const controlCollectionPrefix = "_dataengine_"

// DryRun 只读运行全部前置检查并扫描冲突，不写任何东西（migrate -dry-run）。有 fail 项时同时返回报告与 ErrNotReady。
func DryRun(ctx context.Context, plan Plan) (Report, error) {
	run, err := openRun(withContext(ctx), plan)
	if err != nil {
		return Report{}, err
	}
	defer run.close()
	return run.check(ctx)
}

// Migrate 先做与 DryRun 相同的检查，有 fail 项返回 ErrNotReady（什么也不写）；全部通过才逐集合拷贝，返回清单。
func Migrate(ctx context.Context, plan Plan) (Report, Manifest, error) {
	ctx = withContext(ctx)
	run, err := openRun(ctx, plan)
	if err != nil {
		return Report{}, Manifest{}, err
	}
	defer run.close()
	report, err := run.check(ctx)
	if err != nil {
		return report, Manifest{}, err
	}
	report.DryRun = false
	manifest := Manifest{Version: ManifestVersion, From: run.from.sid, Into: run.into.sid, CreatedAt: time.Now().UTC()}
	for _, database := range report.ServerScoped {
		manifest.Databases = append(manifest.Databases, ManifestDatabase{Logical: database.Logical, Source: database.Source,
			Target: database.Target, SourceMongo: database.SourceMongo, TargetMongo: database.TargetMongo})
		for _, collection := range database.Collections {
			documents, err := run.copyCollection(ctx, database, collection.Name)
			manifest.Documents = append(manifest.Documents, documents...)
			for _, document := range documents {
				if document.Outcome == OutcomeCopied {
					report.Copied++
				} else {
					report.AlreadyPresent++
				}
			}
			if err != nil {
				return report, manifest, fmt.Errorf("sidmerge: copy %s.%s (already copied documents stay in %s; re-run the same migrate to finish): %w",
					database.Source, collection.Name, database.Target, err)
			}
		}
	}
	return report, manifest, nil
}

// run 是一次运行持有的两侧事实与 Mongo 连接。
type run struct {
	plan                 Plan
	from, into           side
	fromMongo, intoMongo *mongo.Client
}

func openRun(ctx context.Context, plan Plan) (*run, error) {
	from, into, err := plan.resolve()
	if err != nil {
		return nil, err
	}
	r := &run{plan: plan, from: from, into: into}
	if r.fromMongo, err = connect(ctx, from.mongoURI); err != nil {
		return nil, fmt.Errorf("sidmerge: mongo of sid %d: %w", from.sid, err)
	}
	if r.intoMongo, err = connect(ctx, into.mongoURI); err != nil {
		r.close()
		return nil, fmt.Errorf("sidmerge: mongo of sid %d: %w", into.sid, err)
	}
	return r, nil
}

func connect(ctx context.Context, uri string) (*mongo.Client, error) {
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return nil, err
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, err
	}
	return client, nil
}

func (r *run) close() {
	for _, client := range []*mongo.Client{r.fromMongo, r.intoMongo} {
		if client != nil {
			_ = client.Disconnect(context.Background())
		}
	}
}

func (r *run) check(ctx context.Context) (Report, error) {
	report := Report{From: r.from.sid, Into: r.into.sid, DryRun: true, Bus: busChecklist(r.from), Business: businessChecklist}
	report.Checks = append(report.Checks, walCheck(ctx, r.from), walCheck(ctx, r.into))
	declared := r.plan.declared()
	if len(declared) == 0 {
		report.Checks = append(report.Checks, Check{Name: "databases", Status: StatusFail,
			Detail: "no logical database is declared in this binary: build the tool inside the project and import its db package"})
	}
	for _, database := range declared {
		check, err := r.coLocation(ctx, database)
		if err != nil {
			return report, err
		}
		report.Checks = append(report.Checks, check)
	}
	outbox, err := r.outboxCheck(ctx)
	if err != nil {
		return report, err
	}
	report.Checks = append(report.Checks, outbox)
	sagas, err := r.sagaChecks(ctx)
	if err != nil {
		return report, err
	}
	report.Checks = append(report.Checks, sagas...)
	report.Checks = append(report.Checks, busCheck(report.Bus))

	for _, database := range declared {
		if !database.ServerScoped {
			continue
		}
		scoped, conflicts, err := r.scan(ctx, database.Name)
		if err != nil {
			return report, err
		}
		report.ServerScoped = append(report.ServerScoped, scoped)
		report.Conflicts = append(report.Conflicts, conflicts...)
		for _, collection := range scoped.Collections {
			if slices.Contains(r.plan.RemoteManaged, database.Name+"/"+collection.Name) {
				report.RemoteManaged = append(report.RemoteManaged, database.Name+"/"+collection.Name)
			}
		}
	}
	conflicts := Check{Name: "conflicts", Status: StatusOK, Detail: "no _id is present on both sides with different content"}
	if len(report.Conflicts) > 0 {
		conflicts.Status = StatusFail
		conflicts.Detail = fmt.Sprintf("%d documents have the same _id on both sides with different content (listed under conflicts); "+
			"the business resolves them (renumber, drop one side) and the migrate runs again — nothing is overwritten or skipped", len(report.Conflicts))
	}
	report.Checks = append(report.Checks, conflicts)
	if len(report.RemoteManaged) > 0 {
		report.Checks = append(report.Checks, Check{Name: "remote-managed", Status: StatusConfirm,
			Detail: "remote=managed collections in sid-scoped databases: " + strings.Join(report.RemoteManaged, ", ") +
				"; their documents are copied, their ownership metadata (remote_entity) is not — handle it by the Remote rules"})
	}
	report.Ready = len(failed(report.Checks)) == 0
	return report, notReady(report.Checks)
}

// walCheck：两侧的 game 进程都已正常停机、WAL 全部投影确认，且配置里的库名映射与该进程最后一次运行记下的
// （databases.json）相同——否则这份配置不是那个进程实际用的，按它换算的库名不可信。
func walCheck(ctx context.Context, s side) Check {
	check := Check{Name: fmt.Sprintf("wal:%d", s.sid)}
	if s.walChecked {
		check.Status, check.Detail = StatusConfirm, "the WAL is on another machine; the operator confirmed sidmove wal-drained there ("+s.walDir+")"
		return check
	}
	if _, err := os.Stat(s.walDir); err != nil {
		check.Status = StatusFail
		check.Detail = fmt.Sprintf("WAL directory %s: %v; run the tool where the WAL is, pass its directory, or run sidmove wal-drained on that machine and pass the -*-wal-checked flag", s.walDir, err)
		return check
	}
	report, err := WALDrained(ctx, s.walDir)
	if err != nil {
		check.Status, check.Detail = StatusFail, fmt.Sprintf("%s: %s", s.walDir, firstNonEmpty(report.Reason, err.Error()))
		return check
	}
	if len(report.Databases) > 0 && !maps.Equal(report.Databases, s.databases.Physicals()) {
		check.Status = StatusFail
		check.Detail = fmt.Sprintf("%s is drained, but sid %d last ran with database mapping %v while this config maps %v; pass the config that sid actually ran with",
			s.walDir, s.sid, report.Databases, s.databases.Physicals())
		return check
	}
	check.Status, check.Detail = StatusOK, fmt.Sprintf("%s drained (%s)", s.walDir, firstNonEmpty(report.Reason, report.Checkpoint))
	return check
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// coLocation：非按 sid 的部分只有在两侧映射到同一 Mongo 上的同一物理库时，A 的文档才已经在 B 读得到的地方；
// 不同就拒绝合服并点名逻辑库（维护者 2026-10-20 决定，不做通用搬迁）。按 sid 分库的逻辑库也可能有不按 sid 的 DAO：
// 两侧基础库不同、且 A 的基础库里没有任何集合时，说明这部分没有数据，放行。
func (r *run) coLocation(ctx context.Context, database coredata.LogicalDatabase) (Check, error) {
	check := Check{Name: "co-location:" + database.Name}
	fromPhysical, intoPhysical := r.from.databases.Physical(database.Name), r.into.databases.Physical(database.Name)
	if r.from.mongo == r.into.mongo && strings.EqualFold(fromPhysical, intoPhysical) {
		check.Status, check.Detail = StatusOK, fmt.Sprintf("both map %q to %s on %s", database.Name, fromPhysical, r.from.mongo)
		return check, nil
	}
	where := fmt.Sprintf("sid %d maps %q to %s on %s, sid %d to %s on %s", r.from.sid, database.Name, fromPhysical, r.from.mongo, r.into.sid, intoPhysical, r.into.mongo)
	if database.ServerScoped {
		names, err := collectionNames(ctx, r.fromMongo.Database(fromPhysical))
		if err != nil {
			return check, err
		}
		if len(names) == 0 {
			check.Status, check.Detail = StatusOK, where+"; A's base database holds no collection, so only its dbscope=sid part (migrated) has data"
			return check, nil
		}
	}
	check.Status = StatusFail
	check.Detail = where + ": A's documents are not where B reads them. The merge is refused: map both to the same database on the same Mongo, or move the data first"
	return check, nil
}

// outboxCheck：停机时已暂存、未发布的 effect 留在 A 的 outbox 等下次启动，A 不再启动就没人发；outbox 认领不按
// sid 过滤，所以 A、B 共用同一个控制库时 B 启动后会发出它们。
func (r *run) outboxCheck(ctx context.Context) (Check, error) {
	check := Check{Name: fmt.Sprintf("outbox:%d", r.from.sid)}
	if r.from.mongo == r.into.mongo && r.from.controlDB == r.into.controlDB {
		check.Status, check.Detail = StatusOK, fmt.Sprintf("A and B share control database %s on %s: B publishes what A left in the outbox", r.from.controlDB, r.from.mongo)
		return check, nil
	}
	pending, err := r.fromMongo.Database(r.from.controlDB).Collection(engine.OutboxCollection).CountDocuments(ctx, bson.D{})
	if err != nil {
		return check, fmt.Errorf("sidmerge: count the outbox of sid %d: %w", r.from.sid, err)
	}
	if pending > 0 {
		check.Status = StatusFail
		check.Detail = fmt.Sprintf("%d effects wait in %s.%s and A and B do not share a control database: nobody would publish them. "+
			"Start A once more until its outbox drains (dataengine outbox backlog 0), then stop it and check again", pending, r.from.controlDB, engine.OutboxCollection)
		return check, nil
	}
	check.Status, check.Detail = StatusOK, fmt.Sprintf("%s.%s is empty", r.from.controlDB, engine.OutboxCollection)
	return check, nil
}

// nonTerminal 是 saga 的非终态。
var nonTerminal = []coresaga.Status{coresaga.StatusPending, coresaga.StatusWaiting, coresaga.StatusCompensating}

// sagaChecks：saga 状态里可能记着发起方的 sid 并按它路由（game-demo 赠礼的 FromSID），A 退役后指向 A 的非终态
// saga 无法推进，所以合服前全部 saga 必须是终态。saga 记录不带“属于哪个 sid”的字段，检查的是整个 saga 存储：
// 两侧存储相同只查一次。
func (r *run) sagaChecks(ctx context.Context) ([]Check, error) {
	stores := []side{r.from}
	if r.into.mongo != r.from.mongo || r.into.saga.Store.Database != r.from.saga.Store.Database || r.into.saga.Store.SagaCollection != r.from.saga.Store.SagaCollection {
		stores = append(stores, r.into)
	}
	var checks []Check
	for _, s := range stores {
		check := Check{Name: "saga:" + s.saga.Store.Database}
		records, err := listNonTerminalSagas(ctx, s)
		if err != nil {
			return nil, fmt.Errorf("sidmerge: saga records of sid %d: %w", s.sid, err)
		}
		if len(records) == 0 {
			check.Status, check.Detail = StatusOK, "every saga is terminal"
		} else {
			var sample []string
			for _, record := range records[:min(len(records), 10)] {
				sample = append(sample, fmt.Sprintf("%s(%s,%s)", record.ID, record.Type, record.Status))
			}
			more := ""
			if len(records) == 1000 {
				more = " (at least)"
			}
			check.Status = StatusFail
			check.Detail = fmt.Sprintf("%d%s sagas are not terminal, e.g. %s; let them finish (or resolve them by the saga admin surface) before the merge",
				len(records), more, strings.Join(sample, ", "))
		}
		checks = append(checks, check)
	}
	return checks, nil
}

func listNonTerminalSagas(ctx context.Context, s side) ([]coresaga.Record, error) {
	cfg := fmongo.DefaultConfig(s.mongoURI)
	cfg.RequireReplicaSet = false // 只读查询，不需要事务
	client, err := mongodriver.NewClient(cfg, mongodriver.IndexMigrationPolicy{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close(context.Background()) }()
	store, err := coresaga.NewMongoStore(client, s.saga.Store)
	if err != nil {
		return nil, err
	}
	return store.List(ctx, coresaga.Query{Statuses: nonTerminal, Limit: 1000})
}

// busCheck：发往 A 的持久消息无法由框架判断能否丢弃，清单里有要处理的项时记 confirm（runbook 第 2 步人工确认）。
func busCheck(items []BusItem) Check {
	var pending []string
	for _, item := range items {
		if item.Persistent && item.AddressedToSID && item.Action != "none" {
			pending = append(pending, item.Name)
		}
	}
	if len(pending) == 0 {
		return Check{Name: "bus", Status: StatusOK, Detail: "nothing persistent is addressed to A in this configuration (see bus)"}
	}
	return Check{Name: "bus", Status: StatusConfirm, Detail: "confirm by hand before the merge, as listed under bus: " + strings.Join(pending, ", ")}
}

func (r *run) scopedDatabase(logical string) ScopedDatabase {
	return ScopedDatabase{
		Logical:     logical,
		Source:      serverScopedDatabase(r.from.databases.Physical(logical), r.from.sid),
		Target:      serverScopedDatabase(r.into.databases.Physical(logical), r.into.sid),
		SourceMongo: r.from.mongo, TargetMongo: r.into.mongo,
	}
}

// scan 扫描一个按 sid 分库：每个集合的文档数、要拷的、已在的与冲突。只读。
func (r *run) scan(ctx context.Context, logical string) (ScopedDatabase, []Conflict, error) {
	scoped := r.scopedDatabase(logical)
	names, err := collectionNames(ctx, r.fromMongo.Database(scoped.Source))
	if err != nil {
		return scoped, nil, err
	}
	var conflicts []Conflict
	for _, name := range names {
		count := CollectionCount{Name: name}
		source := r.fromMongo.Database(scoped.Source).Collection(name)
		target := r.intoMongo.Database(scoped.Target).Collection(name)
		err := eachDocument(ctx, source, func(raw bson.Raw) error {
			count.Documents++
			outcome, sourceDigest, targetDigest, err := compare(ctx, target, raw)
			if err != nil {
				return err
			}
			switch outcome {
			case compareAbsent:
				count.ToCopy++
			case compareSame:
				count.AlreadyPresent++
			case compareDifferent:
				count.Conflicts++
				conflicts = append(conflicts, Conflict{Logical: logical, Collection: name, ID: idString(raw), SourceDigest: sourceDigest, TargetDigest: targetDigest})
			}
			return nil
		})
		if err != nil {
			return scoped, conflicts, fmt.Errorf("sidmerge: scan %s.%s: %w", scoped.Source, name, err)
		}
		scoped.Collections = append(scoped.Collections, count)
	}
	return scoped, conflicts, nil
}

// copyCollection 拷贝一个集合。逐份再比较一次：检查之后有人写了目标（不该发生：B 已停），遇到不同内容就停下。
func (r *run) copyCollection(ctx context.Context, scoped ScopedDatabase, name string) ([]ManifestDocument, error) {
	source := r.fromMongo.Database(scoped.Source).Collection(name)
	target := r.intoMongo.Database(scoped.Target).Collection(name)
	var documents []ManifestDocument
	err := eachDocument(ctx, source, func(raw bson.Raw) error {
		outcome, sourceDigest, targetDigest, err := compare(ctx, target, raw)
		if err != nil {
			return err
		}
		document := ManifestDocument{Logical: scoped.Logical, Collection: name, ID: idString(raw), Version: version(raw), Digest: sourceDigest}
		switch outcome {
		case compareDifferent:
			return fmt.Errorf("_id %s appeared in %s with different content (digest %s) after the check; is B running?", document.ID, scoped.Target, targetDigest)
		case compareSame:
			document.Outcome = OutcomeAlreadyPresent
		case compareAbsent:
			if _, err := target.InsertOne(ctx, raw); err != nil {
				return fmt.Errorf("insert _id %s: %w", document.ID, err)
			}
			document.Outcome = OutcomeCopied
		}
		documents = append(documents, document)
		return nil
	})
	return documents, err
}

type compareOutcome int

const (
	compareAbsent compareOutcome = iota
	compareSame
	compareDifferent
)

func compare(ctx context.Context, target *mongo.Collection, raw bson.Raw) (compareOutcome, string, string, error) {
	sourceDigest, err := Digest(raw)
	if err != nil {
		return 0, "", "", err
	}
	var existing bson.Raw
	err = target.FindOne(ctx, bson.D{{Key: "_id", Value: raw.Lookup("_id")}}).Decode(&existing)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return compareAbsent, sourceDigest, "", nil
	}
	if err != nil {
		return 0, "", "", err
	}
	targetDigest, err := Digest(existing)
	if err != nil {
		return 0, "", "", err
	}
	if targetDigest == sourceDigest {
		return compareSame, sourceDigest, targetDigest, nil
	}
	return compareDifferent, sourceDigest, targetDigest, nil
}

func eachDocument(ctx context.Context, collection *mongo.Collection, visit func(bson.Raw) error) error {
	cursor, err := collection.Find(ctx, bson.D{}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return err
	}
	defer func() { _ = cursor.Close(context.Background()) }()
	for cursor.Next(ctx) {
		raw := append(bson.Raw(nil), cursor.Current...)
		if err := visit(raw); err != nil {
			return err
		}
	}
	return cursor.Err()
}

// collectionNames 是一个库里要处理的集合（排序）：不含 system.* 与 DataEngine 自己的控制集合。
func collectionNames(ctx context.Context, database *mongo.Database) ([]string, error) {
	names, err := database.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		return nil, fmt.Errorf("sidmerge: list collections of %s: %w", database.Name(), err)
	}
	names = slices.DeleteFunc(names, func(name string) bool {
		return strings.HasPrefix(name, "system.") || strings.HasPrefix(name, controlCollectionPrefix)
	})
	slices.Sort(names)
	return names, nil
}

// Digest 是一份文档内容的摘要：递归按键排序后的 BSON 的 SHA-256 前 16 字节（十六进制）。与字段顺序无关，所以
// 原样拷贝前后、两次读出的同一份文档摘要相同。
func Digest(raw bson.Raw) (string, error) {
	var document bson.D
	if err := bson.Unmarshal(raw, &document); err != nil {
		return "", err
	}
	encoded, err := bson.Marshal(canonical(document))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:16]), nil
}

func canonical(value any) any {
	switch typed := value.(type) {
	case bson.D:
		ordered := slices.Clone(typed)
		slices.SortFunc(ordered, func(a, b bson.E) int { return strings.Compare(a.Key, b.Key) })
		for i := range ordered {
			ordered[i].Value = canonical(ordered[i].Value)
		}
		return ordered
	case bson.M:
		ordered := make(bson.D, 0, len(typed))
		for _, key := range slices.Sorted(maps.Keys(typed)) {
			ordered = append(ordered, bson.E{Key: key, Value: canonical(typed[key])})
		}
		return ordered
	case bson.A:
		out := make(bson.A, len(typed))
		for i, item := range typed {
			out[i] = canonical(item)
		}
		return out
	}
	return value
}

// idString 是 _id 的 canonical Extended JSON（{"_id": ...}），清单里用它记文档，回退时解析回来。
func idString(raw bson.Raw) string {
	encoded, err := bson.MarshalExtJSON(bson.D{{Key: "_id", Value: raw.Lookup("_id")}}, true, false)
	if err != nil {
		return fmt.Sprintf("<unencodable _id: %v>", err)
	}
	return string(encoded)
}

func idFilter(id string) (bson.D, error) {
	var document bson.Raw
	if err := bson.UnmarshalExtJSON([]byte(id), true, &document); err != nil {
		return nil, fmt.Errorf("manifest _id %s: %w", id, err)
	}
	value, err := document.LookupErr("_id")
	if err != nil {
		return nil, fmt.Errorf("manifest _id %s: %w", id, err)
	}
	return bson.D{{Key: "_id", Value: value}}, nil
}

func version(raw bson.Raw) int64 {
	value, err := raw.LookupErr("_version")
	if err != nil {
		return 0
	}
	if number, ok := value.AsInt64OK(); ok {
		return number
	}
	return 0
}
