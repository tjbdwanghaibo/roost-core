package chat

import (
	"context"
	"errors"
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

// Configuration names shared channels as kind:target. A malformed entry, or
// a pair-scoped kind that needs a participant, stops the process at startup
// rather than being skipped on every tick.
func TestPruneChannelsConfigurationFailsClosed(t *testing.T) {
	for _, bad := range []string{"world", "world:", ":1", "world:abc", "world:-1"} {
		if _, err := parsePruneChannels([]string{bad}); err == nil {
			t.Errorf("entry %q was accepted", bad)
		}
	}
	channels, err := parsePruneChannels([]string{"world:1", " group:42 "})
	if err != nil || len(channels) != 2 || channels[1] != (Channel{Kind: ChannelGroup, Target: 42}) {
		t.Fatalf("parsed %+v err=%v", channels, err)
	}
	if _, err := resolvePruneChannels(&pruneRecorder{}, []Channel{{Kind: ChannelPrivate, Target: 7}}); !errors.Is(err, ErrChannelInvalid) {
		t.Fatalf("a pair-scoped kind in chat.prune_channels was resolved: %v", err)
	}
	refs, err := resolvePruneChannels(&pruneRecorder{}, channels)
	if err != nil || len(refs) != 2 || refs[0].Key() != "world:1" {
		t.Fatalf("refs=%v err=%v", refs, err)
	}

	cfg := modConfig()
	cfg.Set("chat.prune_channels", []string{"world:oops"})
	mod := NewMod(allowAllPolicy{}, NewBodyRegistry(), SystemAuthenticatorFunc(func(context.Context) (SystemToken, error) { return SystemToken{}, fmt.Errorf("no system path") }), nil, nil)
	if err := mod.Init(cfg); err == nil {
		t.Fatal("Init accepted a malformed chat.prune_channels entry")
	}
}
