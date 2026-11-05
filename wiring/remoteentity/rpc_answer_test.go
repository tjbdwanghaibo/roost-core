// 实体服务写 RPC 的答复：不是 owner 时按权威的所有权记录改投，确定没执行的给可重试，结果未知的原样保留。

package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/worker"
	"github.com/tjbdwanghaibo/roost-core/infra/network/servicerpc"
)

type ownershipLookup struct {
	entity.RemoteOwnershipManager
	lease entity.RemoteEntityMarkerLease
	found bool
	err   error
	reads int
}

func (o *ownershipLookup) GetRemoteOwnership(context.Context, int64) (entity.RemoteEntityMarkerLease, bool, error) {
	o.reads++
	return o.lease, o.found, o.err
}

func TestRPCWriteAnswerRedirectsToTheDurableOwner(t *testing.T) {
	const local int32 = 3000
	ctx := context.Background()
	fenced := fmt.Errorf("nest: write: %w: entity=1 owner=3001", entity.ErrRemoteFenced)

	moved := &ownershipLookup{lease: entity.RemoteEntityMarkerLease{OwnerSid: 3001, MarkerEpoch: 2}, found: true}
	answer := RPCWriteAnswer(ctx, moved, local, 1, fenced)
	if owner, ok := servicerpc.RedirectSID(answer); !ok || owner != 3001 || servicerpc.Code(answer) != servicerpc.CodeRedirect || moved.reads != 1 {
		t.Fatalf("fenced write: answer=%v code=%d owner=%d,%v reads=%d, want redirect to 3001", answer, servicerpc.Code(answer), owner, ok, moved.reads)
	}

	for name, lookup := range map[string]*ownershipLookup{
		"owner still local": {lease: entity.RemoteEntityMarkerLease{OwnerSid: local}, found: true},
		"no record":         {},
		"lookup failed":     {err: errors.New("mongo down")},
	} {
		if answer := RPCWriteAnswer(ctx, lookup, local, 1, fenced); servicerpc.Classify(answer) != servicerpc.OutcomeRetryable {
			t.Errorf("%s: answer=%v outcome=%s, want retryable", name, answer, servicerpc.Classify(answer))
		}
	}

	for _, tc := range []struct {
		name string
		err  error
		want servicerpc.Outcome
	}{
		{"success", nil, servicerpc.OutcomeOK},
		{"reloading", entity.ErrRemoteEntityReloading, servicerpc.OutcomeRetryable},
		{"transfer in progress", entity.ErrRemoteOwnerTransition, servicerpc.OutcomeRetryable},
		{"write slots full", entity.ErrRemoteOverloaded, servicerpc.OutcomeRetryable},
		{"admission version conflict", entity.ErrRemoteVersionConflict, servicerpc.OutcomeRetryable},
		// Nest 派发队列满（例如 io 池的 worker 都在等 strict 确认）：准入时就拒绝，handler 没有开始，确定没执行。
		{"nest dispatch queue full", fmt.Errorf("sync sender: %w", errors.Join(corenest.ErrQueueFull, worker.ErrWorkerQueueFull)), servicerpc.OutcomeRetryable},
		{"indeterminate wrapping a fence", errors.Join(entity.ErrRemotePersistenceIndeterminate, entity.ErrRemoteFenced), servicerpc.OutcomeUnknown},
		{"budget expired", context.DeadlineExceeded, servicerpc.OutcomeUnknown},
		{"business rejection", servicerpc.Check(100001, "alliance full", ""), servicerpc.OutcomeRejected},
	} {
		lookup := &ownershipLookup{}
		answer := RPCWriteAnswer(ctx, lookup, local, 1, tc.err)
		if got := servicerpc.Classify(answer); got != tc.want || lookup.reads != 0 {
			t.Errorf("%s: answer=%v outcome=%s reads=%d, want %s without an ownership read", tc.name, answer, got, lookup.reads, tc.want)
		}
	}
}
