//go:build integration

// RR-20261005-NC-172：真实 nats-server（私有进程）上，JetStream 同步总线的 handler 卡住时停机。旧行为：
// SyncBusMod.StopWithContext 立即返回 nil 并释放总线，按 App 的停机顺序 NatsMod 随即关连接，handler 返回后
// ack 失败，消息留在 ack_pending、AckWait 后重投。承诺：预算内 handler 未返回时 Mod 返回 ctx 错误、保留总线
// （App 因此不关 NATS）；handler 返回后重试成功，这条投递在连接仍在时被确认。
package syncbus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus"
)

func TestRealJetStreamSyncBusStopDrainsAnInFlightHandler(t *testing.T) {
	url := startPrivateJetStream(t)
	subscriber, err := startMigrationNode(t, url, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}, 1), make(chan struct{})
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	if _, err := subscriber.bus.Subscribe(migrationTopic, func(context.Context, *fsyncbus.SyncMsg) error {
		entered <- struct{}{}
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	publisher, err := startMigrationNode(t, url, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	publishMigration(t, publisher.bus, 1)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not receive the message")
	}

	// App 的停机顺序：SyncBusMod 先停；它报告停完（nil）App 才停 NatsMod。
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	stopErr := subscriber.sync.StopWithContext(ctx)
	busRetained := subscriber.sync.bus != nil
	natsClosed := false
	if stopErr == nil {
		_ = subscriber.nats.StopWithContext(context.Background())
		natsClosed = true
	}
	t.Logf("budgeted stop with the handler in flight: err=%v bus_retained=%v nats_closed=%v", stopErr, busRetained, natsClosed)
	close(release)
	released = true
	var retryErr error
	if !natsClosed {
		retryCtx, cancelRetry := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelRetry()
		retryErr = subscriber.sync.StopWithContext(retryCtx)
	}

	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	infoCtx, cancelInfo := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelInfo()
	stream, err := js.Stream(infoCtx, "ZZMIGRATE_SYNC")
	if err != nil {
		t.Fatal(err)
	}
	var ackPending int
	var ackFloor uint64
	consumers := 0
	lister := stream.ListConsumers(infoCtx)
	for info := range lister.Info() {
		if info.Config.FilterSubject != "zzmigrate.sync."+migrationTopic {
			continue
		}
		consumers++
		ackPending, ackFloor = info.NumAckPending, info.AckFloor.Consumer
	}
	t.Logf("retry stop: err=%v; consumer ack_pending=%d ack_floor=%d consumers=%d", retryErr, ackPending, ackFloor, consumers)
	if !errors.Is(stopErr, context.DeadlineExceeded) || !busRetained || natsClosed {
		t.Fatal("the stop reported success with the handler in flight, so the connection was closed under it")
	}
	if retryErr != nil || ackPending != 0 || ackFloor != 1 {
		t.Fatal("after the handler returned the retry did not finish the stop, or the delivery was not acknowledged")
	}
}
