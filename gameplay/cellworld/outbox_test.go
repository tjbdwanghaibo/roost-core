package cellworld

import (
	"context"
	"errors"
	fentity "github.com/tjbdwanghaibo/roost-core/framework/entity"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type controlledTransport struct {
	Transport
	publish func(string, []byte) error
}

func (t controlledTransport) Publish(subject string, data []byte) error {
	return t.publish(subject, data)
}

func TestBlockedNetworkDoesNotBlockLocalWorkAndDrainWaits(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p := startProc(t, procSpec{sid: 201, base: true, grid: worldGrid(t, 1, 1), transport: controlledTransport{
		Transport: newLoopback(), publish: func(string, []byte) error { once.Do(func() { close(entered) }); <-release; return nil },
	}})
	defer p.stop()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	if !p.node.SendExt(202, "probe", []byte("x")) {
		t.Fatal("enqueue")
	}
	<-entered
	local := make(chan struct{})
	if !p.node.enqueue(job{kind: jobCall, call: func(context.Context) { close(local) }}) {
		t.Fatal("local enqueue")
	}
	select {
	case <-local:
	case <-time.After(time.Second):
		t.Fatal("network blocked local worker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.node.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("premature drain: %v", err)
	}
	if p.node.SendExt(202, "probe", nil) {
		t.Fatal("draining node admitted a new message")
	}
	close(release)
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := p.node.Drain(ctx2); err != nil {
		t.Fatal(err)
	}
	if p.node.Stats().OutboundPending != 0 {
		t.Fatal("did not settle output")
	}
}

func TestBlockedDestroyDoesNotBlockNetwork(t *testing.T) {
	entered, release, received := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	p := startProc(t, procSpec{sid: 201, base: true, grid: worldGrid(t, 1, 1), transport: controlledTransport{Transport: newLoopback(), publish: func(string, []byte) error { once.Do(func() { close(received) }); return nil }}, mod: func(cfg *NodeConfig) {
		cfg.Destroy = func(context.Context, fentity.IThreadSafeEntity) error { close(entered); <-release; return nil }
	}})
	defer p.stop()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	e := &testActor{EntityBase: fentity.NewEntityBase(actorID(1), fentity.EntityCategoryOther, true, kindTActor)}
	if !p.node.enqueue(job{kind: jobDestroy, entity: e}) {
		t.Fatal("destroy enqueue")
	}
	<-entered
	if !p.node.enqueue(job{kind: jobDestroy, entity: e}) {
		t.Fatal("duplicate should join the same destroy")
	}
	if !p.node.SendExt(202, "probe", nil) {
		t.Fatal("send enqueue")
	}
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("destroy blocked network")
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.node.Drain(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestControlRetryHasAFiniteBudget(t *testing.T) {
	var calls atomic.Int32
	p := startProc(t, procSpec{sid: 201, base: true, grid: worldGrid(t, 1, 1), transport: controlledTransport{Transport: newLoopback(), publish: func(string, []byte) error { calls.Add(1); return errors.New("offline") }}, mod: func(cfg *NodeConfig) { cfg.PublishRetry = time.Millisecond }})
	defer p.stop()
	if !p.node.SendExt(202, "probe", nil) {
		t.Fatal("enqueue")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.node.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || p.node.Stats().ControlRetries != 2 || p.node.Stats().ControlExhausted != 1 {
		t.Fatalf("calls=%d stats=%+v", calls.Load(), p.node.Stats())
	}
}

func TestStateCoalescingPreservesVersionAndTombstone(t *testing.T) {
	n := &Node{cfg: NodeConfig{OutboxSize: 1}}
	n.network.init()
	update := func(v Version) {
		n.enqueueSend(outgoing{sid: 102, msg: &wireMsg{Kind: kindGhost, Ghost: &ghostBatch{Updates: []EntityState{{ID: 1, Ver: v}}}}})
	}
	remove := func(v Version) {
		n.enqueueSend(outgoing{sid: 102, msg: &wireMsg{Kind: kindGhost, Ghost: &ghostBatch{Removes: []ghostRemove{{ID: 1, Ver: v}}}}})
	}
	update(Version{Epoch: 1, Seq: 1})
	remove(Version{Epoch: 1, Seq: 2})
	update(Version{Epoch: 1, Seq: 1})
	update(Version{Epoch: 1, Seq: 2})
	batch := n.network.take(1)
	if len(batch) != 1 || len(batch[0].out.msg.Ghost.Removes) != 1 || n.outputPending.Load() != 1 {
		t.Fatalf("lost tombstone: %+v", batch)
	}
	n.outputPending.Add(-1)
	update(Version{Epoch: 2, Seq: 1})
	remove(Version{Epoch: 1, Seq: 3})
	batch = n.network.take(1)
	if len(batch) != 1 || len(batch[0].out.msg.Ghost.Updates) != 1 {
		t.Fatalf("old remove beat new generation: %+v", batch)
	}
}
