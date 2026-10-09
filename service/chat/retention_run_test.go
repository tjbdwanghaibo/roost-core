package chat

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// pruneRecorder is a Store that only knows how to Resolve and Prune — the
// two calls the retention loop makes. Anything else is a bug in the loop and
// panics on the nil embedded interface.
type pruneRecorder struct {
	Store
	mu     sync.Mutex
	pruned []string
	seen   chan struct{}
}

func (p *pruneRecorder) Resolve(ch Channel, participant int64) (ChannelRef, error) {
	if ch.Kind == ChannelPrivate {
		return ChannelRef{}, fmt.Errorf("%w: kind %q needs a participant", ErrChannelInvalid, ch.Kind)
	}
	return ChannelRef{Kind: ch.Kind, key: fmt.Sprintf("%s:%d", ch.Kind, ch.Target)}, nil
}

func (p *pruneRecorder) Prune(_ context.Context, ref ChannelRef, limit int) (int, error) {
	p.mu.Lock()
	p.pruned = append(p.pruned, ref.Key())
	p.mu.Unlock()
	select {
	case p.seen <- struct{}{}:
	default:
	}
	return 1, nil
}

// The retention loop prunes what the deployment enumerated — and nothing when
// nothing was enumerated. Until U-0022 the loop iterated over a method that
// returned a hardcoded nil, so Prune was never called by any process and
// retention_age was a setting with no executor.
func TestTheRetentionLoopPrunesTheEnumeratedChannels(t *testing.T) {
	previous := pruneEvery
	pruneEvery = 5 * time.Millisecond
	t.Cleanup(func() { pruneEvery = previous })

	recorder := &pruneRecorder{seen: make(chan struct{}, 1)}
	world, _ := recorder.Resolve(Channel{Kind: ChannelWorld, Target: 1}, 0)
	service, err := NewService(ServiceConfig{Store: recorder, PruneChannels: func(context.Context) []ChannelRef {
		return []ChannelRef{world}
	}})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{service: service}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.run(ctx) }()
	select {
	case <-recorder.seen:
	case <-time.After(5 * time.Second):
		t.Fatal("the retention loop never pruned the enumerated channel")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.pruned) == 0 || recorder.pruned[0] != "world:1" {
		t.Fatalf("pruned %v, want world:1", recorder.pruned)
	}
}

