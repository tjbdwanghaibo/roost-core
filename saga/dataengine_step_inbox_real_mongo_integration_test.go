//go:build integration

package saga

// B27 第 2 批：RR-20260927-16 的未验证项“真实 Mongo 上的确定性写失败（例如权限 / 文档校验）没有构造；伪 Mongo 以一次注入的写错误
// 模拟‘中止 → 重跑’”。这里在隔离库上给操作状态文档集合加一个稀疏唯一索引（completion），预先放一份带同一 completion 载荷的诱饵文档：
// Reserve 在事务内读到权威回执后 markCompleted 的 UpdateOne 每次都以 DuplicateKey 失败，是确定性的写失败。
// 承诺（RR-16 记录写明的行为）：失败不静默——计数 saga.step_inbox.mark_completed_error_total 增长、Warn 带 command_id 与原因；
// 返回值不改：真实服务端因写错误中止事务，驱动重跑到事务超时，Reserve 以错误返回，回执仍是权威（Replay 仍能读到完成结果），
// 状态文档保持 pending。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/driver"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRealMongoReserveDeterministicMarkCompletedFailureIsReportedNotSilent(t *testing.T) {
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Skip("ROOST_DATAENGINE_IT_MONGO_URI is not set; run through kit/scripts/integration/dataengine-env.sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cfg := fmongo.DefaultConfig(uri)
	// 确定性失败会让驱动重跑到事务超时；把超时收紧到 5s，用例只观察行为，不等默认的 30s。
	cfg.TransactionTimeout = 5 * time.Second
	client, err := driver.NewClient(cfg, driver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())
	database := fmt.Sprintf("roost_b27b2_%d_%d", os.Getpid(), time.Now().Unix())
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := client.Database(database).Drop(cleanup); err != nil {
			t.Errorf("drop %s: %v", database, err)
		}
	}()
	inbox, err := NewDataEngineStepInbox(client, database, DataEngineStepInboxOptions{Owner: "b27b2-worker", LeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	command := dataEngineCommand("b27b2-command", "b27b2-operation", "a")
	if reservation, err := inbox.Reserve(ctx, command); err != nil || reservation.Duplicate {
		t.Fatalf("first reserve=%+v err=%v", reservation, err)
	}
	completion := Completion{CommandID: command.ID, IdempotencyKey: command.IdempotencyKey, SagaID: command.SagaID, Success: true, Data: []byte("done"), CompletedAt: time.Now().UTC().Truncate(time.Millisecond)}
	effect, err := NewCompletionEffect(completion)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := commandDigest(command)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.receipts().InsertOne(ctx, dataEngineReceipt{ID: dataEngineStepNamespace + "/" + command.ID, Digest: digest, Payload: effect.Payload}); err != nil {
		t.Fatal(err)
	}
	// 确定性写失败：completion 上的稀疏唯一索引 + 一份已带同一载荷的诱饵文档，markCompleted 的 $set completion 必然撞唯一键。
	if err := inbox.operations().EnsureIndexes(ctx, []fmongo.IndexModel{{Keys: bson.D{{Key: "completion", Value: 1}}, Name: "b27b2_completion_unique", Unique: true, Sparse: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.operations().InsertOne(ctx, bson.M{"_id": "b27b2-decoy", "status": operationStatusSettled, "completion": effect.Payload}); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	before := markCompletedErrors()
	started := time.Now()
	reservation, err := inbox.Reserve(ctx, command)
	elapsed := time.Since(started)
	grew := markCompletedErrors() - before
	t.Logf("Reserve after %s: reservation=%+v err=%v mark_completed_error_total+=%d", elapsed, reservation, err, grew)
	if grew < 1 {
		t.Fatalf("saga.step_inbox.mark_completed_error_total grew by %d, want at least 1: the deterministic markCompleted failure is silent", grew)
	}
	text := logs.String()
	if !strings.Contains(text, "level=WARN") || !strings.Contains(text, "command_id="+command.ID) || !strings.Contains(text, "mark operation attempt settled failed") {
		t.Fatalf("no warning with the command id for the swallowed markCompleted failure; logs=%q", text)
	}
	if !strings.Contains(strings.ToLower(text), "duplicate key") && !errors.Is(err, fmongo.ErrDuplicateKey) {
		t.Fatalf("neither the log nor the error names the deterministic cause (duplicate key); logs=%q err=%v", text, err)
	}
	// RR-16 记录的行为：确定性失败让事务一直重跑到超时，Reserve 以错误返回；回执仍是权威，状态文档仍 pending。
	if err == nil {
		t.Fatalf("Reserve returned no error although the attempt can never be marked settled: reservation=%+v (RR-20260927-16 documents a retry until the transaction timeout)", reservation)
	}
	if elapsed < cfg.TransactionTimeout {
		t.Logf("Reserve gave up before the transaction timeout (%s < %s): the server aborted without a transient retry", elapsed, cfg.TransactionTimeout)
	}
	replayed, found, err := inbox.Replay(ctx, command)
	// Replay 同样要 markCompleted，同样失败；它不吞错误，直接返回。
	if err == nil || found {
		t.Fatalf("Replay=%+v found=%v err=%v, want the markCompleted failure surfaced", replayed, found, err)
	}
	var state stepOperation
	if err := inbox.operations().FindOne(ctx, bson.M{"_id": command.IdempotencyKey}, &state); err != nil || state.CommandID != command.ID || state.Status != operationStatusPending {
		t.Fatalf("operation state=%+v err=%v, want the attempt still pending (the settled mark never landed)", state, err)
	}
	receiptCompletion, found, err := inbox.readReceipt(ctx, command.ID, digest)
	if err != nil || !found || string(receiptCompletion.Data) != "done" {
		t.Fatalf("receipt completion=%+v found=%v err=%v, want the authoritative receipt intact", receiptCompletion, found, err)
	}
}
