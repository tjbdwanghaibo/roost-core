package saga

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/metrics"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
)

// RR-20260927-16（OPEN-ITEMS C25）：Reserve 在事务内读到权威回执时顺手把 claim 标成 completed（markCompleted），
// 旧实现 `_ = inbox.markCompleted(...)` 把写错误整个吞掉。维护者决定不改返回值（回执才是权威，claim 只是协调行；
// 真实服务端会因写错误中止事务并由驱动重跑），但失败不能静默：每次失败记一条 Warn（带 command_id 与原因），
// 并累加无标签计数 saga.step_inbox.mark_completed_error_total。将来出现确定性失败（重跑到超时）时，日志与计数是唯一线索。
func TestReserveReportsSwallowedMarkCompletedFailure(t *testing.T) {
	client := newDataEngineInboxMongo()
	injected := errors.New("injected claim update failure")
	flaky := &claimUpdateFailOnce{Client: client, err: injected}
	inbox, err := NewDataEngineStepInbox(flaky, "game", DataEngineStepInboxOptions{Owner: "worker-1", LeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	command := dataEngineCommand("command-mark", "operation-mark", "a")
	if _, err := inbox.Reserve(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	completion := Completion{CommandID: command.ID, IdempotencyKey: command.IdempotencyKey, SagaID: command.SagaID, Success: true, Data: []byte("done"), CompletedAt: time.Now().UTC()}
	effect, err := NewCompletionEffect(completion)
	if err != nil {
		t.Fatal(err)
	}
	if err := inboxReceipts(client).Seed(dataEngineReceipt{
		ID: dataEngineStepNamespace + "/" + command.ID, Digest: mustCommandDigest(t, command), Payload: effect.Payload,
	}); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	before := markCompletedErrors()
	flaky.armed.Store(true)

	reservation, err := inbox.Reserve(context.Background(), command)
	// 返回值不变：第一次尝试的写错误让事务中止，驱动重跑后 claim 标记成功，Reserve 照常返回 Duplicate + 回执里的完成结果。
	if err != nil || !reservation.Duplicate || reservation.Completion.CommandID != command.ID || string(reservation.Completion.Data) != "done" {
		t.Fatalf("reservation=%+v err=%v, want the duplicate completion from the receipt", reservation, err)
	}
	if flaky.failed.Load() != 1 {
		t.Fatalf("premise: injected claim update failures=%d, want 1", flaky.failed.Load())
	}
	if got := markCompletedErrors() - before; got != 1 {
		t.Fatalf("saga.step_inbox.mark_completed_error_total grew by %d, want 1: the swallowed failure is silent", got)
	}
	text := logs.String()
	if !strings.Contains(text, "level=WARN") || !strings.Contains(text, "command_id="+command.ID) || !strings.Contains(text, injected.Error()) {
		t.Fatalf("no warning with the command id and cause for the swallowed markCompleted failure; logs=%q", text)
	}
	var claim dataEngineClaim
	if err := inboxClaims(client).FindOne(context.Background(), map[string]any{"_id": dataEngineStepNamespace + "/" + command.ID}, &claim); err != nil || claim.Status != claimStatusCompleted {
		t.Fatalf("claim=%+v err=%v, want completed after the retried transaction", claim, err)
	}
}

func markCompletedErrors() int64 {
	for _, metric := range metrics.Snapshot() {
		if metric.Name == "saga.step_inbox.mark_completed_error_total" {
			return metric.Value
		}
	}
	return 0
}

// claimUpdateFailOnce 在 armed 之后让 claim 集合的第一次 UpdateOne 返回 err（经伪 Mongo 的事务语义：写错误中止事务）。
type claimUpdateFailOnce struct {
	*mongotest.Client
	err    error
	armed  atomic.Bool
	failed atomic.Int32
}

func (m *claimUpdateFailOnce) Database(name string) fmongo.IDatabase {
	return claimFailDatabase{IDatabase: m.Client.Database(name), mongo: m}
}

type claimFailDatabase struct {
	fmongo.IDatabase
	mongo *claimUpdateFailOnce
}

func (d claimFailDatabase) Collection(name string) fmongo.ICollection {
	coll := d.IDatabase.Collection(name)
	if name != dataEngineClaimCollection {
		return coll
	}
	return claimFailCollection{ICollection: coll, mongo: d.mongo}
}

type claimFailCollection struct {
	fmongo.ICollection
	mongo *claimUpdateFailOnce
}

func (c claimFailCollection) UpdateOne(ctx context.Context, filter any, update any) (*fmongo.UpdateResult, error) {
	if c.mongo.armed.CompareAndSwap(true, false) {
		c.mongo.failed.Add(1)
		// 与服务端一致：事务里的写错误中止事务。借真实集合一次注定失败的写入登记中止原因。
		raw := c.mongo.Client.Collection("game", dataEngineClaimCollection)
		raw.Errors["UpdateOne"] = c.mongo.err
		defer delete(raw.Errors, "UpdateOne")
	}
	return c.ICollection.UpdateOne(ctx, filter, update)
}
