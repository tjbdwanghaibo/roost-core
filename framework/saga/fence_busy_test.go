package saga

// 分区发布遇到“期望序号检查被拒、但没有别的写者”时不能误判为被隔离（2026-11-07 v1.42.1 发版前 Saga 整轮压测，单写者下
// mongo-forward c128 一档 fail-stop：`partition 1 expected last event 11463: nats log: wrong last sequence (err_code=10164)`）。
//
// 服务端（nats-server 2.14.5）在 apply 路径上先存消息、回 PubAck（jetstream_cluster.go processJetStreamMsg），之后才删“这个
// 期望序号过滤正在处理”的标记（同文件 expectedPerSubjectInProcess）。协调器收到上一轮的 PubAck 立刻发下一轮，入站检查
// （jetstream_batching.go）看到标记还在就回 10164，这次没有写入。旧实现把它与 10071（序号真的不符）一样处理：读过滤上的最后一条，
// 那是自己上一轮的末条、不是这一块的 → ErrPartitionFenced → 整个进程 fail-stop。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

// fenceLog 是只有一个分区事件过滤的内存日志：PublishFenced 按 fence.LastSeq 与过滤上最后一条的序号比较（不符返回 10071），
// busy 次之前的发布直接返回 10164、不写入；intruder 非空时在第一次 10164 之后把它写进过滤（别的写者的提案随后落库）。
type fenceLog struct {
	fnats.JetStreamLog
	mu       sync.Mutex
	last     fnats.LogMsg
	busy     int
	intruder *fnats.LogMsg
	calls    int
}

func (l *fenceLog) PublishFenced(_ context.Context, msgs []fnats.LogMsg, fence fnats.LogFence) (fnats.LogPublishAck, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.busy > 0 {
		l.busy--
		if l.intruder != nil {
			l.last, l.intruder = *l.intruder, nil
		}
		return fnats.LogPublishAck{}, fmt.Errorf("%w (err_code=10164: wrong last sequence)", fnats.ErrLogWrongLastSequence)
	}
	if fence.LastSeq != l.last.Seq {
		return fnats.LogPublishAck{}, fmt.Errorf("%w (err_code=10071: wrong last sequence: %d)", fnats.ErrLogWrongLastSequence, l.last.Seq)
	}
	l.last = fnats.LogMsg{Seq: l.last.Seq + uint64(len(msgs)), MsgID: msgs[len(msgs)-1].MsgID}
	return fnats.LogPublishAck{Seq: l.last.Seq, Count: len(msgs)}, nil
}

func (l *fenceLog) LastMsg(context.Context, string, string) (fnats.LogMsg, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last.Seq == 0 {
		return fnats.LogMsg{}, fnats.ErrLogNotFound
	}
	return l.last, nil
}

func TestPartitionPublishRetriesAFenceCheckRejectedWithoutAnotherWriter(t *testing.T) {
	const previous = 11463 // 自己上一轮的末条
	chunk := []fnats.LogMsg{{Subject: "unit.saga.0.ev.none", MsgID: "unit.0.11463.7.0"}, {Subject: "unit.saga.0.ev.done", MsgID: "unit.0.11463.7.1"}}
	cases := []struct {
		name      string
		log       *fenceLog
		wantSeq   uint64
		wantFence bool
		wantBusy  uint64
	}{
		{
			// 服务端还在处理自己上一轮：过滤上最后一条仍是期望序号，重发后写入。
			name:    "own previous proposal still in process",
			log:     &fenceLog{last: fnats.LogMsg{Seq: previous, MsgID: "unit.0.11460.6.2"}, busy: 2},
			wantSeq: previous + 2, wantBusy: 2,
		},
		{
			// 被拒时别的写者的提案在处理，随后落库：最后一条序号已前进且不是自己的 → 隔离。
			name:      "another writer's proposal lands",
			log:       &fenceLog{last: fnats.LogMsg{Seq: previous, MsgID: "unit.0.11460.6.2"}, busy: 1, intruder: &fnats.LogMsg{Seq: previous + 1, MsgID: "other.0.11463.1.0"}},
			wantFence: true,
		},
		{
			// 别的写者早已写进来（10071）：照旧隔离。
			name:      "another writer already advanced the filter",
			log:       &fenceLog{last: fnats.LogMsg{Seq: previous + 3, MsgID: "other.0.11463.1.2"}},
			wantFence: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := newUnitPartition(t, func(o *Options) { o.PublishTimeout, o.PublishUnknownBudget = time.Second, 2*time.Second })
			u.e.log = tc.log
			seq, err := u.p.publishChunk(context.Background(), chunk, fnats.LogFence{Filter: u.e.layout.EventFilter(0), LastSeq: previous})
			if tc.wantFence {
				if !errors.Is(err, ErrPartitionFenced) {
					t.Fatalf("publish = %d, %v; want ErrPartitionFenced (another writer advanced the filter)", seq, err)
				}
				return
			}
			if err != nil || seq != tc.wantSeq {
				t.Fatalf("publish = %d, %v after %d call(s); want seq %d: the fence check was rejected while no other writer had written (last event still %d)",
					seq, err, tc.log.calls, tc.wantSeq, previous)
			}
			if got := u.e.Stats().FenceBusy; got != tc.wantBusy {
				t.Fatalf("FenceBusy = %d, want %d", got, tc.wantBusy)
			}
		})
	}
}
