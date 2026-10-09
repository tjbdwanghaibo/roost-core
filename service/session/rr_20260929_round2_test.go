package session

import (
	"context"
	"errors"
	"fmt"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	driver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestReviewAttachCannotAssertAlreadyReleased(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	run := mustEnter(t, h, 1, "forge")
	resource := scene("held")
	resource.ReleasedAtUnix = h.clock.Now().Unix()
	resource.ForcedRelease = true
	if _, err := h.service.Attach(ctx, 1, run.ID, resource); !errors.Is(err, ErrRunInvalid) {
		t.Fatalf("forged release markers accepted: %v", err)
	}
	if _, err := h.service.Attach(ctx, 1, run.ID, scene("held")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Finish(ctx, 1, run.ID, StateSucceeded, "done"); err != nil {
		t.Fatal(err)
	}
	if h.releaser.count("scene", "held") != 1 {
		t.Fatalf("caller-controlled release marker skipped release: calls=%d", h.releaser.count("scene", "held"))
	}
}

func TestReviewSuccessfulFinishRetryFreesClaim(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	run := mustEnter(t, h, 1, "retry")
	if _, err := h.service.Attach(ctx, 1, run.ID, scene("retry")); err != nil {
		t.Fatal(err)
	}
	h.releaser.failFor["scene:retry"] = 1
	if _, err := h.service.Finish(ctx, 1, run.ID, StateSucceeded, "done"); err == nil {
		t.Fatal("expected first release failure")
	}
	if _, err := h.service.Finish(ctx, 1, run.ID, StateSucceeded, "done"); err != nil {
		t.Fatal(err)
	}
	_, found, err := h.claims.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("successful Finish retry leaves terminal owner's claim")
	}
}

type reviewPausedClaimDelete struct {
	ClaimStore
	once             atomic.Bool
	entered, release chan struct{}
}

func (s *reviewPausedClaimDelete) DeleteIf(ctx context.Context, k int64, v versionstore.Versioned[Claim], match func(Claim) bool) error {
	if s.once.CompareAndSwap(false, true) {
		close(s.entered)
		<-s.release
	}
	return s.ClaimStore.DeleteIf(ctx, k, v, match)
}
func TestReviewTerminalFinishCannotDeleteReacquiredClaim(t *testing.T) {
	gate := &reviewPausedClaimDelete{ClaimStore: versionstore.NewMemoryStore[int64, Claim](), entered: make(chan struct{}), release: make(chan struct{})}
	h := newHarness(t, func(c *Config) {
		if addr := os.Getenv("ROOST_REVIEW_REDIS"); addr != "" {
			client, err := driver.NewClient(fredis.DefaultConfig(addr))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			stores, err := NewRedisStores(client, RedisConfig{Prefix: fmt.Sprintf("service-review-b:%d", time.Now().UnixNano()), RequestTTL: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			c.Runs = stores.Runs
			c.Requests = stores.Requests
			gate.ClaimStore = stores.Claims
			t.Log("real Redis backend")
		}
		c.Claims = gate
	})
	ctx := context.Background()
	a := mustEnter(t, h, 1, "a")
	done := make(chan error, 1)
	go func() { _, err := h.service.Finish(ctx, 1, a.ID, StateSucceeded, "done"); done <- err }()
	<-gate.entered
	b, bErr := h.service.Enter(ctx, 1, enterReq("b"))
	close(gate.release)
	aErr := <-done
	if bErr != nil || aErr != nil {
		t.Fatal(bErr, aErr)
	}
	c, cErr := h.service.Enter(ctx, 1, enterReq("c"))
	bStored, bFound, err := h.service.cfg.Runs.Get(ctx, b.ID)
	if err != nil || !bFound {
		t.Fatal(err)
	}
	if cErr == nil && bStored.Value.Live(h.clock.Now().Unix()) {
		t.Fatalf("late Finish deleted replacement claim: b=%s c=%s both live", b.ID, c.ID)
	}
}
