package directory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

func TestReleaseCannotBypassReservationToken(t *testing.T) {
	dir, _ := newDirectory(t)
	ctx := context.Background()
	claim, err := dir.Reserve(ctx, "name", "owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := dir.Release(ctx, "name", "owner"); !errors.Is(err, ErrClaimStale) {
		t.Fatalf("reserved name released without token: %v", err)
	}
	if _, err := dir.Commit(ctx, claim); err != nil {
		t.Fatalf("reservation lost: %v", err)
	}
}

func TestCASConflictHasRetryableDirectoryCode(t *testing.T) {
	if code,_ := Error(versionstore.ErrConflict); code != 530107 { t.Fatalf("CAS conflict classified as %d, want 530107",code) }
}
func TestReleaseExpiredReservationIsNoOp(t *testing.T) {
	dir, clock := newDirectory(t)
	ctx := context.Background()
	if _, err := dir.Reserve(ctx, "name", "old", time.Minute); err != nil {
		t.Fatal(err)
	}
	clock.advance(2 * time.Minute)
	if err := dir.Release(ctx, "name", "new"); err != nil {
		t.Fatalf("expired reservation conflicts: %v", err)
	}
}
