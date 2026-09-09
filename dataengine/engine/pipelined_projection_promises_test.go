package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

// U-0146 · C8（快慢路径不对称）· classscan C8：Projector 的 Commit（严格）与
// Enqueue（流水线）。
//
// 两条提交路径对同一条记录必须给出同样的结果：拒绝时都不留下准入、不计提交；
// 接受时都要在记录落盘后尽快投影。此前只有 Commit 在（同步）落盘后 signal 唤醒
// 投影循环，Enqueue 拿到票就返回、票完成时无人唤醒——事务释放的 kick 大多早于
// fsync，于是一批流水线提交的尾巴要等到 IdlePoll（默认 1 秒）才落 Mongo。

func newPipelinedProjector(t *testing.T, mutate func(*nestwal.Options)) (*Projector, *projectorOutboxFake) {
	t.Helper()
	opts := nestwal.DefaultOptions(t.TempDir())
	opts.WriterVersion = nestwal.WriterVersionV2
	if mutate != nil {
		mutate(&opts)
	}
	wal, err := nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	store := newProjectorOutboxFake()
	// IdlePoll 拉到一小时：投影只能靠唤醒发生，不能靠轮询兜底。
	projector, err := NewProjector(wal, store, ProjectorOptions{CloseWAL: true, IdlePoll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projector.Close(context.Background()) })
	return projector, store
}

func pipelinedRecord(sequence byte) corenest.CommitRecord {
	record := projectorRecord(sequence, false)
	record.Durability = corenest.DurabilityPipelined
	return record
}

func TestPipelinedCommitIsProjectedOnceDurableWithoutWaitingForIdlePoll(t *testing.T) {
	// 批延迟拉到 200ms：事务释放时的那次唤醒必然早于落盘，和生产里一样。
	projector, store := newPipelinedProjector(t, func(opts *nestwal.Options) { opts.BatchDelay = 200 * time.Millisecond })
	// 让启动时的那次唤醒先被消费掉，投影循环进入 IdlePoll 等待。
	time.Sleep(50 * time.Millisecond)
	record := pipelinedRecord(11)
	ticket, err := projector.Enqueue(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	// nest 在 Enqueue 返回后立刻释放实体锁并通知投影器。
	projector.TransactionReleased(record.ID)
	select {
	case <-ticket.Done():
		if err := ticket.Err(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("pipelined commit never became durable")
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		store.mu.Lock()
		_, projected := store.records[record.ID]
		store.mu.Unlock()
		if projected {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a durable pipelined commit was not projected within 500ms (stats %+v); it would wait for IdlePoll", projector.Stats())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCommitAndEnqueueRejectAnOversizedRecordAlikeAndLeaveNoAdmission(t *testing.T) {
	projector, store := newPipelinedProjector(t, func(opts *nestwal.Options) { opts.MaxRecordBytes = 512 })
	ctx := context.Background()
	strict := projectorRecord(12, false)
	strict.Mutations[0].Data = make([]byte, 4096)
	pipelined := pipelinedRecord(13)
	pipelined.Mutations[0].Data = make([]byte, 4096)

	if err := projector.Commit(ctx, strict); !errors.Is(err, nestwal.ErrRecordTooLarge) {
		t.Fatalf("Commit of an oversized record = %v, want ErrRecordTooLarge", err)
	}
	if _, err := projector.Enqueue(ctx, pipelined); !errors.Is(err, nestwal.ErrRecordTooLarge) {
		t.Fatalf("Enqueue of an oversized record = %v, want ErrRecordTooLarge", err)
	}
	for _, id := range []corenest.TransactionID{strict.ID, pipelined.ID} {
		if projector.isHeld(id) {
			t.Fatalf("a rejected record is still held: %s", id.String())
		}
	}
	if stats := projector.Stats(); stats.Committed != 0 || stats.WALUnacked != 0 {
		t.Fatalf("rejected records were counted: %+v", stats)
	}
	// 对照：两条路径接受的记录都被投影。
	if err := projector.Commit(ctx, projectorRecord(14, false)); err != nil {
		t.Fatal(err)
	}
	projector.TransactionReleased(projectorRecord(14, false).ID)
	ticket, err := projector.Enqueue(ctx, pipelinedRecord(15))
	if err != nil {
		t.Fatal(err)
	}
	projector.TransactionReleased(pipelinedRecord(15).ID)
	<-ticket.Done()
	deadline := time.Now().Add(time.Second)
	for {
		store.mu.Lock()
		_, strictProjected := store.records[projectorRecord(14, false).ID]
		_, pipelinedProjected := store.records[pipelinedRecord(15).ID]
		store.mu.Unlock()
		if strictProjected && pipelinedProjected {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("accepted records not projected: strict=%v pipelined=%v", strictProjected, pipelinedProjected)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
