package nestwal_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	engine "github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

// RR-20260926-50：真实 WAL 进入 terminal（段文件 fsync 返回 EIO，RR-33 起粘滞为 ErrCommitIndeterminate）后，
// 本进程不会再确认任何记录。已在 DataEngine Projector.WaitEntityProjection 里等待的冷加载必须立即拿到
// 可判别的 WAL 错误，而不是等到自己的截止时间或 Projector 关闭。放在 nestwal 目录的外部测试包里，
// 是为了用 nestwal 的 fsync 测试缝在上层组件上制造真实 terminal（engine 包内拿不到这个缝）。

type acceptingStore struct{}

func (acceptingStore) Project(context.Context, coredata.CommitRecord) error { return nil }

type enteredContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

// Done 在等待方的 select 求值 ctx.Done() 时通知测试：此后发生的事件一定是在“等待中”被观察到的。
func (c *enteredContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func projectionRecord(sequence byte, entityID int64, durability coredata.Durability) coredata.CommitRecord {
	var id coredata.TransactionID
	id[15] = sequence
	data := []byte{0x05, 0, 0, 0, 0} // 空 BSON 文档；ManualReplay 下不投影，Projector 不解析它
	return coredata.CommitRecord{
		ID: id, Handler: "test.handler", Durability: durability,
		Mutations: []coredata.Mutation{{
			Key:  coredata.DocumentKey{Database: "game", Resource: "players", ID: entityID},
			Kind: coredata.MutationPut, NextVersion: 1, Mask: 1, Schema: 1, Codec: "bson-v2", Data: data,
		}},
	}
}

func TestWALTerminalWakesEntityProjectionWaiters(t *testing.T) {
	failing := make(chan struct{})
	opts := nestwal.DefaultOptions(t.TempDir())
	opts.WriterVersion = nestwal.WriterVersionV2
	opts.GroupCommitInterval = time.Hour
	nestwal.SetSyncFileForTest(&opts, func(file *os.File) error {
		select {
		case <-failing:
			return &os.PathError{Op: "sync", Path: file.Name(), Err: syscall.EIO}
		default:
			return file.Sync()
		}
	})
	wal, err := nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wal.Close(context.Background()) })
	projector, err := engine.NewProjector(wal, acceptingStore{}, engine.ProjectorOptions{CloseWAL: false, ManualReplay: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projector.Close(context.Background()) })

	// 已准入、仍在实体锁内（未 TransactionReleased）：测试期间不会投影，实体 41 的冷加载须等待。
	if err := projector.Commit(context.Background(), projectionRecord(1, 41, 2)); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	entered := &enteredContext{Context: waitCtx, entered: make(chan struct{})}
	waiter := make(chan error, 1)
	go func() { waiter <- projector.WaitEntityProjection(entered, 41) }()
	select {
	case <-entered.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not start waiting")
	}

	close(failing)
	if err := projector.Commit(context.Background(), projectionRecord(2, 42, 2)); !errors.Is(err, corenest.ErrCommitIndeterminate) {
		t.Fatalf("premise: strict commit on a failing fsync err=%v, want ErrCommitIndeterminate", err)
	}
	select {
	case <-wal.Terminated():
	default:
		t.Fatal("premise: WAL is not terminal after the injected fsync failure")
	}
	select {
	case err := <-waiter:
		if !errors.Is(err, corenest.ErrCommitIndeterminate) {
			t.Fatalf("waiter woken by WAL terminal err=%v, want errors.Is ErrCommitIndeterminate", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WAL went terminal but a cold load already waiting in WaitEntityProjection is still blocked; it gets no WAL error until its own deadline or Close")
	}
}
