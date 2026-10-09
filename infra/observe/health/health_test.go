package health

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRegistrySnapshotAggregatesDependencyHealth(t *testing.T) {
	reg := NewRegistry()
	reg.Register("mongo", CheckerFunc(func(context.Context) Result {
		return Result{Status: StatusOK, Message: "connected"}
	}))
	reg.Register("redis", CheckerFunc(func(context.Context) Result {
		return Result{Status: StatusFail, Message: "ping failed", Err: errors.New("timeout")}
	}))

	snap := reg.Snapshot(context.Background())
	if snap.OK {
		t.Fatalf("snapshot should be unhealthy: %+v", snap)
	}
	if len(snap.Results) != 2 || snap.Results[0].Name != "mongo" || snap.Results[1].Name != "redis" {
		t.Fatalf("results should be stable and sorted: %+v", snap.Results)
	}
	if snap.Results[1].Error != "timeout" {
		t.Fatalf("redis error = %q", snap.Results[1].Error)
	}
}

func TestRegistrySnapshotRecoversCheckerPanic(t *testing.T) {
	reg := NewRegistry()
	reg.Register("redis", CheckerFunc(func(context.Context) Result {
		panic("boom")
	}))

	snap := reg.Snapshot(context.Background())
	if snap.OK {
		t.Fatalf("snapshot should be unhealthy after checker panic: %+v", snap)
	}
	if len(snap.Results) != 1 {
		t.Fatalf("results = %+v, want one result", snap.Results)
	}
	got := snap.Results[0]
	if got.Name != "redis" || got.Status != StatusFail {
		t.Fatalf("panic result = %+v, want redis fail", got)
	}
	if !strings.Contains(got.Error, "panic: boom") {
		t.Fatalf("panic error = %q, want panic reason", got.Error)
	}
}

// D1（维护者 2026-10-06 第五轮决定）：Degraded 算可用，只置 Degraded；Fail 与未知状态让 OK 为假。
func TestRegistrySnapshotCountsDegradedAsAvailable(t *testing.T) {
	reg := NewRegistry()
	reg.Register("mongo", CheckerFunc(func(context.Context) Result { return Result{Status: StatusOK} }))
	reg.Register("singleton", CheckerFunc(func(context.Context) Result {
		return Result{Status: StatusDegraded, Message: "renewal outcome unknown"}
	}))
	snap := reg.Snapshot(context.Background())
	if !snap.OK || !snap.Degraded {
		t.Fatalf("snapshot with a degraded checker = ok %v degraded %v, want ok and degraded", snap.OK, snap.Degraded)
	}
	if got := snap.DegradedResults(); len(got) != 1 || got[0].Name != "singleton" || got[0].Message != "renewal outcome unknown" {
		t.Fatalf("DegradedResults = %+v", got)
	}

	reg.Register("custom", CheckerFunc(func(context.Context) Result { return Result{Status: "warn"} }))
	if snap := reg.Snapshot(context.Background()); snap.OK || !snap.Degraded {
		t.Fatalf("unknown status = ok %v degraded %v, want not ok (treated as fail), still degraded", snap.OK, snap.Degraded)
	}
}
