package session

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestConfiguredOwnerSourceReleasesExpiredRun(t *testing.T) {
	calls := 0
	h := newHarness(t, func(c *Config) {
		c.Owners = OwnerSourceFunc(func(ctx context.Context, limit int) ([]int64, error) {
			calls++
			if limit != 1 {
				t.Fatalf("limit=%d", limit)
			}
			return []int64{7}, nil
		})
	})
	ctx := context.Background()
	run := mustEnter(t, h, 7, "idle")
	if _, err := h.service.Attach(ctx, 7, run.ID, scene("idle")); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(11 * time.Minute)
	resolved, err := h.service.SweepPending(ctx, 1)
	if err != nil || calls != 1 || len(resolved) != 1 || resolved[0].State != StateExpired || h.releaser.count("scene", "idle") != 1 {
		t.Fatalf("resolved=%+v calls=%d releases=%d err=%v", resolved, calls, h.releaser.count("scene", "idle"), err)
	}
	if _, found, err := h.claims.Get(ctx, 7); found || err != nil {
		t.Fatal(found, err)
	}
}

func TestOwnerSourceIsBoundedAndFailuresAreRetryable(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("roster unavailable")
	attempt := 0
	h := newHarness(t, func(c *Config) {
		c.Owners = OwnerSourceFunc(func(context.Context, int) ([]int64, error) {
			attempt++
			if attempt == 1 {
				return nil, sentinel
			}
			return []int64{1, 2}, nil
		})
	})
	run := mustEnter(t, h, 1, "live")
	if _, err := h.service.SweepPending(ctx, 1); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if _, err := h.service.SweepPending(ctx, 1); !errors.Is(err, ErrRangeInvalid) {
		t.Fatal(err)
	}
	stored, _, err := h.runs.Get(ctx, run.ID)
	if err != nil || stored.Value.State != StateOpen {
		t.Fatal(stored, err)
	}
	lazy := newHarness(t)
	if lazy.service.BackgroundSweepEnabled() {
		t.Fatal("unconfigured source enabled")
	}
	if result, err := lazy.service.SweepPending(ctx, 1); len(result) != 0 || err != nil {
		t.Fatal(result, err)
	}
}
