package saga

import (
	"context"
	"errors"
	"testing"
	"time"
)

// runStoreContract 是协调器存储（Store + CompletionHistoryStore + LateSuccessAlarmStore）的契约：Apply 的版本与租约
// fence、同一操作的 outbox 替换、回执去重与身份冲突、tombstone 的关闭方式只升不降、关闭操作删除排队命令、领取的到期与租约、
// outbox 的 Ack / Nack 租约、迟到告警按（操作，代际）只记一次、启动身份原样保存。断言只看 Store 的公开行为，不看存储形状。
func runStoreContract(t *testing.T, newStore func(t *testing.T) Store) {
	t.Helper()
	base := time.Now().UTC().Truncate(time.Millisecond)
	pending := func(id string) Record {
		return Record{ID: id, Type: "rally", DefinitionVersion: 1, BusinessKey: "bk-" + id, StartDigest: "digest-" + id,
			Status: StatusPending, Phase: PhaseForward, Version: 1, Data: []byte("start"), NextRunAt: base, CreatedAt: base, UpdatedAt: base}
	}
	waiting := func(r Record, attempt uint32) Record {
		r = r.Clone()
		r.Version++
		r.Status = StatusWaiting
		r.Attempt = attempt
		r.OperationKey = operationKey(r.ID, r.Phase, r.Step)
		r.CommandID = commandID(r.OperationKey, r.Incarnation, attempt)
		r.NextRunAt = base.Add(time.Minute)
		r.UpdatedAt = base.Add(time.Duration(r.Version) * time.Millisecond)
		r.Lease = Lease{}
		return r
	}
	outboxFor := func(r Record) *OutboxRecord {
		command := Command{ID: r.CommandID, IdempotencyKey: r.OperationKey, SagaID: r.ID, SagaType: r.Type, DefinitionVersion: 1, BusinessKey: r.BusinessKey,
			Step: r.Step, StepName: "reserve", Phase: r.Phase, Attempt: r.Attempt, Topic: "reserve", Payload: []byte("p"), DeadlineAt: r.NextRunAt, CreatedAt: base}
		return &OutboxRecord{Command: command, NextAttemptAt: base, CreatedAt: base}
	}
	success := func(r Record) Completion {
		return Completion{CommandID: r.CommandID, IdempotencyKey: r.OperationKey, SagaID: r.ID, Success: true, Data: []byte("ok"), CompletedAt: base}
	}
	ctx := context.Background()

	t.Run("create, read and identity", func(t *testing.T) {
		store := newStore(t)
		record := pending("c1")
		if err := store.Create(ctx, record); err != nil {
			t.Fatal(err)
		}
		got, err := store.Get(ctx, record.ID)
		if err != nil || got.StartDigest != record.StartDigest || got.Version != 1 || got.Status != StatusPending || !got.NextRunAt.Equal(base) || string(got.Data) != "start" {
			t.Fatalf("Get = %+v, %v", got, err)
		}
		byKey, err := store.GetByBusinessKey(ctx, record.Type, record.BusinessKey)
		if err != nil || byKey.ID != record.ID {
			t.Fatalf("GetByBusinessKey = %+v, %v", byKey, err)
		}
		again := pending("c1-other")
		again.BusinessKey = record.BusinessKey
		if err := store.Create(ctx, again); !errors.Is(err, ErrAlreadyExists) {
			t.Fatalf("Create with a used business key = %v, want ErrAlreadyExists", err)
		}
		sameID := pending("c1")
		sameID.BusinessKey = "another"
		if err := store.Create(ctx, sameID); !errors.Is(err, ErrAlreadyExists) {
			t.Fatalf("Create with a used id = %v, want ErrAlreadyExists", err)
		}
		if _, err := store.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get(missing) = %v, want ErrNotFound", err)
		}
	})

	t.Run("apply fences version and lease", func(t *testing.T) {
		store := newStore(t)
		record := pending("f1")
		if err := store.Create(ctx, record); err != nil {
			t.Fatal(err)
		}
		claimed, err := store.ClaimDue(ctx, ClaimRequest{Owner: "a", Now: base, LeaseDuration: time.Second, Limit: 4})
		if err != nil || len(claimed) != 1 || claimed[0].Lease.Owner != "a" || claimed[0].Lease.Token != 1 {
			t.Fatalf("claim = %+v, %v", claimed, err)
		}
		after := waiting(claimed[0], 1)
		if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: 1, ExpectedLease: Lease{Owner: "a", Token: 99}, After: after}); !errors.Is(err, ErrConflict) {
			t.Fatalf("apply with a stale lease = %v, want ErrConflict", err)
		}
		stale := after.Clone()
		stale.Version = 3
		if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: 2, After: stale}); !errors.Is(err, ErrConflict) {
			t.Fatalf("apply with a stale version = %v, want ErrConflict", err)
		}
		outcome, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: 1, ExpectedLease: claimed[0].Lease, After: after, Outbox: outboxFor(after)})
		if err != nil || outcome != ApplyApplied {
			t.Fatalf("apply = %v, %v", outcome, err)
		}
		got, err := store.Get(ctx, record.ID)
		if err != nil || got.Version != 2 || got.Status != StatusWaiting || got.Lease.Owner != "" || got.StartDigest != record.StartDigest {
			t.Fatalf("after apply: %+v, %v", got, err)
		}
		if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: 1, ExpectedLease: claimed[0].Lease, After: waiting(claimed[0], 1)}); !errors.Is(err, ErrConflict) {
			t.Fatalf("second apply from the old version = %v, want ErrConflict", err)
		}
	})

	t.Run("claim due honours next run, lease and terminal", func(t *testing.T) {
		store := newStore(t)
		later := pending("d-later")
		later.NextRunAt = base.Add(time.Hour)
		done := pending("d-done")
		done.Status, done.NextRunAt = StatusCompleted, time.Time{}
		for _, r := range []Record{pending("d-now"), later, done} {
			if err := store.Create(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		first, err := store.ClaimDue(ctx, ClaimRequest{Owner: "a", Now: base, LeaseDuration: time.Second, Limit: 8})
		if err != nil || len(first) != 1 || first[0].ID != "d-now" {
			t.Fatalf("first claim = %+v, %v; want only d-now", first, err)
		}
		if again, err := store.ClaimDue(ctx, ClaimRequest{Owner: "b", Now: base.Add(500 * time.Millisecond), LeaseDuration: time.Second, Limit: 8}); err != nil || len(again) != 0 {
			t.Fatalf("claim while leased = %+v, %v; want none", again, err)
		}
		takeover, err := store.ClaimDue(ctx, ClaimRequest{Owner: "b", Now: base.Add(2 * time.Second), LeaseDuration: time.Second, Limit: 8})
		if err != nil || len(takeover) != 1 || takeover[0].Lease.Owner != "b" || takeover[0].Lease.Token != 2 {
			t.Fatalf("takeover after the lease = %+v, %v; want owner b token 2", takeover, err)
		}
		if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: 1, ExpectedLease: first[0].Lease, After: waiting(first[0], 1)}); !errors.Is(err, ErrConflict) {
			t.Fatalf("apply by the previous lease holder = %v, want ErrConflict", err)
		}
	})

	t.Run("outbox replaces older attempts of one operation", func(t *testing.T) {
		store := newStore(t)
		record := pending("o1")
		if err := store.Create(ctx, record); err != nil {
			t.Fatal(err)
		}
		first := waiting(record, 1)
		if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: 1, After: first, Outbox: outboxFor(first)}); err != nil {
			t.Fatal(err)
		}
		held, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "p", Now: base, LeaseDuration: time.Minute, Limit: 8})
		if err != nil || len(held) != 1 || held[0].Command.ID != first.CommandID || held[0].Lease.Token != 1 {
			t.Fatalf("claim outbox = %+v, %v", held, err)
		}
		second := waiting(first, 2)
		if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: first.Version, After: second, Outbox: outboxFor(second)}); err != nil {
			t.Fatal(err)
		}
		items, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "audit", Now: base.Add(time.Hour), LeaseDuration: time.Minute, Limit: 8})
		if err != nil || len(items) != 1 || items[0].Command.ID != second.CommandID || items[0].Command.Attempt != 2 || string(items[0].Command.Payload) != "p" {
			t.Fatalf("outbox after the second attempt = %+v, %v; want only %s", items, err, second.CommandID)
		}
	})

	t.Run("outbox claim, nack backoff and ack lease", func(t *testing.T) {
		store := newStore(t)
		record := pending("n1")
		if err := store.Create(ctx, record); err != nil {
			t.Fatal(err)
		}
		after := waiting(record, 1)
		if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: 1, After: after, Outbox: outboxFor(after)}); err != nil {
			t.Fatal(err)
		}
		held, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "p", Now: base, LeaseDuration: time.Second, Limit: 8})
		if err != nil || len(held) != 1 {
			t.Fatalf("claim = %+v, %v", held, err)
		}
		if err := store.NackOutbox(ctx, after.CommandID, Lease{Owner: "p", Token: 7}, base.Add(time.Minute), "x"); !errors.Is(err, ErrConflict) {
			t.Fatalf("nack with a wrong token = %v, want ErrConflict", err)
		}
		if err := store.NackOutbox(ctx, after.CommandID, held[0].Lease, base.Add(time.Minute), "publish failed"); err != nil {
			t.Fatal(err)
		}
		if early, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "q", Now: base.Add(30 * time.Second), LeaseDuration: time.Second, Limit: 8}); err != nil || len(early) != 0 {
			t.Fatalf("claim during the nack backoff = %+v, %v; want none", early, err)
		}
		retry, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "q", Now: base.Add(2 * time.Minute), LeaseDuration: time.Second, Limit: 8})
		if err != nil || len(retry) != 1 || retry[0].Attempt != 1 || retry[0].Lease.Owner != "q" {
			t.Fatalf("claim after the backoff = %+v, %v", retry, err)
		}
		if err := store.AckOutbox(ctx, after.CommandID, held[0].Lease); !errors.Is(err, ErrConflict) {
			t.Fatalf("ack with the earlier lease = %v, want ErrConflict", err)
		}
		if err := store.AckOutbox(ctx, after.CommandID, retry[0].Lease); err != nil {
			t.Fatal(err)
		}
		if left, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "audit", Now: base.Add(time.Hour), LeaseDuration: time.Second, Limit: 8}); err != nil || len(left) != 0 {
			t.Fatalf("outbox after ack = %+v, %v; want empty", left, err)
		}
	})

	t.Run("receipt dedup and identity conflict", func(t *testing.T) {
		store := newStore(t)
		record := pending("r1")
		if err := store.Create(ctx, record); err != nil {
			t.Fatal(err)
		}
		w := waiting(record, 1)
		if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: 1, After: w, Outbox: outboxFor(w)}); err != nil {
			t.Fatal(err)
		}
		receipt := success(w)
		next := w.Clone()
		next.Version++
		next.Status, next.Step, next.CompletedSteps, next.Attempt, next.OperationKey, next.CommandID = StatusPending, 1, 1, 0, "", ""
		next.NextRunAt = base
		if outcome, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: w.Version, After: next, Receipt: &receipt, CloseOperation: w.OperationKey}); err != nil || outcome != ApplyApplied {
			t.Fatalf("apply with receipt = %v, %v", outcome, err)
		}
		// 两次送达算出了逐字节相同的请求（时间戳落在同一微秒）：第二次是重复，不能也报“已应用”。
		if outcome, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: w.Version, After: next, Receipt: &receipt, CloseOperation: w.OperationKey}); err != nil || outcome != ApplyDuplicate {
			t.Fatalf("the identical receipt-bearing request delivered again = %v, %v; want ApplyDuplicate", outcome, err)
		}
		// 回执检查先于版本：同一结果带着旧版本重来（重投后另算出的转移）是重复，不是冲突。
		redelivered := next.Clone()
		redelivered.LastError = "computed again from the redelivery"
		if outcome, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: w.Version, After: redelivered, Receipt: &receipt, CloseOperation: w.OperationKey}); err != nil || outcome != ApplyDuplicate {
			t.Fatalf("replayed receipt = %v, %v; want ApplyDuplicate", outcome, err)
		}
		forged := receipt
		forged.Data = []byte("different")
		if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: next.Version, After: func() Record { r := next.Clone(); r.Version++; return r }(), Receipt: &forged}); !errors.Is(err, ErrIdentityConflict) {
			t.Fatalf("same command id, different result = %v, want ErrIdentityConflict", err)
		}
		history := store.(CompletionHistoryStore)
		got, err := history.CompletionHistory(ctx, receipt)
		if err != nil || !got.Recorded || !got.Receipt {
			t.Fatalf("history of the accepted receipt = %+v, %v", got, err)
		}
		if _, err := history.CompletionHistory(ctx, forged); !errors.Is(err, ErrIdentityConflict) {
			t.Fatalf("history of a forged receipt = %v, want ErrIdentityConflict", err)
		}
		other := receipt
		other.CommandID = commandID(w.OperationKey, 0, 9)
		if got, err := history.CompletionHistory(ctx, other); err != nil || !got.Recorded || got.Receipt || got.Closure != OperationClosedWithResult {
			t.Fatalf("history of another attempt of the closed operation = %+v, %v; want recorded by the tombstone (result)", got, err)
		}
		if recorded, err := store.CompletionRecorded(ctx, receipt); err != nil || !recorded {
			t.Fatalf("CompletionRecorded = %v, %v", recorded, err)
		}
		fresh := Completion{CommandID: "r1:1:1:1", IdempotencyKey: operationKey("r1", PhaseForward, 1), SagaID: "r1", Success: true}
		if got, err := history.CompletionHistory(ctx, fresh); err != nil || got.Recorded {
			t.Fatalf("history of a never-closed operation = %+v, %v; want nothing recorded", got, err)
		}
	})

	t.Run("tombstone closure only upgrades and close deletes queued commands", func(t *testing.T) {
		store := newStore(t)
		record := pending("t1")
		if err := store.Create(ctx, record); err != nil {
			t.Fatal(err)
		}
		w := waiting(record, 1)
		if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: 1, After: w, Outbox: outboxFor(w)}); err != nil {
			t.Fatal(err)
		}
		held, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "p", Now: base, LeaseDuration: time.Minute, Limit: 8})
		if err != nil || len(held) != 1 {
			t.Fatalf("claim = %+v, %v", held, err)
		}
		// 超时用尽：放弃关闭，排队命令随之删除。
		failed := w.Clone()
		failed.Version++
		failed.Status, failed.OperationKey, failed.CommandID, failed.NextRunAt = StatusFailed, "", "", time.Time{}
		if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: w.Version, After: failed, CloseOperation: w.OperationKey}); err != nil {
			t.Fatal(err)
		}
		history := store.(CompletionHistoryStore)
		late := success(w)
		if got, err := history.CompletionHistory(ctx, late); err != nil || !got.Recorded || got.Receipt || got.Closure != OperationAbandoned {
			t.Fatalf("history after an abandon = %+v, %v; want abandoned", got, err)
		}
		if left, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "audit", Now: base.Add(time.Hour), LeaseDuration: time.Second, Limit: 8}); err != nil || len(left) != 0 {
			t.Fatalf("outbox after closing the operation = %+v, %v; want empty", left, err)
		}
		marker := store.(LateSuccessAlarmStore)
		if first, err := marker.MarkLateSuccessAlarm(ctx, late, 0); err != nil || !first {
			t.Fatalf("first alarm = %v, %v; want true", first, err)
		}
		if again, err := marker.MarkLateSuccessAlarm(ctx, late, 0); err != nil || again {
			t.Fatalf("second alarm of the same incarnation = %v, %v; want false", again, err)
		}
		if next, err := marker.MarkLateSuccessAlarm(ctx, late, 1); err != nil || !next {
			t.Fatalf("alarm of another incarnation = %v, %v; want true", next, err)
		}
		noTombstone := Completion{CommandID: "t1:1:1:1", IdempotencyKey: operationKey("t1", PhaseForward, 1), SagaID: "t1", Success: true}
		if marked, err := marker.MarkLateSuccessAlarm(ctx, noTombstone, 0); err != nil || marked {
			t.Fatalf("alarm without a tombstone = %v, %v; want false", marked, err)
		}
		// 迟到的成功被接收（方向 ④）：升级为带结果关闭；之后再放弃关闭同一操作不降级。
		reopened := failed.Clone()
		reopened.Version++
		reopened.Status, reopened.Phase, reopened.LateStep, reopened.LateData = StatusManualRequired, PhaseForward, 1, []byte("ok")
		if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: failed.Version, After: reopened, Receipt: &late, CloseOperation: w.OperationKey}); err != nil {
			t.Fatal(err)
		}
		other := late
		other.CommandID = commandID(w.OperationKey, 0, 5)
		if got, err := history.CompletionHistory(ctx, other); err != nil || got.Closure != OperationClosedWithResult {
			t.Fatalf("closure after the late success = %+v, %v; want result", got, err)
		}
		again := reopened.Clone()
		again.Version++
		if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: reopened.Version, After: again, CloseOperation: w.OperationKey}); err != nil {
			t.Fatal(err)
		}
		if got, err := history.CompletionHistory(ctx, other); err != nil || got.Closure != OperationClosedWithResult {
			t.Fatalf("closure after a later abandon of the same operation = %+v, %v; want still result", got, err)
		}
	})

	t.Run("ack after the operation closed is not a conflict", func(t *testing.T) {
		store := newStore(t)
		record := pending("a1")
		if err := store.Create(ctx, record); err != nil {
			t.Fatal(err)
		}
		w := waiting(record, 1)
		if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: 1, After: w, Outbox: outboxFor(w)}); err != nil {
			t.Fatal(err)
		}
		held, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "p", Now: base, LeaseDuration: time.Minute, Limit: 8})
		if err != nil || len(held) != 1 {
			t.Fatalf("claim = %+v, %v", held, err)
		}
		receipt := success(w)
		done := w.Clone()
		done.Version++
		done.Status, done.CompletedSteps, done.OperationKey, done.CommandID, done.NextRunAt = StatusCompleted, 1, "", "", time.Time{}
		if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: w.Version, After: done, Receipt: &receipt, CloseOperation: w.OperationKey}); err != nil {
			t.Fatal(err)
		}
		if err := store.AckOutbox(ctx, held[0].Command.ID, held[0].Lease); err != nil {
			t.Fatalf("ack of a command whose operation closed = %v, want nil", err)
		}
		if err := store.NackOutbox(ctx, held[0].Command.ID, held[0].Lease, base.Add(time.Minute), "late"); err != nil {
			t.Fatalf("nack of a command whose operation closed = %v, want nil", err)
		}
	})
}
