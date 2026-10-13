package health

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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

// 维护者第十二轮决定（readyz checker 期限）：每个 checker 有自己的期限，到期报 Fail；checker 并发
// 调用，几个同时卡住也只等一个期限；checker 拿到的 ctx 带这个期限，配合 ctx 的 checker 会自己返回。
func TestSnapshotBoundsEveryCheckerByOneDeadline(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	reg := NewRegistry()
	reg.SetCheckTimeout(50 * time.Millisecond)
	stuck := CheckerFunc(func(context.Context) Result { <-release; return Result{} })
	reg.Register("a-stuck", stuck)
	reg.Register("b-stuck", stuck)
	sawDeadline := make(chan bool, 1)
	reg.Register("c-cooperative", CheckerFunc(func(ctx context.Context) Result {
		_, ok := ctx.Deadline()
		sawDeadline <- ok
		return Result{Status: StatusOK}
	}))

	started := time.Now()
	snap := reg.Snapshot(context.Background())
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("two stuck checkers took %s; checkers must run concurrently under one deadline", elapsed)
	}
	if snap.OK {
		t.Fatal("snapshot OK with two stuck checkers")
	}
	for _, result := range snap.Results[:2] {
		if result.Status != StatusFail || !strings.Contains(result.Error, "did not return within") || !strings.Contains(result.Error, "50ms") {
			t.Fatalf("stuck checker %s = %+v, want Fail naming the 50ms limit", result.Name, result)
		}
	}
	if snap.Results[2].Status != StatusOK {
		t.Fatalf("cooperative checker = %+v", snap.Results[2])
	}
	if !<-sawDeadline {
		t.Fatal("the checker's ctx carries no deadline")
	}
}
