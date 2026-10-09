package health

import (
	"context"
	"strings"
	"testing"
	"time"
)

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
