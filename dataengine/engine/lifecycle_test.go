package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

func lifecycleAssembly(t *testing.T, client fmongo.IMongo, js fnats.IJetStream) *Assembly {
	t.Helper()
	cfg := AssemblyConfig{
		Mongo: MongoStoreConfig{DefaultDatabase: "game", ServerID: 1},
		WAL:   nestwal.DefaultOptions(t.TempDir()), Projector: DefaultProjectorOptions(),
		Outbox: OutboxWorkerOptions{Owner: "lifecycle"},
	}
	cfg.WAL.WriterVersion = nestwal.WriterVersionV2
	a, err := Assemble(AssemblyDeps{Mongo: client, JetStream: js, Access: entity.NewManagerAccess(entity.NewEntityManager())}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return a
}

func TestRuntimeRepeatedStartPreservesReadyAndOutbox(t *testing.T) {
	a := lifecycleAssembly(t, mongotest.NewClient(), &recordingJetStream{})
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := a.Runtime()
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !r.Ready() {
		t.Fatal("repeated Start dismantled ready runtime")
	}
	select {
	case <-r.Outbox.done:
		t.Fatal("repeated Start closed outbox")
	default:
	}
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err == nil {
		t.Fatal("stopped runtime became ready again")
	}
}

func TestAssemblyConcurrentStartsShareRuntime(t *testing.T) {
	a := lifecycleAssembly(t, mongotest.NewClient(), &recordingJetStream{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := a.Start(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if a.Runtime() == nil || !a.Runtime().Ready() {
		t.Fatal("runtime is not ready")
	}
}

type parkedStream struct {
	recordingJetStream
	entered, release chan struct{}
}

func (s *parkedStream) EnsureStream(ctx context.Context, _ fnats.JetStreamConfig) error {
	close(s.entered)
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func TestAssemblyShutdownWaitsForStartingOwner(t *testing.T) {
	js := &parkedStream{entered: make(chan struct{}), release: make(chan struct{})}
	a := lifecycleAssembly(t, mongotest.NewClient(), js)
	started := make(chan error, 1)
	go func() { started <- a.Start(context.Background()) }()
	<-js.entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := a.Shutdown(ctx)
	cancel()
	close(js.release)
	if startErr := <-started; startErr != nil {
		t.Fatal(startErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown while starting=%v, want deadline", err)
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.Runtime() != nil {
		t.Fatal("shutdown did not release runtime")
	}
}

type cancelProjectionMongo struct {
	fmongo.IMongo
	cancel context.CancelFunc
}

func (c cancelProjectionMongo) Database(name string) fmongo.IDatabase {
	return cancelProjectionDB{c.IMongo.Database(name), c.cancel}
}

type cancelProjectionDB struct {
	fmongo.IDatabase
	cancel context.CancelFunc
}

func (d cancelProjectionDB) Collection(name string) fmongo.ICollection {
	coll := d.IDatabase.Collection(name)
	if name == "heroes" {
		return cancelProjectionCollection{coll, d.cancel}
	}
	return coll
}

type cancelProjectionCollection struct {
	fmongo.ICollection
	cancel context.CancelFunc
}

func (c cancelProjectionCollection) FindOneAndUpdate(context.Context, any, any, any, ...fmongo.FindOneAndUpdateOption) error {
	c.cancel()
	return context.Canceled
}
func TestAssemblyCanceledRecoveryRetainsCleanupOwnership(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := lifecycleAssembly(t, cancelProjectionMongo{mongotest.NewClient(), cancel}, &recordingJetStream{})
	wal, err := nestwal.Open(a.cfg.WAL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wal.Append(context.Background(), projectorRecord(17, false)); err != nil {
		t.Fatal(err)
	}
	if err := wal.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start=%v", err)
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := nestwal.Open(a.cfg.WAL)
	if err != nil {
		t.Fatalf("failed startup leaked WAL: %v", err)
	}
	defer reopened.Close(context.Background())
	assertWALReplayCount(t, reopened, 1)
}
func TestAssemblyOwnsWALWhenProjectorDoesNot(t *testing.T) {
	a := lifecycleAssembly(t, mongotest.NewClient(), &recordingJetStream{})
	a.cfg.Projector.CloseWAL = false
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := nestwal.Open(a.cfg.WAL)
	if err != nil {
		t.Fatalf("assembly leaked its WAL: %v", err)
	}
	reopened.Close(context.Background())
}

func TestAssemblyFailedConstructionCanRetry(t *testing.T) {
	for _, stage := range []string{"projector", "outbox"} {
		t.Run(stage, func(t *testing.T) {
			a := lifecycleAssembly(t, mongotest.NewClient(), &recordingJetStream{})
			good := a.cfg
			if stage == "projector" {
				a.cfg.Projector.RetryMin = time.Second
				a.cfg.Projector.RetryMax = time.Millisecond
			} else {
				a.cfg.Outbox.Owner = ""
			}
			if err := a.Start(context.Background()); err == nil {
				t.Fatal("invalid construction succeeded")
			}
			if a.Runtime() != nil {
				t.Fatal("completed failed-start cleanup retained runtime")
			}
			a.cfg = good
			if err := a.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
