package nest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// 排空执行器必须等到已准入的多实体 handler 真正结束；取消请求和 SyncWait 都不能提前释放资源。
func TestClientMultiSettledWaitsPastCancellationAndSyncTimeout(t *testing.T) {
	ResetHandlersForTest()
	t.Cleanup(ResetHandlersForTest)
	getter := newMockGetter()
	ids := []int64{mustBuildCastID(t, 8191, entity.EntityCategory(1), nestLocalKind), mustBuildCastID(t, 8192, entity.EntityCategory(1), nestLocalKind)}
	for _, id := range ids {
		getter.Add(newMockEntity(id, entity.EntityCategory(1)))
	}
	engine := NewEngine(NestOptionWithGetter(getter), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 8}, WorkerPoolConfig{}, WorkerPoolConfig{}), NestOptionWithSyncTimeout(time.Millisecond))
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	failure := errors.New("handler finished with rejection")
	name := NewHandlerName("client_multi_settled")
	MustRegisterMemoryHandler(name, func(targets []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		if len(targets) != 2 {
			return nil, errors.New("missing multi entity target")
		}
		close(started)
		<-release
		return nil, failure
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := engine.RequestMultiSettled(ctx, name, ids, nil); done <- err }()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("not admitted: %v", err)
	case <-time.After(time.Second):
		t.Fatal("handler not started")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("released before handler completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	select {
	case err := <-done:
		if !errors.Is(err, failure) {
			t.Fatalf("lost completion error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("completion was not delivered")
	}
	if _, err := engine.RequestMultiSettled(context.Background(), name, nil, nil); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("invalid target admission: %v", err)
	}
}
