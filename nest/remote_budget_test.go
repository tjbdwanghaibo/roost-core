package nest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// Remote 后端预算拒绝发生于慢阶段；不得执行业务，也不得遗留同 ID 的调度所有权。
func TestRemoteBudgetRejectionReleasesIDAndWorkers(t *testing.T) {
	var prepares, ran atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	closeRelease := sync.OnceFunc(func() { close(release) })
	defer closeRelease()
	manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) {
		if prepares.Add(1) == 1 {
			close(entered)
			<-release
			return nil, entity.ErrRemoteOverloaded
		}
		return &stagedRemoteBatch{}, nil
	}}
	m, id, local := stagedEngine(t, manager)
	first := stagedSend(t, m, id, true, func() { ran.Add(1) })
	stagedSignal(t, entered)
	second := stagedSend(t, m, id, true, func() { ran.Add(1) })
	// 普通慢业务也能回到快池执行，不需要获取 Remote 写额度。
	msg, ch := GenSyncMsg(MsgTypeSingle)
	msg.Name = "staged"
	msg.Tid = local
	msg.Cost = true
	if err := m.dispatcher.TrySendMsg(msg); err != nil {
		closeRelease()
		t.Fatal(err)
	}
	localReply := stagedWait(t, ch)
	closeRelease()
	if localReply != "ok" {
		t.Fatalf("ordinary slow work=%v", localReply)
	}
	if err, ok := stagedWait(t, first).(error); !ok || !errors.Is(err, entity.ErrRemoteOverloaded) {
		t.Fatalf("refusal=%v", err)
	}
	if got := stagedWait(t, second); got != "ok" {
		t.Fatalf("same ID successor=%v", got)
	}
	if ran.Load() != 1 || prepares.Load() != 2 {
		t.Fatalf("ran=%d prepares=%d", ran.Load(), prepares.Load())
	}
}
