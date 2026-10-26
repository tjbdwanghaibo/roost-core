package physicsroom

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
)

// 只复现服务邮箱逻辑：模拟物理执行器正等待一次慢 Commit，尚未消费输入。
func TestMovementMustNotRenewOldJump(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ref := Ref{ID: "review", Epoch: 1}
		member := Member{Room: ref, PlayerID: 1, Token: "review-member"}
		state := &memberState{member: member, body: 1}
		r := &room{ref: ref, members: map[int64]*memberState{1: state}}
		s := &Service{cfg: DefaultConfig(nil), rooms: map[string]*room{"review": r}, metrics: servicemetrics.Wrap(nil)}
		if err := s.Submit(context.Background(), member, Input{Sequence: 1, Jump: true}); err != nil {
			t.Fatal(err)
		}
		jumpAt := state.inputAt
		for seq := uint64(2); seq <= 5; seq++ {
			time.Sleep(100 * time.Millisecond)
			if err := s.Submit(context.Background(), member, Input{Sequence: seq}); err != nil {
				t.Fatal(err)
			}
		}
		if time.Since(jumpAt) <= s.cfg.InputTTL {
			t.Fatal("probe did not cross original deadline")
		}
		if state.jumpPending && time.Since(state.inputAt) <= s.cfg.InputTTL {
			t.Fatalf("expired jump remains executable: jump age=%v, latest movement age=%v, TTL=%v", time.Since(jumpAt), time.Since(state.inputAt), s.cfg.InputTTL)
		}
	})
}
