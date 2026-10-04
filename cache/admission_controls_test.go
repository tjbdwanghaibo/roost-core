package cache

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// RR-20261004-NC-13～15：验证fatal与普通故障分流，以及拒绝后的读取失败不交付旧值。
func TestCacheAdmissionRemoteErrorCompatibility(t *testing.T) {
	for _, operation := range []string{"get", "delete"} {
		for _, mode := range []string{"fatal_ignore", "fatal_strict", "outage_ignore", "outage_strict", "unclassified"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				cfg := staleConfig()
				local := NewLocalStore(cfg)
				remote := &admissionPolicyRemote{Store: NewLocalStore(cfg)}
				cause := errors.New("outage")
				fatal := mode == "fatal_ignore" || mode == "fatal_strict"
				if fatal || mode == "unclassified" {
					cause = ErrConflictingWrite
				}
				remoteErr := fmt.Errorf("remote: %w", cause)
				if operation == "get" {
					remote.getErr = remoteErr
				} else {
					remote.deleteErr = remoteErr
					if err := local.Set(ctx, staleValue{Key: 1, Version: 2}); err != nil {
						t.Fatal(err)
					}
				}
				loads := 0
				opts := ReadThroughOptions{IgnoreRemoteError: mode != "fatal_strict" && mode != "outage_strict"}
				if mode != "unclassified" {
					opts.FatalRemoteError = func(err error) bool { return errors.Is(err, ErrConflictingWrite) }
				}
				store := NewReadThroughStore[int, staleValue](local, remote, func(context.Context, int) (staleValue, bool, error) {
					loads++
					return staleValue{Key: 1, Version: 3}, true, nil
				}, cfg, opts)
				var err error
				if operation == "get" {
					_, _, err = store.Get(ctx, 1)
				} else {
					err = store.Delete(ctx, 1)
				}
				wantError := fatal || !opts.IgnoreRemoteError
				if wantError && !errors.Is(err, cause) || !wantError && err != nil {
					t.Fatalf("err=%v wantError=%v", err, wantError)
				}
				_, held, getErr := local.Get(ctx, 1)
				if getErr != nil {
					t.Fatal(getErr)
				}
				if operation == "get" {
					if wantError && (loads != 0 || held) {
						t.Fatal("rejected read caused fallback")
					}
					if !wantError && (loads != 1 || !held) {
						t.Fatal("outage fallback was disabled")
					}
				} else if held != fatal {
					t.Fatalf("L1 held=%v, want fatal=%v", held, fatal)
				}
			})
		}
	}
}

type admissionFailingLocal struct {
	Store[int, staleValue]
	setErr, getErr error
	// cleanGets 是前几次 Get 正常返回 miss 的次数，之后才返回 getErr。
	cleanGets, gets int
}

func (s *admissionFailingLocal) Set(context.Context, staleValue) error { return s.setErr }
func (s *admissionFailingLocal) Get(context.Context, int) (staleValue, bool, error) {
	s.gets++
	if s.gets <= s.cleanGets {
		return staleValue{}, false, nil
	}
	return staleValue{}, false, s.getErr
}

func TestCacheAdmissionLayeredRejectedReadFailures(t *testing.T) {
	for _, mode := range []string{"stale_read_error", "conflict_read_error", "conflict_miss", "outage"} {
		t.Run(mode, func(t *testing.T) {
			cfg := staleConfig()
			ctx := context.Background()
			remote := NewLocalStore(cfg)
			if err := remote.Set(ctx, staleValue{Key: 1, Version: 1, Payload: "rejected"}); err != nil {
				t.Fatal(err)
			}
			readErr := errors.New("L1 read failure")
			local := &admissionFailingLocal{setErr: fmt.Errorf("wrapped: %w", ErrConflictingWrite)}
			if mode == "stale_read_error" {
				local.setErr = fmt.Errorf("wrapped: %w", ErrStaleWrite)
			}
			if mode == "stale_read_error" || mode == "conflict_read_error" {
				local.getErr = readErr
			}
			if mode == "outage" {
				local.setErr = errors.New("L1 unavailable")
			}
			store := NewLayeredStore[int, staleValue](local, remote, 0, cfg)
			if mode == "stale_read_error" {
				// RR-20261004-02：stale 拒绝只在 L1 窗口有效时才读回 L1；窗口外
				// （含 ttl≤0）过期副本没有否决权，交付权威值。这里验证窗口内读回
				// 失败的情形：第一次 L1 检查正常 miss，回填被拒后的读回失败。
				store = NewLayeredStore[int, staleValue](local, remote, time.Minute, cfg)
				store.setLocalExpiry(1, time.Now())
				local.cleanGets = 1
			}
			got, held, err := store.Get(ctx, 1)
			if mode == "outage" {
				if err != nil || !held || got.Payload != "rejected" {
					t.Fatalf("ordinary backfill failure blocked read: %+v %v %v", got, held, err)
				}
				return
			}
			if held || got != (staleValue{}) || !errors.Is(err, local.setErr) {
				t.Fatalf("refused value escaped: %+v %v %v", got, held, err)
			}
			if local.getErr != nil && !errors.Is(err, readErr) {
				t.Fatalf("read cause lost: %v", err)
			}
		})
	}
}
