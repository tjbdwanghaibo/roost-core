package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

type blockedProjectionStore struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockedProjectionStore) Project(ctx context.Context, _ coredata.CommitRecord) error {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func requireDeadline(t *testing.T, call func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- call(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("got %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Error("operation remained blocked after its deadline")
	}
}

// 先用通道确认存储正在执行，再验证另一个调用能取消等待；不靠 sleep 猜测锁时序。
func TestProjectorWaitsRespectDeadline(t *testing.T) {
	for _, method := range []string{"replay", "flush", "second_flush"} {
		t.Run(method, func(t *testing.T) {
			store := &blockedProjectionStore{entered: make(chan struct{}), release: make(chan struct{})}
			p, wal := stoppedProjectorWithRecords(t, store, []coredata.CommitRecord{projectorRecord(91, false)}, 0)
			first := make(chan error, 1)
			go func() {
				if method == "second_flush" {
					first <- p.Flush(context.Background())
				} else {
					_, err := p.ReplayPass(context.Background())
					first <- err
				}
			}()
			<-store.entered
			requireDeadline(t, func(ctx context.Context) error {
				if method == "replay" {
					_, err := p.ReplayPass(ctx)
					return err
				}
				return p.Flush(ctx)
			})
			close(store.release)
			if err := <-first; err != nil {
				t.Fatal(err)
			}
			if err := p.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertWALReplayCount(t, wal, 0)
		})
	}
}

func TestProjectorShutdownDeadlineBehindBackgroundProjection(t *testing.T) {
	opts := nestwal.DefaultOptions(t.TempDir())
	w, err := nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	store := &blockedProjectionStore{entered: make(chan struct{}), release: make(chan struct{})}
	p, err := NewProjector(w, store, ProjectorOptions{CloseWAL: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(context.Background())
	defer close(store.release)
	record := projectorRecord(92, false)
	if err = p.Commit(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	p.TransactionReleased(record.ID)
	<-store.entered
	requireDeadline(t, p.Shutdown)
	if t.Failed() {
		return // 修前复现时让 defer 释放存储，不在清理阶段再次无限等待。
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 取消等待不能把未投影记录确认为完成；真实 WAL 重开后仍可恢复。
	reopened, err := nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	assertWALReplayIDs(t, reopened, []coredata.TransactionID{record.ID})
}

type shutdownBlockedOutbox struct {
	*projectorOutboxFake
	entered, canceled, release chan struct{}
	enterOnce, cancelOnce      sync.Once
}

func (s *shutdownBlockedOutbox) Claim(ctx context.Context, _ string, _ time.Time, _ int, _ time.Duration) ([]OutboxItem, error) {
	s.enterOnce.Do(func() { close(s.entered) })
	<-ctx.Done()
	s.cancelOnce.Do(func() { close(s.canceled) })
	<-s.release
	return nil, ctx.Err()
}

func TestRuntimeConcurrentShutdownHonorsDeadline(t *testing.T) {
	s := &shutdownBlockedOutbox{projectorOutboxFake: newProjectorOutboxFake(), entered: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
	w, err := NewOutboxWorker(s, failingOutboxPublisher{}, OutboxWorkerOptions{Owner: "deadline", Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	w.Start(context.Background())
	<-s.entered
	a := &Assembly{runtime: &Runtime{Outbox: w}}
	first := make(chan error, 1)
	go func() { first <- a.Shutdown(context.Background()) }()
	<-s.canceled // 第一次 Shutdown 已持有运行时关闭所有权并等待 worker。
	requireDeadline(t, a.Shutdown)
	if a.Runtime() == nil {
		t.Error("incomplete shutdown lost its runtime")
	}
	close(s.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.Runtime() != nil {
		t.Fatal("completed shutdown retained runtime")
	}
}
