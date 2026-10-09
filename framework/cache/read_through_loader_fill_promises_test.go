package cache

import (
	"context"
	"errors"
	"testing"
)

// RR-20261004-04：ReadThrough 的 loader 回填与 L2 回填是同一类 L1 写入口，准入
// 拒绝要按同一条规则处理——ErrStaleWrite / ErrConflictingWrite 交付 L1 已准入的
// 值；L1 没有值时 stale 是 miss、conflict 是拒绝。L2 对 loader 值回写报
// ErrStaleWrite 说明 L2 已有更新的值，也不是读取失败。
// 旧行为（v1.19.0）：loader 分支把任何 setLocal 错误、以及 strict 模式下 L2 的
// stale 拒绝原样返回，`Get` 因为“写被拒”而失败。
func TestReadThroughLoaderFillRefusalIsNotReadFailure(t *testing.T) {
	conflictCfg := staleConfig()
	conflictCfg.Conflict = func(old, next staleValue) bool { return old.Version == next.Version && old.Payload != next.Payload }
	cases := []struct {
		name string
		// local 构造 L1；publish 是 loader 执行期间落到 L1 / L2 的并发写。
		local   func() Store[int, staleValue]
		remote  func() Store[int, staleValue]
		publish func(ctx context.Context, local, remote Store[int, staleValue]) error
		want    staleValue
		wantOK  bool
		wantErr error
	}{
		{
			name:  "local_stale",
			local: func() Store[int, staleValue] { return NewLocalStore(staleConfig()) },
			publish: func(ctx context.Context, l, _ Store[int, staleValue]) error {
				return l.Set(ctx, staleValue{Key: 1, Version: 2, Payload: "published"})
			},
			want: staleValue{Key: 1, Version: 2, Payload: "published"}, wantOK: true,
		},
		{
			name: "atomic_stale",
			local: func() Store[int, staleValue] {
				return NewAtomicLocalStore(AtomicLocalConfig[int, staleValue]{StoreConfig: staleConfig(), Shards: 1})
			},
			publish: func(ctx context.Context, l, _ Store[int, staleValue]) error {
				return l.Set(ctx, staleValue{Key: 1, Version: 2, Payload: "published"})
			},
			want: staleValue{Key: 1, Version: 2, Payload: "published"}, wantOK: true,
		},
		{
			name: "atomic_conflict",
			local: func() Store[int, staleValue] {
				return NewAtomicLocalStore(AtomicLocalConfig[int, staleValue]{StoreConfig: conflictCfg, Shards: 1})
			},
			publish: func(ctx context.Context, l, _ Store[int, staleValue]) error {
				return l.Set(ctx, staleValue{Key: 1, Version: 1, Payload: "published"})
			},
			want: staleValue{Key: 1, Version: 1, Payload: "published"}, wantOK: true,
		},
		{
			// L1 拒绝且读回没有值：stale 表示被更新的删除取代，读到 miss。
			name:  "stale_without_value",
			local: func() Store[int, staleValue] { return &admissionFailingLocal{setErr: ErrStaleWrite} },
		},
		{
			// conflict 且读回没有值：拒绝原样返回，不交付被拒的 loader 值。
			name:    "conflict_without_value",
			local:   func() Store[int, staleValue] { return &admissionFailingLocal{setErr: ErrConflictingWrite} },
			wantErr: ErrConflictingWrite,
		},
		{
			// strict 模式：loader 期间另一个副本把 v2 发布进 L2，回写 v1 被 L2
			// 以 stale 拒绝；L2 已有更新的值，这次读取照常返回 loader 的结果。
			name:   "remote_stale",
			local:  func() Store[int, staleValue] { return NewLocalStore(staleConfig()) },
			remote: func() Store[int, staleValue] { return NewLocalStore(staleConfig()) },
			publish: func(ctx context.Context, _, r Store[int, staleValue]) error {
				return r.Set(ctx, staleValue{Key: 1, Version: 2, Payload: "elsewhere"})
			},
			want: staleValue{Key: 1, Version: 1, Payload: "loaded"}, wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			local := tc.local()
			var remote Store[int, staleValue]
			if tc.remote != nil {
				remote = tc.remote()
			}
			loader := func(ctx context.Context, key int) (staleValue, bool, error) {
				if tc.publish != nil {
					if err := tc.publish(ctx, local, remote); err != nil {
						t.Errorf("concurrent publish: %v", err)
					}
				}
				return staleValue{Key: key, Version: 1, Payload: "loaded"}, true, nil
			}
			store := NewReadThroughStore[int, staleValue](local, remote, loader, staleConfig(), ReadThroughOptions{})
			got, ok, err := store.Get(ctx, 1)
			t.Logf("Get = %+v ok=%v err=%v", got, ok, err)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || ok || got != (staleValue{}) {
					t.Fatalf("want refusal %v without a value, got %+v ok=%v err=%v", tc.wantErr, got, ok, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("read failed with a write admission error: %v", err)
			}
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("Get = %+v ok=%v, want %+v ok=%v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
