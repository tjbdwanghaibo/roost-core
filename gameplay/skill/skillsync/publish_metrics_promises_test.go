package skillsync

import (
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncstream"
)

type perObserverGate struct {
	entered  chan int64
	releases map[int64]chan struct{}
}

func (p *perObserverGate) Publish(packet syncstream.Packet) error {
	p.entered <- packet.Observer.ID
	<-p.releases[packet.Observer.ID]
	return nil
}

// RR-20261007-08：两个发布窗口重叠不能重复累加全局 outbox 计数。
func TestConcurrentCoordinatorMetricsCountEachPublishOnce(t *testing.T) {
	publisher := &perObserverGate{entered: make(chan int64, 2), releases: map[int64]chan struct{}{1: make(chan struct{}), 2: make(chan struct{})}}
	projector, _ := NewProjector(1)
	coordinator, err := NewCoordinator(CoordinatorOptions{Runtime: skill.NewRuntime(skill.NewMemoryHost(skill.AuthorityIdentity{}), skill.RuntimeOptions{}), History: syncstream.NewHistory(syncstream.HistoryOptions{}), Publisher: publisher, Projector: projector, Visibility: AllowAllVisibility{}})
	if err != nil {
		t.Fatal(err)
	}
	stream := syncstream.Stream{Topic: TopicState, Key: 1}
	for id := int64(1); id <= 2; id++ {
		if err := coordinator.OpenObserver(syncstream.Observer{ID: id}); err != nil {
			t.Fatal(err)
		}
		if err := coordinator.outbox.Put(syncstream.Packet{Observer: syncstream.Observer{ID: id}, Stream: stream, Epoch: 1, Sequence: 1}); err != nil {
			t.Fatal(err)
		}
	}
	results := []chan error{make(chan error, 1), make(chan error, 1)}
	for i := range results {
		go func(i int) { results[i] <- coordinator.publishDue(syncstream.Observer{ID: int64(i + 1)}, stream) }(i)
	}
	defer func() {
		for _, ch := range publisher.releases {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	}()
	for range 2 {
		select {
		case <-publisher.entered:
		case <-time.After(time.Second):
			t.Fatal("publish did not enter")
		}
	}
	close(publisher.releases[1])
	if err := <-results[0]; err != nil {
		t.Fatal(err)
	}
	close(publisher.releases[2])
	if err := <-results[1]; err != nil {
		t.Fatal(err)
	}
	if got := coordinator.Metrics().Published; got != 2 {
		t.Fatalf("published=%d want 2", got)
	}
}
