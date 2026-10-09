package entity

import (
	"errors"
	"testing"
)

// RR-20260930-13（REMAINING §3 N26，维护者 2026-09-30 拍板）：SetEntityVersion 与 SetRemoteVersionVector 同一规则——它只改 StateVersion、
// 沿用当前 fence，所以“同一 fence 下不回退”（RR-20260927-15）在这个入口就是“不能写更小的 StateVersion”。承诺：更小的版本被拒绝
// （ErrRemoteVersionConflict）、向量不变；相等 / 更大照常写入，其余维度不动。旧行为：不做任何检查，直接 CAS 改写，迟到的调用能把
// 已推进的版本写回旧值。
func TestSetEntityVersionRejectsRewindUnderSameFence(t *testing.T) {
	current := RemoteVersionVector{StateVersion: 5, MarkerEpoch: 1, LockFence: 7, RouteEpoch: 1}
	for _, tc := range []struct {
		name    string
		next    int64
		want    uint64 // 调用后的 StateVersion
		wantErr error
	}{
		{"lower version", 4, 5, ErrRemoteVersionConflict},
		{"negative (treated as 0)", -1, 5, ErrRemoteVersionConflict},
		{"same version", 5, 5, nil},
		{"higher version", 6, 6, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := NewRemoteEntityBase(1, EntityCategoryNone, false, 0)
			if err := base.SetRemoteVersionVector(current); err != nil {
				t.Fatal(err)
			}
			// 修前红测试只用旧 API（无返回值）断言向量不回退；修后 SetEntityVersion 返回 error，这里一并钉住哨兵。
			err := base.SetEntityVersion(tc.next)
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("SetEntityVersion(%d) = %v, want %v", tc.next, err, tc.wantErr)
			}
			got := base.RemoteVersionVector()
			want := current
			want.StateVersion = tc.want
			if got != want {
				t.Fatalf("after SetEntityVersion(%d): vector=%+v, want %+v (a lower StateVersion under the same fence must not be written)", tc.next, got, want)
			}
		})
	}
}
