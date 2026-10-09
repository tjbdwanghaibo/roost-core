//go:build integration

package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// RR-20260926-34：记录 mongotest 伪 Mongo 必须对齐的真实服务端行为，并证明错误转换
// 保留原错误链。事务内撞唯一键会中止事务；之后同一事务里的读返回 NoSuchTransaction，
// 带 TransientTransactionError 标签（驱动据此重跑整个回调）。撞键本身不带该标签。
func TestRealMongoWriteErrorAbortsTransactionAndKeepsDriverChain(t *testing.T) {
	client := connect(t, replicaSetURI(t), true, IndexMigrationPolicy{})
	database := fmt.Sprintf("roost_it_txabort_%d_%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = client.Database(database).Drop(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	coll := client.Database(database).Collection("c")
	if _, err := coll.InsertOne(ctx, bson.M{"_id": "x", "v": 1}); err != nil {
		t.Fatal(err)
	}
	session, err := client.StartSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer session.EndSession(ctx)
	stop := errors.New("stop after the first attempt")
	calls := 0
	var insertErr, findErr error
	err = session.WithTransaction(ctx, func(txCtx context.Context) error {
		calls++
		if calls > 1 {
			return stop
		}
		_, insertErr = coll.InsertOne(txCtx, bson.M{"_id": "x", "v": 2})
		var found bson.M
		findErr = coll.FindOne(txCtx, bson.M{"_id": "x"}, &found)
		return findErr
	})
	if !errors.Is(insertErr, fmongo.ErrDuplicateKey) {
		t.Fatalf("insert=%v, want errors.Is ErrDuplicateKey", insertErr)
	}
	var writeErr mongo.WriteException
	if !errors.As(insertErr, &writeErr) || !mongo.IsDuplicateKeyError(insertErr) {
		t.Fatalf("insert=%v lost the driver WriteException chain", insertErr)
	}
	if hasLabel(insertErr, "TransientTransactionError") {
		t.Fatalf("duplicate key unexpectedly transient: %v", insertErr)
	}
	if findErr == nil || !hasLabel(findErr, "TransientTransactionError") {
		t.Fatalf("find after the write error=%v, want an aborted-transaction error labelled TransientTransactionError", findErr)
	}
	var serverErr mongo.ServerError
	if !errors.As(findErr, &serverErr) || !serverErr.HasErrorCode(251) {
		t.Fatalf("find after the write error=%v, want NoSuchTransaction (251)", findErr)
	}
	if calls != 2 || !errors.Is(err, stop) {
		t.Fatalf("calls=%d err=%v, want the driver to retry the transient callback once", calls, err)
	}

	// The driver decides with errors.As on the first LabeledError in the chain.
	// Now that the duplicate key keeps its WriteException, joining it in front
	// of the transient error hides the label: no retry. mongotest mirrors this.
	calls = 0
	err = session.WithTransaction(ctx, func(txCtx context.Context) error {
		calls++
		if calls > 1 {
			return stop
		}
		_, insertErr := coll.InsertOne(txCtx, bson.M{"_id": "x", "v": 3})
		var found bson.M
		return errors.Join(insertErr, coll.FindOne(txCtx, bson.M{"_id": "x"}, &found))
	})
	if calls != 1 || !errors.Is(err, fmongo.ErrDuplicateKey) || errors.Is(err, stop) {
		t.Fatalf("calls=%d err=%v, want the joined duplicate key returned without a retry", calls, err)
	}
}

func hasLabel(err error, label string) bool {
	var labeled mongo.LabeledError
	return errors.As(err, &labeled) && labeled.HasErrorLabel(label)
}
