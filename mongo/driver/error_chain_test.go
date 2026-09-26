package driver

import (
	"errors"
	"testing"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	drivermongo "go.mongodb.org/mongo-driver/v2/mongo"
)

// RR-20260926-34：撞键转换成 fmongo.ErrDuplicateKey 时必须保留驱动错误链，调用方既能
// errors.Is 判撞键，也能 errors.As 取回 WriteException、错误码与服务端标签；非撞键错误
// （例如带 TransientTransactionError 的 WriteConflict）原样返回。
func TestWrapErrorKeepsTheDriverChainBehindTheSentinel(t *testing.T) {
	duplicate := drivermongo.WriteException{
		WriteErrors: drivermongo.WriteErrors{{Code: 11000, Message: "E11000 duplicate key error"}},
		Labels:      []string{"SomeServerLabel"},
	}
	err := wrapError(duplicate)
	if !errors.Is(err, fmongo.ErrDuplicateKey) {
		t.Fatalf("err=%v, want errors.Is ErrDuplicateKey", err)
	}
	var writeErr drivermongo.WriteException
	if !errors.As(err, &writeErr) || !drivermongo.IsDuplicateKeyError(err) {
		t.Fatalf("err=%v lost the WriteException", err)
	}
	var labeled drivermongo.LabeledError
	if !errors.As(err, &labeled) || !labeled.HasErrorLabel("SomeServerLabel") {
		t.Fatalf("err=%v lost the server labels", err)
	}

	conflict := drivermongo.CommandError{Code: 112, Name: "WriteConflict", Labels: []string{"TransientTransactionError"}}
	if got := wrapError(conflict); errors.Is(got, fmongo.ErrDuplicateKey) || !errors.As(got, &labeled) || !labeled.HasErrorLabel("TransientTransactionError") {
		t.Fatalf("write conflict=%v, want it unchanged and still transient", got)
	}
	if got := wrapError(drivermongo.ErrNoDocuments); got != fmongo.ErrNotFound {
		t.Fatalf("no documents=%v, want the bare ErrNotFound", got)
	}
}
