package engine

import (
	"context"
	"errors"
	"testing"

	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// lostReplyClient 让每个事务照常提交，但丢掉提交应答：调用方拿到 ErrCommitResultUnknown（驱动的“提交已发出、
// 结果未确认”），目标库里却已经有了这次写的文档（RV-20261114-01）。
type lostReplyClient struct{ fmongo.IMongo }

func (c lostReplyClient) StartSession(ctx context.Context) (fmongo.ISession, error) {
	session, err := c.IMongo.StartSession(ctx)
	if err != nil {
		return nil, err
	}
	return lostReplySession{session}, nil
}

type lostReplySession struct{ fmongo.ISession }

func (s lostReplySession) WithTransaction(ctx context.Context, fn func(context.Context) error) error {
	if err := s.ISession.WithTransaction(ctx, fn); err != nil {
		return err
	}
	return errors.Join(fmongo.ErrCommitResultUnknown, context.DeadlineExceeded)
}

// 复制的提交应答丢了：错误照实返回，结果里列出可能已经写进目标库的文档（Outcome unknown），
// 拿它 DropEntityCopy 能删掉目标库里的副本；重做复制得到已在（already_present），即确定的清单。
// 冲突与读源库失败是确定没写的，不列文档。
func TestCopyHandedOffEntityLostCommitReplyListsWhatMayHaveBeenWritten(t *testing.T) {
	ctx := context.Background()
	client := mongotest.NewClient()
	source := newCopyServer(t, client, 3)
	id := seedPreload(t, client, 3, preloadFirstKind, "preload_first", 987, 1, bson.M{"level": int32(7)})[0]
	target := client.Collection(testDatabase+"_4", "preload_first")
	source.runtime.Projector.holdEntityHandOff(id)

	source.store.client = lostReplyClient{client}
	maybe, err := source.runtime.CopyHandedOffEntity(ctx, id, 4, true)
	if !errors.Is(err, fmongo.ErrCommitResultUnknown) {
		t.Fatalf("copy with a lost commit reply = %v, want ErrCommitResultUnknown", err)
	}
	if _, found := target.Lookup(id); !found {
		t.Fatal("the injection did not commit the target document")
	}
	if len(maybe.Documents) != 1 || maybe.Documents[0].Outcome != EntityCopyUnknown || maybe.Documents[0].Version != 3 ||
		maybe.EntityID != id || maybe.To != 4 {
		t.Fatalf("copy with a lost commit reply returned %+v; want the document that may have been written", maybe)
	}

	source.store.client = client
	again, err := source.runtime.CopyHandedOffEntity(ctx, id, 4, true)
	if err != nil || len(again.Documents) != 1 || again.Documents[0].Outcome != EntityCopyAlreadyPresent {
		t.Fatalf("redoing the copy = %+v, %v; want already_present", again, err)
	}
	if dropped, err := source.runtime.DropEntityCopy(ctx, maybe, 4); err != nil || dropped != 1 {
		t.Fatalf("dropping what may have been written = %d, %v", dropped, err)
	}
	if _, found := target.Lookup(id); found {
		t.Fatal("the target copy is still there")
	}

	// 确定没写的错误不列文档：目标里是别的版本、不覆盖时冲突。
	if _, err := target.InsertOne(ctx, bson.M{"_id": id, "_version": uint64(1), "_schema": uint32(1), "_last_tx": "old"}); err != nil {
		t.Fatal(err)
	}
	conflict, err := source.runtime.CopyHandedOffEntity(ctx, id, 4, false)
	if !errors.Is(err, ErrEntityCopyConflict) || len(conflict.Documents) != 0 {
		t.Fatalf("conflict = %+v, %v; want ErrEntityCopyConflict and no documents", conflict, err)
	}
}
