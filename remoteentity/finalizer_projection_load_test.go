package remoteentity

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

// latencyCountingStorage 统计 finalizer 回源读取，并给每次后端调用加固定延迟以模拟网络往返；
// 两侧（修前/修后）用同一份夹具，只比较 Remote 收尾链路本身的差异。
type latencyCountingStorage struct {
	*MongoCommitter
	latency     time.Duration
	statusReads atomic.Int64
}

func (s *latencyCountingStorage) pause() {
	if s.latency > 0 {
		time.Sleep(s.latency)
	}
}

func (s *latencyCountingStorage) CommitStatus(ctx context.Context, id entity.RemoteTransactionID) (entity.RemoteCommitStatus, error) {
	s.statusReads.Add(1)
	s.pause()
	return s.MongoCommitter.CommitStatus(ctx, id)
}

func (s *latencyCountingStorage) CommitRemote(ctx context.Context, commit entity.RemoteCommit) (entity.RemoteCommitReceipt, error) {
	s.pause()
	return s.MongoCommitter.CommitRemote(ctx, commit)
}

func (s *latencyCountingStorage) MarkRemoteCommitPublished(ctx context.Context, id entity.RemoteTransactionID) error {
	s.pause()
	return s.MongoCommitter.MarkRemoteCommitPublished(ctx, id)
}

func loadEnvInt(name string, fallback int) int {
	if value, err := strconv.Atoi(os.Getenv(name)); err == nil && value > 0 {
		return value
	}
	return fallback
}

// TestRemoteAsyncFinalizerProjectionLoad 是 RR-20260926-38 的前后对照入口（默认跳过）：
// 真实 nestwal WAL + engine.Projector/MongoStore + mongotest，Remote 走正式 Backend + MongoCommitter。
// W 个写者各自在独立实体上连续做 Durability 1 写，统计投影完成吞吐、finalizer 回源读取次数与快照发布次数。
//
//	ROOST_REMOTE_FINALIZE_LOAD=1 ROOST_REMOTE_FINALIZE_WRITES=2000 ROOST_REMOTE_FINALIZE_WRITERS=16 \
//	ROOST_REMOTE_FINALIZE_LATENCY_US=1000 GOWORK=off go test -count=1 -run TestRemoteAsyncFinalizerProjectionLoad -v ./remoteentity
func TestRemoteAsyncFinalizerProjectionLoad(t *testing.T) {
	if os.Getenv("ROOST_REMOTE_FINALIZE_LOAD") != "1" {
		t.Skip("set ROOST_REMOTE_FINALIZE_LOAD=1 to run the finalizer/projection comparison")
	}
	writes := loadEnvInt("ROOST_REMOTE_FINALIZE_WRITES", 2000)
	writers := loadEnvInt("ROOST_REMOTE_FINALIZE_WRITERS", 16)
	latency := time.Duration(loadEnvInt("ROOST_REMOTE_FINALIZE_LATENCY_US", 0)) * time.Microsecond
	// 每个写者轮换的实体数：1 = 同实体连续写（受 gate 持有时间限制）；更大时写入分散到独立实体，
	// 吞吐主要由投影器决定。
	perWriter := loadEnvInt("ROOST_REMOTE_FINALIZE_ENTITIES_PER_WRITER", 1)
	const kind entity.EntityKind = 126
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	mongo := newRemoteMongoFake()
	committer := NewMongoCommitter(mongo, "control", 1000, 0)
	storage := &latencyCountingStorage{MongoCommitter: committer, latency: latency}
	loader := newRemoteTestLoader()
	lives := make([][]*testRemoteEntity, writers)
	for i := range lives {
		for j := range perWriter {
			live := newTestRemoteEntity(int64(26000+i*perWriter+j), 1, kind)
			loader.add(live)
			lives[i] = append(lives[i], live)
		}
	}
	backend, err := NewBackend(loader, storage)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.MaxConcurrentWrites = max(4*writers, writers*perWriter)
	mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	mgr.SetBackend(backend)
	mgr.SetOwnershipStore(committer)
	syncer := &countingSnapshotSyncer{}
	mgr.SetSyncer(syncer)
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = mgr.StopFinalizer(stop)
	})
	store, err := engine.NewMongoStore(mongo, engine.MongoStoreConfig{DefaultDatabase: "game", ServerID: 1000, TransactionReceiptTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetRemoteProjection(committer, mgr); err != nil {
		t.Fatal(err)
	}
	walOptions := nestwal.DefaultOptions(t.TempDir())
	walOptions.WriterVersion = nestwal.WriterVersionV2
	walOptions.GroupCommitInterval = time.Millisecond
	wal, err := nestwal.Open(walOptions)
	if err != nil {
		t.Fatal(err)
	}
	projector, err := engine.NewProjector(wal, store, engine.ProjectorOptions{
		CloseWAL: true, IdlePoll: 5 * time.Millisecond, RetryMin: 5 * time.Millisecond, RetryMax: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closing, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = projector.Close(closing)
	})

	var seq atomic.Uint64
	var failures atomic.Int64
	started := time.Now()
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(owned []*testRemoteEntity) {
			defer wg.Done()
			for i := range writes / writers {
				live := owned[i%len(owned)]
				batch, err := mgr.PrepareRemoteWriteBatch(ctx, []int64{live.GUId()})
				if err != nil {
					failures.Add(1)
					t.Errorf("prepare: %v", err)
					return
				}
				n := seq.Add(1)
				var tx entity.RemoteTransactionID
				tx[0] = 0x38
				binary.BigEndian.PutUint64(tx[8:], n)
				live.dirty.dirty = true
				if err = batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx, "load", "", true, uint8(nest.DurabilityAsync))); err != nil {
					failures.Add(1)
					t.Errorf("finalize: %v", err)
					return
				}
				if err = projector.Commit(ctx, remoteCommitRecord(nest.DurabilityAsync, batch.Commits())); err != nil {
					failures.Add(1)
					t.Errorf("wal: %v", err)
					return
				}
				if _, err = batch.Commit(ctx); err != nil {
					failures.Add(1)
					t.Errorf("commit: %v", err)
					return
				}
				projector.TransactionReleased(coredata.TransactionID(tx))
				if err = batch.Close(ctx); err != nil {
					failures.Add(1)
					t.Errorf("close: %v", err)
					return
				}
			}
		}(lives[w])
	}
	wg.Wait()
	if err = projector.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	for len(mgr.remote.writeSlots) != 0 {
		if ctx.Err() != nil {
			t.Fatalf("write slots not released: %d", len(mgr.remote.writeSlots))
		}
		time.Sleep(time.Millisecond)
	}
	elapsed := time.Since(started)
	total := int64(writes / writers * writers)
	stats := projector.Stats()
	fmt.Printf("RR38LOAD entities_per_writer=%d writes=%d writers=%d latency=%s elapsed=%s tps=%.1f status_reads=%d reads_per_write=%.3f snapshot_publishes=%d publishes_per_write=%.3f failures=%d projection_failures=%d wal_unacked=%d\n",
		perWriter, total, writers, latency, elapsed.Round(time.Millisecond), float64(total)/elapsed.Seconds(), storage.statusReads.Load(),
		float64(storage.statusReads.Load())/float64(total), syncer.published.Load(), float64(syncer.published.Load())/float64(total),
		failures.Load(), stats.ProjectionFailures, stats.WALUnacked)
}
