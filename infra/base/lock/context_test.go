package lock

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 仅暴露旧Mutex方法集，验证外部实现无需新增方法也能使用取消入口。
type legacyContextMutex struct{ Mutex }

func TestLockContextCancellationPreservesOwnerAndAllowsReuse(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "native"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			mu := NewDefaultMutex(1)
			if legacy {
				mu = legacyContextMutex{mu}
			}
			mu.Lock()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if err := LockContext(ctx, mu); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("blocked lock: %v", err)
			}
			if mu.TryLock() {
				t.Fatal("canceled waiter released another owner's lock")
			}
			mu.Unlock()
			if err := LockContext(context.Background(), mu); err != nil {
				t.Fatal(err)
			}
			mu.Unlock()
		})
	}
}
