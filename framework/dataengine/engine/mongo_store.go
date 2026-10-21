package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// DataEngine 在默认库中使用的系统集合：事务去重标记、outbox 暂存与业务回执。
const (
	TransactionCollection = "_dataengine_transactions"
	OutboxCollection      = "_dataengine_outbox"
	ReceiptCollection     = "_dataengine_receipts"
)

var (
	ErrProjectionConflict         = errors.New("dataengine mongo: fatal projection version conflict")
	ErrTransactionIdentity        = errors.New("dataengine mongo: transaction identity conflict")
	ErrReceiptIdentity            = errors.New("dataengine mongo: receipt identity conflict")
	ErrRemoteProjection           = errors.New("dataengine mongo: remote mutation requires remote projector")
	errProjectionBatchUnsupported = errors.New("dataengine mongo: projection batch is unsupported")

	// ErrProjectionBatchNeedsPerRecord 要求投影器把批次退回逐条重投。批量写无法知道
	// 哪条记录没匹配上，而“没匹配”本身有歧义：对新记录是真实版本冲突，对 WAL ack 丢失后
	// 重放的已应用记录看起来完全一样。只有单条路径（比较已存版本与 _last_tx）能区分二者，
	// 所以批次不猜测、直接退回；猜成“冲突”会在无害重放时 fence 整个进程。
	ErrProjectionBatchNeedsPerRecord = errors.New("dataengine mongo: projection batch must be retried per record")
)

// MongoStoreConfig 配置投影存储：默认库、逻辑库映射、服务器分库 ID 与两类回执的保留期。
type MongoStoreConfig struct {
	DefaultDatabase string
	// Databases 是启动时冻结的逻辑库 → 物理库表（D9 修订版 A）。DocumentKey.Database 是 DAO 的逻辑库名，
	// 投影、冷加载与墓碑都经它换成物理库名；表里没有的逻辑库物理名就是逻辑名本身（identity）。
	// nil 不等于 identity：带库名的 DAO 文档一律返回 coredata.ErrDatabaseMapNotBound（与 Remote 存储同一条规则），
	// 进程里有声明的逻辑库时 NewMongoStore 直接拒绝。wiring 传 DataEngine Mod 冻结的表；自建装配用
	// dataengine.NewDatabaseMap 合成（不配映射时 DatabaseMapConfig{Declared: dataengine.DeclaredDatabases()}）。
	Databases             *coredata.DatabaseMap
	ServerID              int32
	TransactionReceiptTTL time.Duration
	ReceiptTTL            time.Duration
}

const defaultReceiptTTL = 30 * 24 * time.Hour

// MongoStore 以精确的版本条件把 WAL mutation 投影到 Mongo。单条普通 mutation 走单文档
// 快路；涉及多文档、effect 或回执的事务在一个 Mongo 会话事务内完成。
type MongoStore struct {
	client        fmongo.IMongo
	cfg           MongoStoreConfig
	now           func() time.Time
	remoteStore   RemoteProjectionStore
	remoteApplier entity.RemoteCommitApplier

	// afterLeaseFence 是测试缝：租约校验通过、业务写入之前在同一 Mongo 事务里调用，
	// 用来在确定的位置插入并发的租约接管（写偏斜回归）。生产为 nil。
	afterLeaseFence func(context.Context)
}

// RemoteProjectionStore 在调用方的 Mongo 事务内应用 Remote 提交并返回回执。
type RemoteProjectionStore interface {
	ApplyRemoteCommitsInTransaction(context.Context, []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error)
}

// SetRemoteProjection 绑定 Remote 投影所需的远端存储与提交应用器；二者必须同时提供。
//
// 远端存储要写 DAO 文档（实现 coredata.DatabaseMapBinder，例如 remoteentity 的 MongoCommitter 与 Backend）时，
// 在这里把本存储的映射表绑过去：自建装配只给 DataEngine 一张表，Remote 写 DAO 文档与投影就用同一张，wiring
// 也只经这一处绑定。远端存储已绑了另一张表时返回错误，不静默覆盖，也不接上投影；本存储没有表时不绑，
// 两边都拒绝写带库名的 DAO 文档（coredata.ErrDatabaseMapNotBound）。
func (store *MongoStore) SetRemoteProjection(remoteStore RemoteProjectionStore, applier entity.RemoteCommitApplier) error {
	if store == nil || remoteStore == nil || applier == nil {
		return errors.New("dataengine mongo: remote store and applier are required")
	}
	if binder, ok := remoteStore.(coredata.DatabaseMapBinder); ok && store.cfg.Databases != nil {
		if err := binder.BindDatabaseMap(store.cfg.Databases); err != nil {
			return fmt.Errorf("dataengine mongo: bind the database map to the remote store: %w", err)
		}
	}
	store.remoteStore, store.remoteApplier = remoteStore, applier
	return nil
}

func NewMongoStore(client fmongo.IMongo, cfg MongoStoreConfig) (*MongoStore, error) {
	if client == nil {
		return nil, errors.New("dataengine mongo: client is required")
	}
	cfg.DefaultDatabase = strings.TrimSpace(cfg.DefaultDatabase)
	if cfg.DefaultDatabase == "" {
		return nil, errors.New("dataengine mongo: default database is required")
	}
	if cfg.TransactionReceiptTTL <= 0 {
		cfg.TransactionReceiptTTL = defaultReceiptTTL
	}
	if cfg.ReceiptTTL <= 0 {
		cfg.ReceiptTTL = defaultReceiptTTL
	}
	// 规则在装配时强制一次：进程链接了声明逻辑库的 DAO（生成的 gen_databases.go 在 init 里登记）却没给表，
	// 第一次投影必然失败，而那时记录已进 WAL、只能卡在投影上，所以在构造时就拒绝。没有声明的进程（手写 DAO、
	// 只用控制库的工具）不在这里拦，由 collection 在访问时拒绝。
	if cfg.Databases == nil {
		if declared := coredata.DeclaredDatabases(); len(declared) > 0 {
			names := make([]string, len(declared))
			for i := range declared {
				names[i] = declared[i].Name
			}
			return nil, fmt.Errorf("%w: this process declares logical databases %s but MongoStoreConfig.Databases is nil; %s",
				coredata.ErrDatabaseMapNotBound, strings.Join(names, ", "), databaseMapHint)
		}
	}
	return &MongoStore{client: client, cfg: cfg, now: time.Now}, nil
}

// EnsureInfrastructure 确保系统集合的 TTL 与认领索引存在。
func (store *MongoStore) EnsureInfrastructure(ctx context.Context) error {
	if store == nil || store.client == nil {
		return errors.New("dataengine mongo: store is not configured")
	}
	transactionTTL, err := ttlSeconds(store.cfg.TransactionReceiptTTL)
	if err != nil {
		return err
	}
	db := store.client.Database(store.cfg.DefaultDatabase)
	if err := db.Collection(TransactionCollection).EnsureIndexes(ctx, []fmongo.IndexModel{{
		Keys: bson.D{{Key: "created_at", Value: 1}}, Name: "ttl_created_at", TTL: transactionTTL,
	}}); err != nil {
		return err
	}
	if err := db.Collection(ReceiptCollection).EnsureIndexes(ctx, []fmongo.IndexModel{{
		Keys: bson.D{{Key: "expires_at", Value: 1}}, Name: "ttl_expires_at", ExpireAt: true, RecreateOnConflict: true,
	}}); err != nil {
		return err
	}
	return db.Collection(OutboxCollection).EnsureIndexes(ctx, []fmongo.IndexModel{
		{Keys: bson.D{{Key: "available_at", Value: 1}, {Key: "lease_until", Value: 1}}, Name: "claim_due"},
		{Keys: bson.D{{Key: "effect_id", Value: 1}}, Name: "uniq_effect", Unique: true},
		// 积压采样要读最老的暂存 effect；没有这个索引，排序会扫全集合，
		// 采样开销恰好随它要测量的积压线性增长。
		{Keys: bson.D{{Key: "created_at", Value: 1}}, Name: "backlog_oldest"},
	})
}

func ttlSeconds(ttl time.Duration) (int32, error) {
	seconds := int64(ttl / time.Second)
	if seconds <= 0 || seconds > int64(^uint32(0)>>1) {
		return 0, fmt.Errorf("dataengine mongo: invalid ttl %s", ttl)
	}
	return int32(seconds), nil
}

func (store *MongoStore) applyMutation(ctx context.Context, txID string, mutation coredata.Mutation, inTransaction bool) error {
	if mutation.Remote != nil {
		return ErrRemoteProjection
	}
	coll, err := store.collection(mutation.Key)
	if err != nil {
		return err
	}
	switch mutation.Kind {
	case coredata.MutationPut:
		return store.applyPut(ctx, coll, txID, mutation, inTransaction)
	case coredata.MutationPatch:
		return store.applyPatch(ctx, coll, txID, mutation)
	case coredata.MutationDelete:
		return store.applyDelete(ctx, coll, txID, mutation)
	default:
		return coredata.ErrInvalidMutationKind
	}
}

func (store *MongoStore) applyPut(ctx context.Context, coll fmongo.ICollection, txID string, mutation coredata.Mutation, inTransaction bool) error {
	var doc bson.M
	if err := bson.Unmarshal(mutation.Data, &doc); err != nil {
		return err
	}
	if id, ok := documentInt64(doc["_id"]); !ok || id != mutation.Key.ID {
		return coredata.ErrInvalidDocumentKey
	}
	doc["_id"] = mutation.Key.ID
	doc["_version"] = mutation.NextVersion
	doc["_schema"] = mutation.Schema
	doc["_last_tx"] = txID
	delete(doc, "_deleted")
	delete(doc, "_deleted_at")
	// 聚合管道更新是真正的整文档替换且支持 upsert；与 $set 不同，不会残留已删除的字段。
	pipeline := bson.A{bson.M{"$replaceWith": doc}}
	var after bson.M
	err := coll.FindOneAndUpdate(ctx, mutationFilter(mutation), pipeline, &after, fmongo.FindOneAndUpdateOption{Upsert: true, ReturnAfter: true})
	if err == nil {
		return nil
	}
	// 多文档路径已在写入前检查持久事务 marker。重复键会中止 Mongo
	// transaction，不能继续在这个 session 中查询；此时缺少 marker 的 Put
	// 不属于已提交重放，应直接报告冲突并让整个事务回滚。
	if inTransaction && errors.Is(err, fmongo.ErrDuplicateKey) {
		return fmt.Errorf("%w: Put duplicate key %s/%s/%d expected=%d next=%d", ErrProjectionConflict,
			mutation.Key.Database, mutation.Key.Resource, mutation.Key.ID, mutation.ExpectedVersion, mutation.NextVersion)
	}
	if errors.Is(err, fmongo.ErrDuplicateKey) || errors.Is(err, fmongo.ErrNotFound) {
		return store.classifyNoMatch(ctx, coll, txID, mutation)
	}
	return err
}

func (store *MongoStore) applyPatch(ctx context.Context, coll fmongo.ICollection, txID string, mutation coredata.Mutation) error {
	update, err := patchUpdate(txID, mutation)
	if err != nil {
		return err
	}
	result, err := coll.UpdateOne(ctx, mutationFilter(mutation), update)
	if err != nil {
		return err
	}
	if result != nil && result.MatchedCount == 1 {
		return nil
	}
	return store.classifyNoMatch(ctx, coll, txID, mutation)
}

func (store *MongoStore) applyDelete(ctx context.Context, coll fmongo.ICollection, txID string, mutation coredata.Mutation) error {
	update := bson.M{"$set": bson.M{
		"_version":    mutation.NextVersion,
		"_schema":     mutation.Schema,
		"_last_tx":    txID,
		"_deleted":    true,
		"_deleted_at": store.now().UTC(),
	}}
	result, err := coll.UpdateOne(ctx, mutationFilter(mutation), update)
	if err != nil {
		return err
	}
	if result != nil && result.MatchedCount == 1 {
		return nil
	}
	return store.classifyNoMatch(ctx, coll, txID, mutation)
}

func mutationFilter(mutation coredata.Mutation) bson.M {
	if mutation.ExpectedVersion == 0 {
		return bson.M{"_id": mutation.Key.ID, "_version": bson.M{"$exists": false}}
	}
	return bson.M{"_id": mutation.Key.ID, "_version": mutation.ExpectedVersion}
}

func patchUpdate(txID string, mutation coredata.Mutation) (bson.M, error) {
	set := bson.M{"_version": mutation.NextVersion, "_schema": mutation.Schema, "_last_tx": txID}
	if len(mutation.Patch.SetBSON) != 0 {
		raw := bson.Raw(mutation.Patch.SetBSON)
		elements, err := raw.Elements()
		if err != nil {
			return nil, err
		}
		for _, element := range elements {
			var value any
			wrapped, err := bson.Marshal(bson.D{{Key: "v", Value: element.Value()}})
			if err != nil {
				return nil, err
			}
			var decoded bson.M
			if err := bson.Unmarshal(wrapped, &decoded); err != nil {
				return nil, err
			}
			value = decoded["v"]
			set[element.Key()] = value
		}
	}
	unset := bson.M{"_deleted": "", "_deleted_at": ""}
	for _, path := range mutation.Patch.Unset {
		unset[path] = ""
	}
	return bson.M{"$set": set, "$unset": unset}, nil
}

// projectionMeta 是判定“已应用重放还是版本冲突”所需的文档元数据。
type projectionMeta struct {
	Version uint64 `bson:"_version"`
	LastTx  string `bson:"_last_tx"`
}

// classifyNoMatch 在版本条件未命中时回读文档：版本与 _last_tx 都等于本事务即幂等重放，
// 否则是 fatal 的 ErrProjectionConflict。
func (store *MongoStore) classifyNoMatch(ctx context.Context, coll fmongo.ICollection, txID string, mutation coredata.Mutation) error {
	var meta projectionMeta
	err := coll.FindOne(ctx, bson.M{"_id": mutation.Key.ID}, &meta)
	if err == nil && meta.Version == mutation.NextVersion && meta.LastTx == txID {
		return nil
	}
	if err != nil && !errors.Is(err, fmongo.ErrNotFound) {
		return err
	}
	physical, _ := store.physicalDatabase(mutation.Key) // 已经解析过（coll 由它得来），这里只用于报错
	return fmt.Errorf("%w: %s/%s/%d (physical database %s) expected=%d next=%d stored=%d", ErrProjectionConflict,
		mutation.Key.Database, mutation.Key.Resource, mutation.Key.ID, physical, mutation.ExpectedVersion, mutation.NextVersion, meta.Version)
}

// databaseMapHint 说明怎么提供映射表，附在 ErrDatabaseMapNotBound 后面。
const databaseMapHint = "pass the frozen map (wiring does this; a self-assembled store builds one with dataengine.NewDatabaseMap, " +
	"DatabaseMapConfig{Declared: dataengine.DeclaredDatabases()} keeps logical names as physical names)"

// physicalDatabase 是 key 所在的物理库名（不含 _<sid>）：逻辑库经映射表换算，空库名取 DataEngine 的默认库。
// 本包访问业务文档的每条路径（单条与批量投影、冷加载、墓碑）都经 collection 走到这里，是 DataEngine 侧的
// 唯一强制点：带库名而没有映射表时返回 coredata.ErrDatabaseMapNotBound，不按逻辑名访问库（维护者 2026-10-21）。
func (store *MongoStore) physicalDatabase(key coredata.DocumentKey) (string, error) {
	if key.Database == "" {
		return store.cfg.DefaultDatabase, nil
	}
	if store.cfg.Databases == nil {
		return "", fmt.Errorf("%w: logical database %q (%s); MongoStoreConfig.Databases is nil; %s",
			coredata.ErrDatabaseMapNotBound, key.Database, key.Resource, databaseMapHint)
	}
	return store.cfg.Databases.Physical(key.Database), nil
}

func (store *MongoStore) collection(key coredata.DocumentKey) (fmongo.ICollection, error) {
	database, err := store.physicalDatabase(key)
	if err != nil {
		return nil, err
	}
	if key.Scope == coredata.DatabaseServer {
		return store.client.DatabaseForSid(database, store.cfg.ServerID).Collection(key.Resource), nil
	}
	return store.client.Database(database).Collection(key.Resource), nil
}

type transactionDocument struct {
	ID        string    `bson:"_id"`
	Digest    []byte    `bson:"digest"`
	CreatedAt time.Time `bson:"created_at"`
	Skipped   bool      `bson:"skipped,omitempty"`
}

// insertTransactionMarker 只在 Project 回调里、checkTransaction 已在同一事务快照中确认
// marker 不存在之后调用。并发投影同一记录时，对方在快照之后提交（或尚未提交）的 marker
// 让这里的插入得到 WriteConflict（TransientTransactionError），驱动重跑回调，由下一次
// 开头的 checkTransaction 裁决“已应用”或身份冲突。
//
// 撞键会中止 Mongo 事务，不能在同一事务里读回。快照读未命中后仍撞键
// 不属于上面的正常路径，按非 fatal 错误整笔回滚，保留原错误链；投影器重试时由新事务
// 开头的 checkTransaction 重新裁决。
func (store *MongoStore) insertTransactionMarker(ctx context.Context, document transactionDocument) error {
	_, err := store.client.Database(store.cfg.DefaultDatabase).Collection(TransactionCollection).InsertOne(ctx, document)
	if errors.Is(err, fmongo.ErrDuplicateKey) {
		return fmt.Errorf("dataengine mongo: transaction marker %s appeared after the snapshot read: %w", document.ID, err)
	}
	return err
}

// leaseFencesMatch 在投影事务内逐个确认 lease fence；任一 fence 失效返回 false，
// 调用方把整笔记录作为跳过处理。
func (store *MongoStore) leaseFencesMatch(ctx context.Context, receipts []coredata.Receipt) (bool, error) {
	now := store.now().UTC()
	for i := range receipts {
		fence, control, err := coredata.DecodeLeaseFenceReceipt(receipts[i])
		if err != nil {
			return false, err
		}
		if !control {
			continue
		}
		// 条件来自 fence 自身（Predicate），不在这里另拼：协调文档由另一个包写入，
		// 两处各写一份 schema 时，任一侧改名都会让所有 fenced 事务静默变成空操作。
		//
		// 校验必须是条件写而不是读：只读 claim 时，租约到期附近
		// 另一事务的接管（lease_token+1）与本投影事务互不写冲突，两边都会提交，同一步骤
		// 执行两次。写同一文档让 Mongo 只允许其中一个提交，输家由驱动重试后看到对方结果。
		result, err := store.client.Database(fence.Database).Collection(fence.Resource).
			UpdateOne(ctx, fence.Predicate(now), fence.Confirmation(now))
		if err == nil && (result == nil || result.MatchedCount == 0) {
			err = fmongo.ErrNotFound
		}
		if errors.Is(err, fmongo.ErrNotFound) {
			// 租约过期是预期结果，但 schema 漂移导致条件永远不满足看起来也一样。
			// 计数后，漂移会表现为所有 fenced 事务同时跳过，而不是悄无声息。
			metrics.IncCounter("dataengine.fence.skipped.total", metrics.Labels{"resource": fence.Resource}, 1)
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
	return true, nil
}

// checkTransaction 在投影事务快照内查事务标记，返回（已应用, 曾被跳过, 错误）；
// 标记存在但摘要不同是 fatal 的 ErrTransactionIdentity。
func (store *MongoStore) checkTransaction(ctx context.Context, txID string, digest []byte) (bool, bool, error) {
	var stored transactionDocument
	err := store.client.Database(store.cfg.DefaultDatabase).Collection(TransactionCollection).FindOne(ctx, bson.M{"_id": txID}, &stored)
	if errors.Is(err, fmongo.ErrNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if !bytes.Equal(stored.Digest, digest) {
		return false, false, ErrTransactionIdentity
	}
	return true, stored.Skipped, nil
}

type receiptDocument struct {
	ID            string    `bson:"_id"`
	Namespace     string    `bson:"namespace"`
	ReceiptID     string    `bson:"receipt_id"`
	TransactionID string    `bson:"transaction_id"`
	Digest        []byte    `bson:"digest"`
	Payload       []byte    `bson:"payload,omitempty"`
	ExpiresAt     time.Time `bson:"expires_at"`
	CreatedAt     time.Time `bson:"created_at"`
}

// stageReceipt 在 Project 的 Mongo 事务内先快照读、后插入。
//
// 真实 Mongo 在事务内任一写错误（撞唯一键也是）后立即中止事务，之后同一事务的读返回
// 带 TransientTransactionError 标签的 NoSuchTransaction，驱动会重跑回调直到
// transaction_timeout。所以身份裁决必须发生在插入之前：已有同 ID 回执时，digest 与
// payload 都相同即幂等成功（允许来自另一事务），任一不同即 fatal ErrReceiptIdentity。
// 快照里没有才插入；并发事务插入同一回执时，服务端以 WriteConflict（transient）中止
// 本事务，驱动重跑回调，下一次快照读就会看到对方已提交的回执并按上面规则裁决。
func (store *MongoStore) stageReceipt(ctx context.Context, txID string, receipt coredata.Receipt) error {
	expiresAt := time.Unix(0, receipt.ExpiresAt).UTC()
	if receipt.ExpiresAt == 0 {
		expiresAt = store.now().UTC().Add(store.cfg.ReceiptTTL)
	}
	doc := receiptDocument{
		ID: receipt.Namespace + "/" + receipt.ID, Namespace: receipt.Namespace, ReceiptID: receipt.ID,
		TransactionID: txID, Digest: append([]byte(nil), receipt.Digest...), Payload: append([]byte(nil), receipt.Payload...),
		ExpiresAt: expiresAt, CreatedAt: store.now().UTC(),
	}
	coll := store.client.Database(store.cfg.DefaultDatabase).Collection(ReceiptCollection)
	var stored receiptDocument
	err := coll.FindOne(ctx, bson.M{"_id": doc.ID}, &stored)
	if err == nil {
		if !bytes.Equal(stored.Digest, doc.Digest) || !bytes.Equal(stored.Payload, doc.Payload) {
			return fmt.Errorf("%w: %s", ErrReceiptIdentity, doc.ID)
		}
		return nil
	}
	if !errors.Is(err, fmongo.ErrNotFound) {
		return err
	}
	_, err = coll.InsertOne(ctx, doc)
	if errors.Is(err, fmongo.ErrDuplicateKey) {
		// 快照读未命中却撞键：事务已被服务端中止，不能在这里读回。按非 fatal 整笔回滚，
		// 投影器重试时由新事务的快照读裁决；原错误链保留，驱动标签不被吞掉。
		return fmt.Errorf("dataengine mongo: receipt %s appeared after the snapshot read: %w", doc.ID, err)
	}
	return err
}

type outboxDocument struct {
	ID            string            `bson:"_id"`
	EffectID      string            `bson:"effect_id"`
	TransactionID string            `bson:"transaction_id"`
	Topic         string            `bson:"topic"`
	Key           string            `bson:"key,omitempty"`
	Payload       []byte            `bson:"payload,omitempty"`
	Headers       map[string]string `bson:"headers,omitempty"`
	AvailableAt   time.Time         `bson:"available_at"`
	LeaseOwner    string            `bson:"lease_owner,omitempty"`
	LeaseUntil    time.Time         `bson:"lease_until"`
	LeaseToken    uint64            `bson:"lease_token"`
	Attempt       uint32            `bson:"attempt"`
	LastError     string            `bson:"last_error,omitempty"`
	CreatedAt     time.Time         `bson:"created_at"`
}

// stageEffect 与 stageReceipt 相同，在事务快照内先读后插。outbox 中
// 已有同 ID effect 时，同一事务、同一 topic 即幂等成功；属于另一事务或 topic 不同是 WAL
// 身份缺陷，返回 fatal ErrTransactionIdentity（同一 saga start 在两个事务各发一次即此形状，
// 按现有设计保持 fatal）。outbox 文档投递成功后会被删除，这项检查只覆盖尚未投递的窗口。
func (store *MongoStore) stageEffect(ctx context.Context, txID string, effect coredata.Effect) error {
	availableAt := time.Unix(0, effect.AvailableAt).UTC()
	if effect.AvailableAt == 0 {
		availableAt = store.now().UTC()
	}
	doc := outboxDocument{
		ID: effect.ID, EffectID: effect.ID, TransactionID: txID, Topic: effect.Topic, Key: effect.Key,
		Payload: append([]byte(nil), effect.Payload...), Headers: cloneHeaders(effect.Headers),
		AvailableAt: availableAt, CreatedAt: store.now().UTC(),
	}
	coll := store.client.Database(store.cfg.DefaultDatabase).Collection(OutboxCollection)
	var stored outboxDocument
	err := coll.FindOne(ctx, bson.M{"_id": doc.ID}, &stored)
	if err == nil {
		if stored.TransactionID != txID || stored.Topic != effect.Topic {
			return fmt.Errorf("%w: effect %s staged by transaction %s", ErrTransactionIdentity, doc.ID, stored.TransactionID)
		}
		return nil
	}
	if !errors.Is(err, fmongo.ErrNotFound) {
		return err
	}
	_, err = coll.InsertOne(ctx, doc)
	if errors.Is(err, fmongo.ErrDuplicateKey) {
		// 见 stageReceipt：事务已中止，非 fatal 回滚，由重试时的快照读裁决。
		return fmt.Errorf("dataengine mongo: effect %s appeared after the snapshot read: %w", doc.ID, err)
	}
	return err
}

// digestRecord 是事务的持久身份摘要（JSON 编码的 SHA-256），用于重放时识别同一事务。
func digestRecord(record coredata.CommitRecord) ([]byte, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	return digest[:], nil
}

func documentKeyLess(left, right coredata.DocumentKey) bool {
	if left.Database != right.Database {
		return left.Database < right.Database
	}
	if left.Scope != right.Scope {
		return left.Scope < right.Scope
	}
	if left.Resource != right.Resource {
		return left.Resource < right.Resource
	}
	return left.ID < right.ID
}

func documentInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, true
	case int32:
		return int64(typed), true
	case int:
		return int64(typed), true
	default:
		return 0, false
	}
}

func cloneHeaders(headers map[string]string) map[string]string {
	if headers == nil {
		return nil
	}
	cloned := make(map[string]string, len(headers))
	for key, value := range headers {
		cloned[key] = value
	}
	return cloned
}
