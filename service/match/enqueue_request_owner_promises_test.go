package match

import (
	"context"
	"errors"
	"testing"
)

// U-0164 · C8 · RR-20260909-05：Enqueue 的幂等重放必须先校验归属。
//
// Requests 只按 requestID 查,命中就直接返回原 Ticket。同一个队列里,player:2 用了
// player:1 的 requestID 就会拿到 player:1 的票和分数,而且自己这次入队意图整个消失 ——
// 既没有建立自己的请求记录,也没有进队。读取侧的 Ticket 一直是校验归属的
// （validateOwnership，ErrNotPermitted），写入侧的重放分支漏了同一道检查。
//
// 拒绝而不是改键空间:requestID 由业务给,同队列撞键本身是客户端缺陷,拒绝把它暴露出来;
// 而按 subject 给请求键加命名空间会静默放行,还要迁移已存的键。
func TestEnqueueReplayRefusesARequestIDFromAnotherSubject(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	queue := ranked()

	first := enqueue(t, store, queue, player(1, 100), "shared-request")

	// The owner's own retry still replays: same ticket, no second queue entry.
	replay, err := store.Enqueue(ctx, queue, player(1, 100), "shared-request")
	if err != nil {
		t.Fatalf("the owner's retry must replay, got %v", err)
	}
	if replay.ID != first.ID {
		t.Fatalf("owner retry produced ticket %s, want the original %s", replay.ID, first.ID)
	}

	// Another subject using the same request id is refused, and told nothing
	// about who owns it.
	stolen, err := store.Enqueue(ctx, queue, player(2, 500), "shared-request")
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("cross-subject replay = (%+v, %v), want ErrNotPermitted", stolen, err)
	}
	if stolen.ID != "" {
		t.Fatalf("a refused enqueue returned ticket %s", stolen.ID)
	}

	// The refusal changed nothing: the owner still holds the only ticket, the
	// request still maps to it, and player 2 has no ticket at all.
	if current, ok, err := store.Ticket(ctx, queue, first.ID, player(1, 100)); err != nil || !ok || current.ID != first.ID {
		t.Fatalf("owner's ticket after the refusal = (%s, %v, %v)", current.ID, ok, err)
	}
	candidates, err := store.Candidates(ctx, queue, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != first.ID {
		t.Fatalf("candidates = %+v, want only the owner's ticket %s", candidates, first.ID)
	}

	// Player 2 can still enqueue under its own request id.
	own, err := store.Enqueue(ctx, queue, player(2, 500), "player-2-request")
	if err != nil {
		t.Fatalf("player 2 must still be able to enqueue: %v", err)
	}
	if own.Subject.ID != 2 {
		t.Fatalf("player 2 received a ticket for subject %d", own.Subject.ID)
	}
}
