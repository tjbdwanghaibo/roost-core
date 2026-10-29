//go:build integration

package engine

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type capturingCommitter struct{ record coredata.CommitRecord }

func (committer *capturingCommitter) Commit(_ context.Context, record corenest.CommitRecord) error {
	committer.record = coredata.CloneCommitRecord(record)
	return nil
}

// leaseStep 是 DataEngine lease fence 的真实 Mongo 用例用的“原生步骤”替身。产生 fenced 记录的 saga 原生收件箱
// （DataEngineStepInbox：操作状态文档 + Reserve 事务 + Bind）在 saga 第 4 批随 Redis 协调器删除，原生步骤改为实体内回执；
// fence 机制本身在 DataEngine 里留到第 5 批单独删除（DESIGN-2026-10-28-saga-jetstream-authority §13）。在那之前它仍是
// 投影器的一条分支，所以这里按旧收件箱写进 Mongo 的同一形状重建最小替身：操作状态文档（owner / lease_token / digest /
// status / lease_until）、带 fence 回执与步骤回执的记录、在 Mongo 事务里接管租约的另一 owner。判定只保留这两个用例
// 走到的分支（回执已存在 → 重复；租约未过期 → 在途；过期 → token+1 接管），与旧 reserveInTransaction 的对应行一致。
type leaseStep struct {
	client   fmongo.IMongo
	database string
}

const (
	leaseStepOperations = "_dataengine_step_operations"
	leaseStepNamespace  = "saga-step"
)

type leaseStepCommand struct {
	ID, OperationKey string
	Digest           []byte
}

func newLeaseStepCommand(name string) leaseStepCommand {
	digest := sha256.Sum256([]byte(name)) // fence 的摘要是 SHA-256 长度（LeaseFence.Validate）
	return leaseStepCommand{ID: name, OperationKey: name + "-op", Digest: digest[:]}
}

// claim 写这个操作的第一份状态文档：owner 持有 token 1，租约到 leaseUntil。
func (f leaseStep) claim(t *testing.T, ctx context.Context, command leaseStepCommand, owner string, leaseUntil time.Time) uint64 {
	t.Helper()
	_, err := f.client.Database(f.database).Collection(leaseStepOperations).InsertOne(ctx, bson.M{
		"_id": command.OperationKey, coredata.LeaseFenceFieldOwner: owner, coredata.LeaseFenceFieldToken: uint64(1),
		coredata.LeaseFenceFieldDigest: command.Digest, coredata.LeaseFenceFieldStatus: coredata.LeaseFenceStatusPending,
		coredata.LeaseFenceFieldLeaseUntil: leaseUntil,
	})
	if err != nil {
		t.Fatal(err)
	}
	return 1
}

// record 是一笔真实 Nest 事务产出的原生步骤记录：fence 回执、带结果的步骤回执、结果 effect，再附上 DAO 修改。
func (f leaseStep) record(t *testing.T, ctx context.Context, command leaseStepCommand, owner string, token uint64, mutation coredata.Mutation) coredata.CommitRecord {
	t.Helper()
	fence, err := coredata.NewLeaseFenceReceipt(coredata.LeaseFence{
		Database: f.database, Resource: leaseStepOperations, DocumentID: command.OperationKey,
		Owner: owner, Token: token, Digest: command.Digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	committer := &capturingCommitter{}
	if _, err := corenest.RunIsolatedTransaction(ctx, committer, "lease-step-"+command.ID, func() (any, error) {
		if err := corenest.AddReceipt(fence); err != nil {
			return nil, err
		}
		if err := corenest.AddReceipt(coredata.Receipt{Namespace: leaseStepNamespace, ID: command.ID, Digest: command.Digest, ExpiresAt: time.Now().Add(time.Hour).UnixNano()}); err != nil {
			return nil, err
		}
		if err := corenest.SetReceiptPayload(leaseStepNamespace, command.ID, []byte("success:"+command.ID)); err != nil {
			return nil, err
		}
		return nil, corenest.Emit(corenest.Effect{ID: "saga-completion:" + command.ID, Topic: "saga.result." + command.ID, Key: command.OperationKey, Payload: []byte("success:" + command.ID)})
	}); err != nil {
		t.Fatal(err)
	}
	record := committer.record
	record.Mutations = append(record.Mutations, mutation)
	return record
}

// takeover 是另一 owner 在一个 Mongo 事务里接管这次操作：先读步骤回执（已投影 → 重复投递），再按租约判定；过期则
// token+1 写回状态文档。返回新 token；duplicate 表示回执已经存在。
func (f leaseStep) takeover(ctx context.Context, command leaseStepCommand, owner string) (token uint64, duplicate bool, err error) {
	session, err := f.client.StartSession(ctx)
	if err != nil {
		return 0, false, err
	}
	defer session.EndSession(ctx)
	err = session.WithTransaction(ctx, func(txCtx context.Context) error {
		token, duplicate = 0, false
		var receipt bson.M
		switch err := f.client.Database(f.database).Collection(ReceiptCollection).FindOne(txCtx, bson.M{"_id": leaseStepNamespace + "/" + command.ID}, &receipt); {
		case err == nil:
			duplicate = true
			return nil
		case !errors.Is(err, fmongo.ErrNotFound):
			return err
		}
		var state struct {
			Token      uint64    `bson:"lease_token"`
			LeaseUntil time.Time `bson:"lease_until"`
		}
		operations := f.client.Database(f.database).Collection(leaseStepOperations)
		if err := operations.FindOne(txCtx, bson.M{"_id": command.OperationKey}, &state); err != nil {
			return err
		}
		if state.LeaseUntil.After(time.Now()) {
			return errors.New("lease step: the current attempt's lease is still valid")
		}
		result, err := operations.UpdateOne(txCtx, bson.M{"_id": command.OperationKey, coredata.LeaseFenceFieldToken: state.Token}, bson.M{"$set": bson.M{
			coredata.LeaseFenceFieldOwner: owner, coredata.LeaseFenceFieldToken: state.Token + 1, coredata.LeaseFenceFieldLeaseUntil: time.Now().Add(time.Hour),
		}})
		if err != nil {
			return err
		}
		if result == nil || result.MatchedCount != 1 {
			return errors.New("lease step: the state document moved")
		}
		token = state.Token + 1
		return nil
	})
	return token, duplicate, err
}

// settle 结算这次操作（状态离开 pending，fence 从此不再通过）。
func (f leaseStep) settle(t *testing.T, ctx context.Context, command leaseStepCommand) {
	t.Helper()
	if _, err := f.client.Database(f.database).Collection(leaseStepOperations).UpdateOne(ctx, bson.M{"_id": command.OperationKey}, bson.M{"$set": bson.M{coredata.LeaseFenceFieldStatus: "settled"}}); err != nil {
		t.Fatal(err)
	}
}

// isolatedRealMongo 连接隔离副本集并使用一次性库名；绝不写共享 game 库，结束时删库。
func isolatedRealMongo(t *testing.T) (context.Context, fmongo.IMongo, string) {
	t.Helper()
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Skip("ROOST_DATAENGINE_IT_MONGO_URI is not set; run through kit/scripts/integration/dataengine-env.sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	client, err := driver.NewClient(fmongo.DefaultConfig(uri), driver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	database := fmt.Sprintf("roost_rr30_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		_ = client.Database(database).Drop(context.Background())
		_ = client.Close(context.Background())
	})
	return ctx, client, database
}

// RR-20260926-30 §6 写偏斜：投影事务只读 claim、Reserve 的接管在另一事务里写 claim。
// 租约到期附近两者并发时，投影事务的快照读到租约有效，接管事务在到期后提交 token+1；
// 投影事务没写 claim，不产生写冲突，两边都提交——同一步骤被执行两次。
// 受控顺序：投影在租约校验通过之后、业务写入之前停在测试缝上，同步执行一次真实的
// 接管（另一 owner，leaseStep.takeover；原为 saga.DataEngineStepInbox.Reserve），再让投影继续提交。
// 承诺：两者至多一个生效——投影落库时接管不能成功拿到新 token。
func TestRealMongoFencedProjectionSerializesWithLeaseTakeover(t *testing.T) {
	ctx, client, database := isolatedRealMongo(t)
	store, err := NewMongoStore(client, MongoStoreConfig{Databases: identityDatabases(t), DefaultDatabase: database})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	steps := leaseStep{client: client, database: database}
	now := time.Now().UTC()
	command := newLeaseStepCommand("rr30-skew")
	token := steps.claim(t, ctx, command, "worker-1", now.Add(time.Hour))

	// 原生步骤的 fence、step receipt 与 completion effect（一笔真实 Nest 事务），再附上本地 DAO 的 4 -> 5 修改。
	heroes := client.Database(database).Collection("heroes")
	if _, err := heroes.InsertOne(ctx, bson.M{"_id": int64(7), "_version": uint64(4), "level": int32(1)}); err != nil {
		t.Fatal(err)
	}
	patch, _ := bson.Marshal(bson.D{{Key: "level", Value: int32(5)}})
	record := steps.record(t, ctx, command, "worker-1", token, coredata.Mutation{
		Key:  coredata.DocumentKey{Database: database, Resource: "heroes", ID: 7},
		Kind: coredata.MutationPatch, ExpectedVersion: 4, NextVersion: 5, Mask: 1, Schema: 1, Codec: "bson-v2",
		Patch: coredata.FieldPatch{SetBSON: patch},
	})

	// 租约边界：接管方（真实时钟）看到租约已过期；投影方的时钟仍在到期之前。
	// 原生步骤的操作状态文档（saga 收件箱，每个操作一份，_id = IdempotencyKey）。
	operations := client.Database(database).Collection(leaseStepOperations)
	if _, err := operations.UpdateOne(ctx, bson.M{"_id": command.OperationKey}, bson.M{"$set": bson.M{"lease_until": now.Add(-time.Second)}}); err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return now.Add(-time.Minute) }

	var takeover uint64
	var takeoverDuplicate bool
	var takeoverErr error
	var once sync.Once
	store.afterLeaseFence = func(context.Context) {
		once.Do(func() {
			bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			takeover, takeoverDuplicate, takeoverErr = steps.takeover(bounded, command, "worker-2")
		})
	}
	projectErr := store.Project(ctx, record)
	store.afterLeaseFence = nil

	var hero struct {
		Version uint64 `bson:"_version"`
	}
	if err := heroes.FindOne(ctx, bson.M{"_id": int64(7)}, &hero); err != nil {
		t.Fatal(err)
	}
	applied := projectErr == nil && hero.Version == 5
	tookOver := takeoverErr == nil && !takeoverDuplicate && takeover > token
	t.Logf("projection err=%v hero_version=%d; concurrent takeover token=%d duplicate=%v err=%v", projectErr, hero.Version, takeover, takeoverDuplicate, takeoverErr)
	if applied && tookOver {
		t.Fatalf("write skew: the fenced step was applied (version=%d) while a concurrent takeover took the claim over (token=%d)", hero.Version, takeover)
	}
	if !applied && !tookOver {
		t.Fatalf("neither side won: projection err=%v version=%d, takeover token=%d duplicate=%v err=%v", projectErr, hero.Version, takeover, takeoverDuplicate, takeoverErr)
	}
	if applied {
		// 投影先提交：之后的接管必须看到 receipt，只能得到已完成的重复投递。
		if _, duplicate, err := steps.takeover(ctx, command, "worker-2"); err != nil || !duplicate {
			t.Fatalf("takeover after the projection committed: duplicate=%v err=%v, want a duplicate", duplicate, err)
		}
	}
}

// 崩溃注入：原生步骤记录（lease fence + saga 回执 + completion effect）与带 effect 的普通记录已在 Mongo 提交，
// checkpoint 合并在本轮末尾，进程恰好在这次 checkpoint 落盘前退出。首投之后世界继续前进：步骤 A 的操作状态文档
// 被结算（fence 现在会失败），被跳过的步骤 B 的租约被续上（fence 现在会通过），outbox 已发布并删除了 effect，
// 同一文档的后续版本已经投影。重启（重开 WAL、新 Store、新投影器）后重放必须返回首投的结论：业务值、回执、
// effect、操作状态文档、事务标记都恰好一次，fence 不被重新判定。
func TestRealMongoSpecialRecordsReplayExactlyOnceAfterLostCheckpoint(t *testing.T) {
	ctx, client, database := isolatedRealMongo(t)
	store, err := NewMongoStore(client, MongoStoreConfig{Databases: identityDatabases(t), DefaultDatabase: database})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	steps := leaseStep{client: client, database: database}
	heroes := client.Database(database).Collection("heroes")
	for _, id := range []int64{7, 8} {
		if _, err := heroes.InsertOne(ctx, bson.M{"_id": id, "_version": uint64(1), "level": int32(1)}); err != nil {
			t.Fatal(err)
		}
	}
	patch := func(id int64, from uint64, level int32) coredata.Mutation {
		set, _ := bson.Marshal(bson.D{{Key: "level", Value: level}})
		return coredata.Mutation{
			Key:  coredata.DocumentKey{Database: database, Resource: "heroes", ID: id},
			Kind: coredata.MutationPatch, ExpectedVersion: from, NextVersion: from + 1, Mask: 1, Schema: 1, Codec: "bson-v2",
			Patch: coredata.FieldPatch{SetBSON: set},
		}
	}
	now := time.Now().UTC()
	step := func(name string, mutation coredata.Mutation) (leaseStepCommand, coredata.CommitRecord) {
		command := newLeaseStepCommand("ckpt-" + name)
		token := steps.claim(t, ctx, command, "worker-1", now.Add(time.Hour))
		return command, steps.record(t, ctx, command, "worker-1", token, mutation)
	}
	// S1：步骤 A 改 hero 7（1→2），首投生效。S2：步骤 B 改 hero 8（1→2），首投时租约已过期，整笔跳过。
	// S3：不带 fence、带 effect 的普通特殊记录，改 hero 7（2→3）——S1 之后的同文档版本。
	commandA, s1 := step("a", patch(7, 1, 2))
	_, s2 := step("b", patch(8, 1, 2))
	operations := client.Database(database).Collection(leaseStepOperations)
	if _, err := operations.UpdateOne(ctx, bson.M{"_id": "ckpt-b-op"}, bson.M{"$set": bson.M{"lease_until": now.Add(-time.Second)}}); err != nil {
		t.Fatal(err)
	}
	var s3ID coredata.TransactionID
	copy(s3ID[:], "ckpt-s3-ordinary")
	s3 := coredata.CommitRecord{
		ID: s3ID, Handler: "ckpt-ordinary", Durability: corenest.DurabilityStrict.Record(),
		Mutations: []coredata.Mutation{patch(7, 2, 3)},
		Effects:   []coredata.Effect{{ID: "ckpt-s3-effect", Topic: "hero.changed", Payload: []byte{3}}},
	}
	records := []coredata.CommitRecord{s1, s2, s3}

	walOptions := nestwal.DefaultOptions(t.TempDir())
	openProjector := func(store ProjectionStore) (*nestwal.WAL, *Projector) {
		wal, err := nestwal.Open(walOptions)
		if err != nil {
			t.Fatal(err)
		}
		projector, err := NewProjector(wal, store, ProjectorOptions{ManualReplay: true, CloseWAL: true, CheckpointInterval: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		return wal, projector
	}
	wal, projector := openProjector(store)
	for i := range records {
		if _, err := wal.Append(ctx, records[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := wal.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	// 进程在本轮唯一一次 checkpoint 落盘前退出：Mongo 已提交三条，WAL checkpoint 仍在它们之前。
	crashed := errors.New("process exited before the checkpoint reached the disk")
	acks := 0
	projector.OverrideAck(func(context.Context, corenest.CommitFence) error { acks++; return crashed })
	if n, err := projector.ReplayPass(ctx); n != len(records) || !errors.Is(err, crashed) || acks != 1 {
		t.Fatalf("first pass n=%d acks=%d err=%v; want all %d special records projected before one merged checkpoint", n, acks, err, len(records))
	}
	if err := projector.Close(ctx); err != nil {
		t.Fatal(err)
	}

	type hero struct {
		Version uint64 `bson:"_version"`
		Level   int32  `bson:"level"`
	}
	readHero := func(id int64) hero {
		var doc hero
		if err := heroes.FindOne(ctx, bson.M{"_id": id}, &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	count := func(collection string) int64 {
		n, err := client.Database(database).Collection(collection).CountDocuments(ctx, bson.M{})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := readHero(7); got != (hero{Version: 3, Level: 3}) {
		t.Fatalf("hero 7 after first pass=%+v", got)
	}
	if got := readHero(8); got != (hero{Version: 1, Level: 1}) {
		t.Fatalf("hero 8 after first pass=%+v (step B should have been skipped)", got)
	}
	if n := count(OutboxCollection); n != 2 {
		t.Fatalf("staged effects=%d, want S1 completion + S3 effect", n)
	}

	// 首投之后世界继续前进。
	steps.settle(t, ctx, commandA)
	if _, err := operations.UpdateOne(ctx, bson.M{"_id": "ckpt-b-op"}, bson.M{"$set": bson.M{"lease_until": now.Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Database(database).Collection(OutboxCollection).DeleteMany(ctx, bson.M{}); err != nil {
		t.Fatal(err)
	}
	snapshot := func(collection string) map[string]bson.M {
		var docs []bson.M
		if err := client.Database(database).Collection(collection).Find(ctx, bson.M{}, &docs); err != nil {
			t.Fatal(err)
		}
		byID := make(map[string]bson.M, len(docs))
		for _, doc := range docs {
			byID[fmt.Sprint(doc["_id"])] = doc
		}
		return byID
	}
	before := map[string]map[string]bson.M{}
	for _, collection := range []string{"heroes", leaseStepOperations, ReceiptCollection, TransactionCollection} {
		before[collection] = snapshot(collection)
	}
	if n := len(before[ReceiptCollection]); n != 1 {
		t.Fatalf("receipts=%d, want only step A's saga receipt", n)
	}
	if n := len(before[TransactionCollection]); n != 3 {
		t.Fatalf("transaction markers=%d", n)
	}

	// 重启：重开 WAL、新 Store、新投影器，像启动恢复一样排空。
	restarted, err := NewMongoStore(client, MongoStoreConfig{Databases: identityDatabases(t), DefaultDatabase: database})
	if err != nil {
		t.Fatal(err)
	}
	wal, projector = openProjector(restarted)
	// 重放读到的是 WAL 解码后的记录（与首投时 Store 看到的完全相同，事务身份 digest 按它计算）。
	var replayed []coredata.CommitRecord
	if err := wal.Replay(ctx, func(_ corenest.CommitFence, record coredata.CommitRecord) error {
		replayed = append(replayed, record)
		return nil
	}); err != nil || len(replayed) != len(records) {
		t.Fatalf("records to replay after restart=%d err=%v", len(replayed), err)
	}
	if err := projector.Flush(ctx); err != nil {
		t.Fatalf("replay after restart: %v", err)
	}
	if stats := projector.Stats(); stats.Projected != uint64(len(records)) || stats.FatalProjectionConflicts != 0 || stats.WALUnacked != 0 {
		t.Fatalf("stats after replay=%+v", stats)
	}
	remaining := 0
	if err := wal.Replay(ctx, func(corenest.CommitFence, coredata.CommitRecord) error { remaining++; return nil }); err != nil || remaining != 0 {
		t.Fatalf("WAL after replay remaining=%d err=%v", remaining, err)
	}
	if err := projector.Close(ctx); err != nil {
		t.Fatal(err)
	}
	for collection, want := range before {
		got := snapshot(collection)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s changed by the replay:\n got %v\nwant %v", collection, got, want)
		}
	}
	if n := count(OutboxCollection); n != 0 {
		t.Errorf("replay re-staged %d effects that the outbox had already published", n)
	}
	// 重放给出首投的 fence 结论：A 生效（虽然它的操作状态文档已结算）、B 跳过（虽然它的租约现在有效）。
	for _, tc := range []struct {
		record      coredata.CommitRecord
		wantSkipped bool
	}{{replayed[0], false}, {replayed[1], true}, {replayed[2], false}} {
		skipped, err := restarted.ProjectFenced(ctx, tc.record)
		if err != nil || skipped != tc.wantSkipped {
			t.Errorf("replayed verdict for %s: skipped=%v err=%v, want skipped=%v", tc.record.ID.String(), skipped, err, tc.wantSkipped)
		}
	}
}
