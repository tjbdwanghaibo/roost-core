package nest

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// RV-20261101-05：失败创建原位写回执也是外部持久副作用；吞错不能使其提交。
func TestFailedBuildReceiptPayload(t *testing.T) {
	for _, tc := range []struct {
		name                                        string
		same, propagate, nested, success, duplicate bool
	}{
		{name: "swallowed"},
		{name: "same_value", same: true},
		{name: "propagated", propagate: true},
		{name: "nested", nested: true},
		{name: "successful", success: true},
		{name: "duplicate_add_is_noop", duplicate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := entity.NewEntityManager()
			pilot := addPilots(t, manager, 39500, 1)[0]
			id := mustBuildCastID(t, 39501, 1, failedCreateKind)
			nestedID := mustBuildCastID(t, 39502, 1, failedCreateKind)
			payload := "failed-build"
			if tc.same {
				payload = "before"
			}
			write := func() error {
				if tc.duplicate {
					return AddReceipt(dataengine.Receipt{Namespace: "review", ID: "command", Digest: []byte{1}, Payload: []byte(payload)})
				}
				return SetReceiptPayload("review", "command", []byte(payload))
			}
			effect := write
			if tc.nested {
				planFailedCreate(t, nestedID, failedCreatePlan{stage: failedCreateInBuilder, sideEffect: write})
				effect = func() error {
					_, err := manager.Create(failedCreateParam(nestedID))
					if !errors.Is(err, errFailedCreateRejected) {
						return errors.New("nested fixture did not reject")
					}
					return nil // 内层错误被处理，外层仍不得丢失副作用标记。
				}
			}
			stage := failedCreateInBuilder
			if tc.success {
				stage = failedCreateNever
			}
			planFailedCreate(t, id, failedCreatePlan{stage: stage, sideEffect: effect})
			committer := &recordingCommitter{}
			mgr := NewEngine(NestOptionWithGetter(entity.NewManagerAccess(manager)), NestOptionWithTransactionCommitter(committer))
			name := NewHandlerName("failed_create_receipt")
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				if err := AddReceipt(dataengine.Receipt{Namespace: "review", ID: "command", Digest: []byte{1}, Payload: []byte("before")}); err != nil {
					return nil, err
				}
				_, err := manager.Create(failedCreateParam(id))
				if tc.success {
					return nil, err
				}
				if !errors.Is(err, errFailedCreateRejected) {
					return nil, errors.New("fixture did not reject")
				}
				if tc.propagate {
					return nil, err
				}
				return "handled", nil
			}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer mgr.Shutdown(context.Background())
			_, err := mgr.Request(context.Background(), name, pilot, nil)
			if tc.success || tc.duplicate {
				if err != nil {
					t.Fatal(err)
				}
				want := payload
				if tc.duplicate {
					want = "before"
				}
				if len(committer.record.Receipts) != 1 || string(committer.record.Receipts[0].Payload) != want {
					t.Fatalf("receipts=%+v", committer.record.Receipts)
				}
				if (manager.Get(id) != nil) != tc.success {
					t.Fatal("wrong publication state")
				}
				return
			}
			if !errors.Is(err, ErrFailedCreateLeftEffects) {
				t.Fatalf("external receipt escaped guard: err=%v receipts=%+v", err, committer.record.Receipts)
			}
			if tc.propagate && !errors.Is(err, errFailedCreateRejected) {
				t.Fatalf("lost original error: %v", err)
			}
			if len(committer.record.Receipts) != 0 || len(committer.record.Mutations) != 0 || manager.Get(id) != nil || manager.Get(nestedID) != nil {
				t.Fatal("failed creation published or persisted effects")
			}
		})
	}
}
