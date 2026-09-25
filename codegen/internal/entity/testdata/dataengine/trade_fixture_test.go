package persistflow

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	"go.mongodb.org/mongo-driver/v2/bson"
)

var errTradeRejected = errors.New("trade rejected after four DAO changes")
var errTradeAckLost = errors.New("injected WAL acknowledgement loss")

// 只控制下一次投影何时进入真实 MongoStore，不替代事务或修改提交记录。
type tradeProjectionGate struct {
	store *engine.MongoStore
	mu    sync.Mutex
	next  *tradeProjectionPause
}
type tradeProjectionPause struct {
	entered chan coredata.CommitRecord
	release chan struct{}
	once    sync.Once
}

func (pause *tradeProjectionPause) resume() { pause.once.Do(func() { close(pause.release) }) }
func (gate *tradeProjectionGate) pause() *tradeProjectionPause {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	pause := &tradeProjectionPause{entered: make(chan coredata.CommitRecord, 1), release: make(chan struct{})}
	gate.next = pause
	return pause
}
func (gate *tradeProjectionGate) wait(ctx context.Context, record coredata.CommitRecord) error {
	gate.mu.Lock()
	pause := gate.next
	gate.next = nil
	gate.mu.Unlock()
	if pause != nil {
		pause.entered <- coredata.CloneCommitRecord(record)
		select {
		case <-pause.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (gate *tradeProjectionGate) Project(ctx context.Context, record coredata.CommitRecord) error {
	if err := gate.wait(ctx, record); err != nil {
		return err
	}
	return gate.store.Project(ctx, record)
}
func (*tradeProjectionGate) SupportsMultiMutationBatch() bool { return true }
func (gate *tradeProjectionGate) ProjectBatch(ctx context.Context, records []coredata.CommitRecord) error {
	if err := gate.wait(ctx, records[0]); err != nil {
		return err
	}
	return gate.store.ProjectBatch(ctx, records)
}

type tradeFixture struct {
	ctx       context.Context
	client    fmongo.IMongo
	database  string
	runtime   *engine.Runtime
	scheduler *nest.NestMgr
	access    *entity.ManagerAccess
	wal       *nestwal.WAL
	options   nestwal.Options
	gate      *tradeProjectionGate
	fatal     chan error
	loseAck   atomic.Bool
	ackFailed chan struct{}
}

func newTradeFixture(t *testing.T, ctx context.Context, client fmongo.IMongo, root, scenario string, policy nest.DurabilityPolicy, ackHook bool, configure ...func(*engine.ProjectorOptions)) *tradeFixture {
	t.Helper()
	h := &tradeFixture{ctx: ctx, client: client, database: NewWalletDao().DbName(), fatal: make(chan error, 1), ackFailed: make(chan struct{}, 1)}
	store, err := engine.NewMongoStore(client, engine.MongoStoreConfig{DefaultDatabase: h.database})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	h.options = nestwal.DefaultOptions(filepath.Join(root, "trade", scenario, policy.String()))
	h.options.WriterVersion = nestwal.WriterVersionV2
	h.wal, err = nestwal.Open(h.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.wal.Close(context.Background()) })
	h.gate = &tradeProjectionGate{store: store}
	projectionOpts := engine.ProjectorOptions{
		CloseWAL: false, RetryMin: time.Hour, RetryMax: time.Hour,
		OnFatal: func(err error) {
			select {
			case h.fatal <- err:
			default:
			}
		},
	}
	for _, apply := range configure {
		apply(&projectionOpts)
	}
	projector, err := engine.NewProjector(h.wal, h.gate, projectionOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projector.Close(context.Background()) })
	if ackHook {
		// 只在全新、空 WAL 上安装；运行中通过 atomic 开关，不修改函数指针。
		projector.OverrideAck(func(ctx context.Context, fence nest.CommitFence) error {
			if h.loseAck.Load() {
				select {
				case h.ackFailed <- struct{}{}:
				default:
				}
				return errTradeAckLost
			}
			return h.wal.Ack(ctx, fence)
		})
	}
	outboxStore, err := engine.NewMongoOutboxStore(store)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := engine.NewOutboxWorker(outboxStore, noEffectsPublisher{}, engine.OutboxWorkerOptions{Owner: "trade-test"})
	if err != nil {
		t.Fatal(err)
	}
	h.access = entity.NewManagerAccess(entity.NewEntityManager())
	names := []string{"trade_seed", "trade_buy", "trade_transfer", "trade_reject", "trade_panic"}
	h.runtime, err = engine.NewRuntime(store, h.wal, projector, outbox, h.access, nil, nil, engine.PipelinedRuntimeConfig{Allowlist: names, Async: true, AsyncWorkers: 2, AsyncQueueCap: 128})
	if err != nil {
		t.Fatal(err)
	}
	if err = h.runtime.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.runtime.Shutdown(context.Background()) })
	options := append(h.runtime.NestOptions(), nest.NestOptionWithGetter(h.access), nest.NestOptionWithWorkerNumAndMsgCap(8, 1, 256))
	h.scheduler = nest.NewEngine(options...)
	meta := nest.HandlerMeta{Rollback: nest.RollbackState, Durability: policy}
	h.scheduler.MustRegisterHandlerWithMeta(nest.NewHandlerName("trade_seed"), func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		e := es[0].(*Trader)
		e.wallet.SetCoins(10000)
		e.inventory.SetItems(1000)
		return nil, nil
	}, meta)
	h.scheduler.MustRegisterHandlerWithMeta(nest.NewHandlerName("trade_buy"), func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		e := es[0].(*Trader)
		e.wallet.SetCoins(e.wallet.GetCoins() - 5)
		e.inventory.SetItems(e.inventory.GetItems() + 1)
		return nil, nil
	}, meta)
	for _, name := range names[2:] {
		h.scheduler.MustRegisterHandlerWithMeta(nest.NewHandlerName(name), func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
			if len(es) != 2 || es[0] == nil || es[1] == nil {
				return nil, errors.New("trade requires both entities")
			}
			a, b := es[0].(*Trader), es[1].(*Trader)
			a.wallet.SetCoins(a.wallet.GetCoins() - 3)
			b.wallet.SetCoins(b.wallet.GetCoins() + 3)
			a.inventory.SetItems(a.inventory.GetItems() + 1)
			b.inventory.SetItems(b.inventory.GetItems() - 1)
			for _, e := range []*Trader{a, b} {
				e.wallet.SetTransfers(e.wallet.GetTransfers() + 1)
				e.inventory.SetTransfers(e.inventory.GetTransfers() + 1)
			}
			if name == "trade_reject" {
				return nil, errTradeRejected
			}
			if name == "trade_panic" {
				panic("injected trade panic after four DAO changes")
			}
			return nil, nil
		}, meta)
	}
	if err = h.scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.scheduler.Shutdown(context.Background()) })
	return h
}
func (h *tradeFixture) seed(t *testing.T, base, count int) []int64 {
	t.Helper()
	ids := make([]int64, count)
	for i := range ids {
		id, err := entity.BuildEntityID(int64(base+i), EntityKindTrader)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
		if _, err = h.access.Create(&entity.EntityCreateParam{IsCreate: true, Kind: EntityKindTrader, Id: id}); err != nil {
			t.Fatal(err)
		}
		if _, err = h.scheduler.Request(h.ctx, nest.NewHandlerName("trade_seed"), id, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.runtime.Flush(h.ctx); err != nil {
		t.Fatal(err)
	}
	return ids
}
func (h *tradeFixture) request(name string, ids ...int64) error {
	_, err := h.scheduler.RequestMulti(h.ctx, nest.NewHandlerName(name), ids, nil)
	return err
}
func (h *tradeFixture) close(t *testing.T, want error) {
	t.Helper()
	if err := h.scheduler.Shutdown(h.ctx); err != nil {
		t.Fatal(err)
	}
	err := h.runtime.Shutdown(h.ctx)
	if want == nil && err != nil || want != nil && !errors.Is(err, want) {
		t.Fatalf("shutdown=%v want=%v", err, want)
	}
	if err = h.runtime.Shutdown(h.ctx); err != nil {
		t.Fatal(err)
	}
}

type tradeState struct {
	Coins, Items, Transfers int64
	Version                 uint64
}

func (h *tradeFixture) assertState(t *testing.T, id int64, want tradeState, memory bool) {
	t.Helper()
	if memory {
		e := h.access.Manager().Get(id).(*Trader)
		got := tradeState{e.wallet.GetCoins(), e.inventory.GetItems(), e.wallet.GetTransfers(), e.wallet.DirtyTracker().Version()}
		if got != want || e.inventory.GetTransfers() != want.Transfers || e.inventory.DirtyTracker().Version() != want.Version {
			t.Fatalf("memory id=%d state=%+v inventory transfers/version=%d/%d want=%+v", id, got, e.inventory.GetTransfers(), e.inventory.DirtyTracker().Version(), want)
		}
	}
	for _, collection := range []string{"trade_wallets", "trade_inventories"} {
		var doc struct {
			Coins     int64  `bson:"coins"`
			Items     int64  `bson:"items"`
			Transfers int64  `bson:"transfers"`
			Version   uint64 `bson:"_version"`
		}
		if err := h.client.Database(h.database).Collection(collection).FindOne(h.ctx, bson.M{"_id": id}, &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Transfers != want.Transfers || doc.Version != want.Version || collection == "trade_wallets" && doc.Coins != want.Coins || collection == "trade_inventories" && doc.Items != want.Items {
			t.Fatalf("Mongo %s/%d=%+v want=%+v", collection, id, doc, want)
		}
	}
}
func (h *tradeFixture) records(t *testing.T) []coredata.CommitRecord {
	t.Helper()
	var records []coredata.CommitRecord
	if err := h.wal.Replay(h.ctx, func(_ nest.CommitFence, r nest.CommitRecord) error { records = append(records, r); return nil }); err != nil {
		t.Fatal(err)
	}
	return records
}
func waitTradeRecord(t *testing.T, ctx context.Context, pause *tradeProjectionPause, ids []int64) coredata.CommitRecord {
	t.Helper()
	var record coredata.CommitRecord
	select {
	case record = <-pause.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	keys := map[string]bool{}
	for _, m := range record.Mutations {
		keys[fmt.Sprintf("%s/%d", m.Key.Resource, m.Key.ID)] = true
	}
	if len(record.Mutations) != 4 || len(keys) != 4 {
		t.Fatalf("trade mutations=%d unique=%d", len(record.Mutations), len(keys))
	}
	for _, id := range ids {
		for _, resource := range []string{"trade_wallets", "trade_inventories"} {
			if !keys[fmt.Sprintf("%s/%d", resource, id)] {
				t.Fatalf("missing mutation %s/%d", resource, id)
			}
		}
	}
	return record
}
