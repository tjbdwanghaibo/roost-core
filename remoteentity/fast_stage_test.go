package remoteentity

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/fctx"
)

func TestFastWorkerRejectsRemoteWaitingBeforeMutation(t *testing.T) {
	// 故意不初始化 Manager：拦截必须早于解引用、准入和任何远端 I/O。
	var manager *Manager
	batch := &remoteWriteBatch{}
	cases := map[string]func(){
		"PrepareRemoteWriteBatch":      func() { _, _ = manager.PrepareRemoteWriteBatch(context.Background(), []int64{1}) },
		"Commit":                       func() { _, _ = batch.Commit(context.Background()) },
		"Close":                        func() { _ = batch.Close(context.Background()) },
		"waitRemoteTransaction":        func() { _, _ = manager.waitRemoteTransaction(context.Background(), entity.RemoteTransactionID{}) },
		"waitTrackedRemoteTransaction": func() { _, _ = manager.waitTrackedRemoteTransaction(context.Background(), nil) },
		"FlushRemoteAll":               func() { _ = manager.FlushRemoteAll(context.Background()) },
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			_, release := fctx.NewContext(fctx.WithFastWorker(), fctx.WithHandler("misuse"))
			defer release()
			defer func() {
				err, ok := recover().(error)
				if !ok || !errors.Is(err, fctx.ErrBlockingInFastWorker) || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "misuse") {
					t.Fatalf("panic=%v", err)
				}
				if batch.closed || batch.committed || batch.reserved {
					t.Fatal("batch mutated before rejection")
				}
			}()
			call()
		})
	}
}
