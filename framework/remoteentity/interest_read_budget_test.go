package remoteentity

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/internal/stopcontract"
)

type deadlineInterestPublisher struct{ capturingInterestPublisher }

func (*deadlineInterestPublisher) PublishRemoteInterest(ctx context.Context, _ entity.RemoteSnapshotInterest, _ bool) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestInterestOutageDoesNotConsumeSnapshotReadBudget(t *testing.T) {
	step4RegisterDelta(t)
	key := staleBackfillKey(t, b2WatermarkKind, 97400)
	client, err := NewSnapshotClient(nil, SnapshotClientDeps{ConsumerSID: 2790, Loader: func(ctx context.Context, _ entity.RemoteSnapshotKey, _ entity.RemoteReadConsistency, _ uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
		if err := ctx.Err(); err != nil {
			return entity.RemoteSnapshotEnvelope{}, false, err
		}
		return step4Full(key, 7, "v7"), true, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	client.transport = &deadlineInterestPublisher{}
	defer client.Stop(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	got, found, err := client.ReadSnapshot(ctx, entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadMonotonic})
	if err != nil || !found || got.StateVersion != 7 {
		t.Fatalf("interest outage blocked healthy authority: version=%d found=%v err=%v", got.StateVersion, found, err)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("read consumed all its budget: %v", err)
	}
}

// 测试需要观察远端兴趣时显式等后台发布结束；ReadSnapshot 本身不承诺总线已确认。
func waitReadInterests(t *testing.T, client *SnapshotClient) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		client.interestPublishMu.Lock()
		running := client.interestPublishRunning
		client.interestPublishMu.Unlock()
		if !running {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("read interest publication did not drain")
}

type heldReadInterestPublisher struct {
	capturingInterestPublisher
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *heldReadInterestPublisher) PublishRemoteInterest(context.Context, entity.RemoteSnapshotInterest, bool) error {
	p.once.Do(func() { close(p.entered) })
	<-p.release
	return nil
}

func TestReadInterestQueueIsBoundedAndStopDrainsPublisher(t *testing.T) {
	client, err := NewSnapshotClient(nil, SnapshotClientDeps{ConsumerSID: 2791})
	if err != nil {
		t.Fatal(err)
	}
	publisher := &heldReadInterestPublisher{entered: make(chan struct{}), release: make(chan struct{})}
	client.transport = publisher
	stop, released := stopcontract.CallerReleases(client.Stop)
	stopcontract.Check(t, stopcontract.Hooks{
		Block: func(tb testing.TB) {
			key := staleBackfillKey(t, b2WatermarkKind, 97500)
			client.queueReadInterest(key)
			select {
			case <-publisher.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("publisher did not start")
			}
			for i := range interestPublishCapacity + 32 {
				client.queueReadInterest(staleBackfillKey(t, b2WatermarkKind, int64(97501+i)))
			}
			client.interestPublishMu.Lock()
			queued := len(client.interestPublishQueue)
			client.interestPublishMu.Unlock()
			if queued != interestPublishCapacity || client.Stats().LocalInterests != interestPublishCapacity+1 || client.Stats().InterestRejected != 32 {
				t.Fatalf("queue=%d stats=%+v", queued, client.Stats())
			}
		},
		Stop:     stop,
		Release:  func() { close(publisher.release) },
		Released: released,
	})
}
