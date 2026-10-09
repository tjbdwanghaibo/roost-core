package remoteentity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/internal/stopcontract"
)

type heldOutboxScan struct {
	*remoteTestLoader
	entered chan struct{}
	release chan struct{}
}

func (s *heldOutboxScan) PendingRemoteCommits(context.Context, int) ([]entity.RemoteCommitStatus, error) {
	close(s.entered)
	<-s.release // 有意不配合取消，验证停止不能提前交还依赖。
	return nil, nil
}

func TestExplicitOutboxRecoveryIsDrainedBeforeStop(t *testing.T) {
	manager := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	storage := &heldOutboxScan{remoteTestLoader: newRemoteTestLoader(), entered: make(chan struct{}), release: make(chan struct{})}
	manager.SetBackend(storage)
	done := make(chan error, 1)
	stop, released := stopcontract.CallerReleases(manager.StopFinalizer)
	stopcontract.Check(t, stopcontract.Hooks{
		Block: func(t testing.TB) {
			go func() { done <- manager.RecoverOutbox(context.Background()) }()
			select {
			case <-storage.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("recovery did not enter storage")
			}
		},
		Stop:     stop,
		Release:  func() { close(storage.release) },
		Released: released,
	})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := manager.RecoverOutbox(context.Background()); !errors.Is(err, ErrAssemblyStopped) {
		t.Fatalf("recovery after stop: %v", err)
	}
}
