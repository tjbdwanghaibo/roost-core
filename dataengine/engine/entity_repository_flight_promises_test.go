package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20260919-08 的第二处：仓库的共享加载也没有 defer。
//
// `loadAggregate` 下面有解码、迁移、`OnInitFinish`——任何一处 panic 都会跳过
// "删除 flight + close(done)"，于是这个聚合从此加载不了：后来的每个请求都等在
// 一个永不关闭的 channel 上。仓库不是只能从 Nest 进来，所以在 Nest 外层 recover
// 盖不住它。

// panickingStore panics inside the read the repository performs.
type panickingStore struct {
	repositoryStore
	panics int
	reads  int
}

func (store *panickingStore) ReadConsistent(ctx context.Context, read func(context.Context) error) error {
	store.reads++
	if store.reads <= store.panics {
		panic("store blew up")
	}
	return store.repositoryStore.ReadConsistent(ctx, read)
}

func TestARepositoryLoadPanicDoesNotWedgeTheAggregate(t *testing.T) {
	ensureDataEngineRepositoryEntity()
	id, err := entity.BuildEntityID(1471, dataEngineRepositoryKind)
	if err != nil {
		t.Fatal(err)
	}
	store := &panickingStore{
		repositoryStore: repositoryStore{docs: map[string][]coredata.RawDocument{
			"repository_profile":   {repositoryRaw(t, "repository_profile", id, 1)},
			"repository_inventory": {repositoryRaw(t, "repository_inventory", id, 1)},
		}},
		panics: 1,
	}
	repository, err := newEntityRepository(entity.NewEntityManager(), store, nil, repositoryGate(true))
	if err != nil {
		t.Fatal(err)
	}

	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("the store panic was swallowed; a failed load must not look like a successful one")
			}
		}()
		_, _ = repository.LoadEntity(context.Background(), id, dataEngineRepositoryKind)
	}()

	fullID, err := entity.NormalizeFullID(id, dataEngineRepositoryKind)
	if err != nil {
		t.Fatal(err)
	}
	repository.flightMu.Lock()
	_, stuck := repository.flights[fullID]
	repository.flightMu.Unlock()
	if stuck {
		t.Fatal("the flight survived the panic; every later load of this aggregate would wait forever")
	}

	// The retry reaches the store again and succeeds.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := repository.LoadEntity(ctx, id, dataEngineRepositoryKind)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the retry after a panic failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the retry hung on the flight the panic left behind")
	}
}

// A waiter attached to the panicking load is released with an error that says
// so, rather than by its own deadline.
func TestRepositoryWaitersOfAPanickingLoadAreReleased(t *testing.T) {
	ensureDataEngineRepositoryEntity()
	id, err := entity.BuildEntityID(1472, dataEngineRepositoryKind)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	store := &blockingPanicStore{entered: entered, release: release}
	repository, err := newEntityRepository(entity.NewEntityManager(), store, nil, repositoryGate(true))
	if err != nil {
		t.Fatal(err)
	}

	leaderDone := make(chan struct{})
	go func() {
		defer func() { _ = recover(); close(leaderDone) }()
		_, _ = repository.LoadEntity(context.Background(), id, dataEngineRepositoryKind)
	}()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	waiter := make(chan error, 1)
	go func() {
		_, err := repository.LoadEntity(ctx, id, dataEngineRepositoryKind)
		waiter <- err
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)
	<-leaderDone

	select {
	case err := <-waiter:
		if err == nil || !strings.Contains(err.Error(), "panic") {
			t.Fatalf("the waiter got %v, want an error naming the panic", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the waiter is still waiting on a flight nobody will close")
	}
}

type blockingPanicStore struct {
	repositoryStore
	entered chan struct{}
	release chan struct{}
	once    bool
}

func (store *blockingPanicStore) ReadConsistent(context.Context, func(context.Context) error) error {
	if !store.once {
		store.once = true
		close(store.entered)
		<-store.release
		panic("store blew up")
	}
	return nil
}
