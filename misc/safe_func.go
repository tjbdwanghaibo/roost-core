package misc

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

func SafeFunc(f func()) {
	if f == nil {
		return
	}
	defer func() {
		if err := recover(); err != nil {
			slog.Error("SafeFunc panic", "err", err)
		}
	}()
	f()
}

func SafeFuncWrapper(f func()) func() {
	return func() {
		SafeFunc(f)
	}
}

func SafeFuncWithRet[T any](f func() T) (t T) {
	if f == nil {
		return
	}
	defer func() {
		if err := recover(); err != nil {
			slog.Error("SafeFuncWithRet panic", "err", err)
		}
	}()
	t = f()
	return
}

// SafeFuncWithTryCount retries f up to tryCount times (at least once even
// for tryCount <= 0), recovering panics into errors like the other Safe*
// helpers. The returned error wraps the last attempt's failure so callers
// can inspect the real cause instead of a bare "try count exceeded".
func SafeFuncWithTryCount(tryCount int, f func() error) error {
	if f == nil {
		return nil
	}
	if tryCount <= 0 {
		tryCount = 1
	}
	var lastErr error
	for c := 0; c < tryCount; c++ {
		if err := safeCallErr(f); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return fmt.Errorf("misc: %d attempts exhausted: %w", tryCount, lastErr)
}

func safeCallErr(f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return f()
}

func SafeFuncWithExpireCtx(d time.Duration, f func(ctx context.Context)) {
	ctx, rls := context.WithTimeout(context.Background(), d)
	defer rls()
	SafeFunc(func() {
		f(ctx)
	})
}
