package entity

import (
	"errors"
	"testing"
)

// RR-20260927-15：SetRemoteVersionVector 在同一 fence 下拒绝 StateVersion 回退（ErrRemoteVersionConflict），
// 更小的 fence 仍是 ErrRemoteFenced；同 fence 的相等 / 更大版本、更大 fence 的任意版本照常写入。
func TestSetRemoteVersionVectorRejectsSameFenceRegression(t *testing.T) {
	current := RemoteVersionVector{StateVersion: 5, MarkerEpoch: 1, LockFence: 7, RouteEpoch: 1}
	for _, tc := range []struct {
		name string
		next RemoteVersionVector
		want error
	}{
		{"same fence, lower version", RemoteVersionVector{StateVersion: 4, MarkerEpoch: 1, LockFence: 7, RouteEpoch: 1}, ErrRemoteVersionConflict},
		{"lower fence", RemoteVersionVector{StateVersion: 6, MarkerEpoch: 1, LockFence: 6, RouteEpoch: 1}, ErrRemoteFenced},
		{"same fence, same version", current, nil},
		{"same fence, higher version", RemoteVersionVector{StateVersion: 6, MarkerEpoch: 1, LockFence: 7, RouteEpoch: 1}, nil},
		{"same fence, marker change", RemoteVersionVector{StateVersion: 5, MarkerEpoch: 2, LockFence: 7, RouteEpoch: 2}, nil},
		{"higher fence, lower version", RemoteVersionVector{StateVersion: 3, MarkerEpoch: 1, LockFence: 8, RouteEpoch: 1}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := NewRemoteEntityBase(1, EntityCategoryNone, false, 0)
			if err := base.SetRemoteVersionVector(current); err != nil {
				t.Fatal(err)
			}
			err := base.SetRemoteVersionVector(tc.next)
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("SetRemoteVersionVector(%+v) = %v, want %v", tc.next, err, tc.want)
			}
			want := current
			if tc.want == nil {
				want = tc.next
			}
			if got := base.RemoteVersionVector(); got != want {
				t.Fatalf("vector=%+v, want %+v", got, want)
			}
		})
	}
}
