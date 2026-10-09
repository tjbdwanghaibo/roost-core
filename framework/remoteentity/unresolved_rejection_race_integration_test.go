//go:build integration

package remoteentity

// OPEN-ITEMS B12：RR-20260926-28 的持久拒绝（RejectUnresolvedRemoteCommits，非事务 InsertOne）
// 与在途 Mongo 事务（CommitRemoteBatch 里插入同一事务 _id）之间的裁决，之前只在 mongotest 上验证过顺序化结果。
// 这里在真实副本集上把两种先后都钉住：
//   - 事务先插入、尚未提交：拒绝插入必须等事务结束，提交则撞键读回 Applied（不覆盖、不回滚），
//     事务中止则拒绝落库，之后迟到的提交只能得到 ErrRemoteRejected；
//   - 拒绝先插入（事务已开始但还没插入）：拒绝立即成功，事务的插入被冲突挡住，
//     CommitRemoteBatch 返回 ErrRemoteRejected，事务里的元数据与数据写入全部不生效。
// 暂停点放在事务集合的 InsertOne 前后，由测试用通道控制事件顺序，不靠 sleep 碰运气。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// kind 144：remoteentity 包内测试已用 12、91～96、119～135、143、196～198、217、236、237、243、249，不能撞号。
const rejectionRaceKind entity.EntityKind = 144

type rejectionRacePause uint8

const (
	pauseBeforeTxInsert rejectionRacePause = iota + 1
	pauseAfterTxInsert
)

// txInsertGate 只拦截事务集合上第一次 InsertOne（也就是 CommitRemoteBatch 事务里那一次）。
type txInsertGate struct {
	pause   rejectionRacePause
	abort   error // 非空时在“插入后”暂停结束后返回它，迫使事务中止
	once    sync.Once
	reached chan struct{}
	release chan struct{}

	mu        sync.Mutex
	insertErr error
	inserts   int
}

func newTxInsertGate(pause rejectionRacePause, abort error) *txInsertGate {
	return &txInsertGate{pause: pause, abort: abort, reached: make(chan struct{}), release: make(chan struct{})}
}

func (g *txInsertGate) insert(ctx context.Context, do func() (string, error)) (string, error) {
	first := false
	g.once.Do(func() { first = true })
	if first && g.pause == pauseBeforeTxInsert {
		close(g.reached)
		<-g.release
	}
	id, err := do()
	g.mu.Lock()
	g.inserts++
	if first {
		g.insertErr = err
	}
	g.mu.Unlock()
	if first && g.pause == pauseAfterTxInsert {
		close(g.reached)
		<-g.release
		if err == nil && g.abort != nil {
			return "", g.abort
		}
	}
	return id, err
}

func (g *txInsertGate) observed() (error, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.insertErr, g.inserts
}

type gatedTxMongo struct {
	fmongo.IMongo
	control string
	gate    *txInsertGate
}

func (m gatedTxMongo) Database(name string) fmongo.IDatabase {
	db := m.IMongo.Database(name)
	if name != m.control {
		return db
	}
	return gatedTxDatabase{IDatabase: db, gate: m.gate}
}

type gatedTxDatabase struct {
	fmongo.IDatabase
	gate *txInsertGate
}

func (d gatedTxDatabase) Collection(name string) fmongo.ICollection {
	c := d.IDatabase.Collection(name)
	if name != remoteTxCollection {
		return c
	}
	return gatedTxCollection{ICollection: c, gate: d.gate}
}

type gatedTxCollection struct {
	fmongo.ICollection
	gate *txInsertGate
}

func (c gatedTxCollection) InsertOne(ctx context.Context, doc any) (string, error) {
	return c.gate.insert(ctx, func() (string, error) { return c.ICollection.InsertOne(ctx, doc) })
}

type rejectionRaceFixture struct {
	mongo    fmongo.IMongo
	database string
	store    *MongoCommitter
	commits  []entity.RemoteCommit
}

func realRejectionRaceMongo(t *testing.T) (fmongo.IMongo, string) {
	t.Helper()
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" {
		t.Skip("set ROOST_DATAENGINE_IT=1 or use scripts/integration/dataengine-env.sh test")
	}
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Fatal("isolated Mongo replica set required")
	}
	mongo, err := mongodriver.NewClient(fmongo.DefaultConfig(uri), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	database := fmt.Sprintf("roost_rr28_race_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := mongo.Database(database).Drop(ctx); err != nil {
			t.Error(err)
		}
		_ = mongo.Close(ctx)
	})
	return mongo, database
}

// newRejectionRaceFixture 经正式 Manager / Backend / MongoCommitter 准备一笔待提交的 Remote 事务内容，
// 不提交：提交与拒绝的先后由各用例自己控制。
func newRejectionRaceFixture(t *testing.T, id int64, tx entity.RemoteTransactionID) rejectionRaceFixture {
	t.Helper()
	mongo, database := realRejectionRaceMongo(t)
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: rejectionRaceKind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	store := NewMongoCommitter(mongo, database, 1000, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.EnsureRemoteStorage(ctx); err != nil {
		t.Fatal(err)
	}
	loader := newRemoteTestLoader()
	live := newTestRemoteEntity(id, 1, rejectionRaceKind)
	loader.add(live)
	backend, err := NewBackend(loader, store)
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	mgr.SetBackend(backend)
	mgr.SetOwnershipStore(store)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = mgr.StopFinalizer(ctx)
	})
	batch, err := mgr.PrepareRemoteWriteBatch(ctx, []int64{live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	live.dirty.set(true)
	if err = batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx, "memory", "", true, 0)); err != nil {
		t.Fatal(err)
	}
	commits := batch.Commits()
	_ = batch.Abort(ctx, errors.New("only the commit content is needed"))
	_ = batch.Close(ctx)
	return rejectionRaceFixture{mongo: mongo, database: database, store: store, commits: commits}
}

type commitResult struct {
	receipts []entity.RemoteCommitReceipt
	err      error
}

// startGatedCommit 用拦截了事务集合 InsertOne 的 committer 发起提交，并等到暂停点。
func (f rejectionRaceFixture) startGatedCommit(t *testing.T, gate *txInsertGate) <-chan commitResult {
	t.Helper()
	gated := NewMongoCommitter(gatedTxMongo{IMongo: f.mongo, control: f.database, gate: gate}, f.database, 1000, 0)
	done := make(chan commitResult, 1)
	go func() {
		receipts, err := gated.CommitRemoteBatch(context.Background(), f.commits)
		done <- commitResult{receipts, err}
	}()
	select {
	case <-gate.reached:
	case r := <-done:
		t.Fatalf("commit finished before reaching the transaction insert: %+v", r)
	case <-time.After(10 * time.Second):
		t.Fatal("commit never reached the transaction insert")
	}
	return done
}

type rejectResult struct {
	status entity.RemoteCommitStatus
	err    error
}

func (f rejectionRaceFixture) startRejection() <-chan rejectResult {
	done := make(chan rejectResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		status, err := f.store.RejectUnresolvedRemoteCommits(ctx, f.commits, "unresolved")
		done <- rejectResult{status, err}
	}()
	return done
}

// dataVersion 读权威元数据与数据文档，区分“事务生效”与“事务中止”。
func (f rejectionRaceFixture) dataVersion(t *testing.T) (meta uint64, dataFound bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := f.commits[0].EntityID
	var doc struct {
		Version uint64 `bson:"_ver"`
	}
	if err := f.mongo.Database(f.database).Collection(remoteMetaCollection).FindOne(ctx, bson.M{"_id": id}, &doc); err != nil {
		t.Fatalf("authority metadata: %v", err)
	}
	var data bson.M
	err := f.mongo.Database(f.database).Collection(f.commits[0].Mutations[0].Collection).FindOne(ctx, bson.M{"_id": id}, &data)
	if err != nil && !errors.Is(err, fmongo.ErrNotFound) {
		t.Fatal(err)
	}
	return doc.Version, err == nil
}

func receiveCommit(t *testing.T, done <-chan commitResult) commitResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("commit did not finish")
		return commitResult{}
	}
}

func receiveRejection(t *testing.T, done <-chan rejectResult) rejectResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("rejection did not finish")
		return rejectResult{}
	}
}

// 事务先插入同一 _id 且未提交：拒绝插入不能抢先成功，要等事务提交后撞键，按读回的 Applied 返回。
func TestRealMongoRejectionWaitsForInFlightCommitThenReportsApplied(t *testing.T) {
	f := newRejectionRaceFixture(t, 1440, remoteTestTxID(144))
	gate := newTxInsertGate(pauseAfterTxInsert, nil)
	commit := f.startGatedCommit(t, gate)
	if err, _ := gate.observed(); err != nil {
		t.Fatalf("transaction insert: %v", err)
	}
	rejection := f.startRejection()
	select {
	case r := <-rejection:
		t.Fatalf("rejection finished while the transaction holding the same _id was still open: %+v", r)
	case <-time.After(500 * time.Millisecond):
	}
	close(gate.release)
	committed := receiveCommit(t, commit)
	if committed.err != nil || len(committed.receipts) != 1 {
		t.Fatalf("commit: receipts=%+v err=%v", committed.receipts, committed.err)
	}
	rejected := receiveRejection(t, rejection)
	if rejected.err != nil || rejected.status.State != entity.RemoteCommitApplied || len(rejected.status.Receipts) != 1 || rejected.status.Receipts[0].StateVersion != committed.receipts[0].StateVersion {
		t.Fatalf("rejection after the commit landed: status=%+v err=%v, want the committed transaction", rejected.status, rejected.err)
	}
	if durable, err := f.store.CommitStatus(context.Background(), f.commits[0].TransactionID); err != nil || durable.State != entity.RemoteCommitApplied {
		t.Fatalf("rejection overwrote the committed transaction: %+v err=%v", durable, err)
	}
	if version, found := f.dataVersion(t); version != f.commits[0].NextVersion || !found {
		t.Fatalf("committed data: meta version=%d data=%v, want %d and present", version, found, f.commits[0].NextVersion)
	}
}

// 事务先插入同一 _id、随后中止：等待中的拒绝插入在中止后成功，迟到的提交只能得到 ErrRemoteRejected。
func TestRealMongoRejectionWinsAfterInFlightCommitAborts(t *testing.T) {
	f := newRejectionRaceFixture(t, 1441, remoteTestTxID(145))
	abort := errors.New("abort the in-flight transaction")
	gate := newTxInsertGate(pauseAfterTxInsert, abort)
	commit := f.startGatedCommit(t, gate)
	rejection := f.startRejection()
	select {
	case r := <-rejection:
		t.Fatalf("rejection finished while the transaction holding the same _id was still open: %+v", r)
	case <-time.After(500 * time.Millisecond):
	}
	close(gate.release)
	if r := receiveCommit(t, commit); !errors.Is(r.err, abort) {
		t.Fatalf("aborted commit: receipts=%+v err=%v", r.receipts, r.err)
	}
	rejected := receiveRejection(t, rejection)
	if rejected.err != nil || rejected.status.State != entity.RemoteCommitRejected {
		t.Fatalf("rejection after abort: status=%+v err=%v", rejected.status, rejected.err)
	}
	if _, err := f.store.CommitRemoteBatch(context.Background(), f.commits); !errors.Is(err, entity.ErrRemoteRejected) {
		t.Fatalf("late commit after the durable rejection: err=%v, want ErrRemoteRejected", err)
	}
	if version, found := f.dataVersion(t); version != f.commits[0].BaseVersion || found {
		t.Fatalf("rejected transaction took effect: meta version=%d data=%v, want %d and absent", version, found, f.commits[0].BaseVersion)
	}
}

// 拒绝先插入（事务已开始、已写元数据与数据，但还没插入事务记录）：拒绝立即成功，
// 事务插入被冲突挡住，CommitRemoteBatch 返回 ErrRemoteRejected，事务内写入全部不生效。
func TestRealMongoRejectionFirstAbortsTheInFlightCommit(t *testing.T) {
	f := newRejectionRaceFixture(t, 1442, remoteTestTxID(146))
	gate := newTxInsertGate(pauseBeforeTxInsert, nil)
	commit := f.startGatedCommit(t, gate)
	rejected := receiveRejection(t, f.startRejection())
	if rejected.err != nil || rejected.status.State != entity.RemoteCommitRejected {
		t.Fatalf("rejection before the transaction insert: status=%+v err=%v", rejected.status, rejected.err)
	}
	close(gate.release)
	r := receiveCommit(t, commit)
	insertErr, inserts := gate.observed()
	t.Logf("in-flight transaction insert after the rejection: err=%v (transaction inserts attempted=%d); commit err=%v", insertErr, inserts, r.err)
	if insertErr == nil {
		t.Fatal("the transaction inserted the same _id after a committed rejection")
	}
	if !errors.Is(r.err, entity.ErrRemoteRejected) {
		t.Fatalf("commit that lost to the rejection: receipts=%+v err=%v, want ErrRemoteRejected", r.receipts, r.err)
	}
	if durable, err := f.store.CommitStatus(context.Background(), f.commits[0].TransactionID); err != nil || durable.State != entity.RemoteCommitRejected {
		t.Fatalf("durable outcome: %+v err=%v, want Rejected", durable, err)
	}
	if version, found := f.dataVersion(t); version != f.commits[0].BaseVersion || found {
		t.Fatalf("rejected transaction took effect: meta version=%d data=%v, want %d and absent", version, found, f.commits[0].BaseVersion)
	}
}
