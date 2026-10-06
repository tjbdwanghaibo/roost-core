//go:build integration

package saga

// N06 S5 review（2026-10-06）在真实依赖上的两组用例：
//
//  1. TestRealMongoCoordinatorLeaseTakeover：协调器 A 租约过期、B 接管、A 晚到 Apply 的三种顺序，跑完整的
//     MongoStore.ClaimDue（单测在 mongotest 上跑同一组用例，RR-20261006-08 之后同样走完整的 ClaimDue）。
//  2. TestRealSagaCrossProcessKillRecovers：真实 NATS JetStream + Mongo 副本集上起两个协调器 + Mongo 步骤
//     进程（同一组 durable），跑一批两步 saga，中途 SIGKILL 其中一个，只留另一个；核对恢复后所有 saga 完成、
//     每个操作的业务写恰好一份、每个 CommandID 的业务事务至多提交一次、outbox 排空、没有残留租约。
//     saga 方向 ②（2026-10-06）之后再断言：每个操作实例的业务事务恰好提交一次（相邻两次尝试不再都提交；
//     N06 S5 的第 2、3 次实跑各有 1 个操作被两次尝试提交，当时靠业务按 IdempotencyKey 幂等兜住）。
//
// 资源：库 roost_revn06s5_<pid>_<ns>、流 REVN06S5_<pid>_<ns>、subject 前缀 revn06s5x<pid>x<ns>，用后删除。
// 运行：source ~/.roost-it/roost-dataengine-it/env.sh 后
//   GOWORK=off go test -tags integration -count=1 -run '^TestRealMongoCoordinatorLeaseTakeover$|^TestRealSagaCrossProcessKillRecovers$' ./saga/

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/driver"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	ndriver "github.com/tjbdwanghaibo/roost-core/nats/driver"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func realRevn06s5Mongo(t *testing.T) (*driver.Client, string) {
	t.Helper()
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Skip("ROOST_DATAENGINE_IT_MONGO_URI is not set; source ~/.roost-it/roost-dataengine-it/env.sh")
	}
	client, err := driver.NewClient(fmongo.DefaultConfig(uri), driver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	database := fmt.Sprintf("roost_revn06s5_%d_%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		// 被 SIGKILL 的进程留下的事务在服务端保持打开、持有锁，直到 transactionLifetimeLimitSeconds（默认 60s）
		// 才被回收；dropDatabase 要等它们。
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if err := client.Database(database).Drop(ctx); err != nil {
			t.Errorf("drop %s: %v", database, err)
		}
		_ = client.Close(context.Background())
	})
	return client, database
}

func TestRealMongoCoordinatorLeaseTakeover(t *testing.T) {
	client, database := realRevn06s5Mongo(t)
	runCoordinatorTakeoverCases(t, func(*testing.T) fmongo.IMongo { return client }, database)
}

// crossProcessDefinition 是两步 saga，两步都是 Mongo 步骤。超时短，让被杀进程手里的尝试很快由协调器重发。
func crossProcessDefinition(prefix string) Definition {
	step := func(name string) Step {
		return Step{Name: name, ForwardTopic: prefix + "." + name, CompensateTopic: prefix + "." + name + ".cancel",
			// MaxAttempts 让步骤预算（约 30 × 4s）长过被杀进程遗留事务的锁（服务端默认 60s 才回收），
			// 用来测出恢复时间；默认预算（5s × 5 次）短于它，见 review 记录 O-S5-3。
			Timeout: 3 * time.Second, MaxAttempts: 30, BackoffMin: 100 * time.Millisecond, BackoffMax: time.Second}
	}
	return Definition{Type: "revn06s5", Version: 1, Steps: []Step{step("reserve"), step("ship")}}
}

func crossProcessAssembly(database, stream, prefix, owner string) AssemblyConfig {
	options := DefaultOptions()
	options.Owner = owner
	options.LeaseDuration = 3 * time.Second
	options.StoreTimeout = 500 * time.Millisecond
	options.PublishTimeout = 500 * time.Millisecond
	options.PollInterval = 50 * time.Millisecond
	options.CoordinatorBatch = 2
	options.PublisherBatch = 1
	consumer := func(durable string) (string, time.Duration, time.Duration, time.Duration, time.Duration) {
		return durable, 5 * time.Second, 2 * time.Second, 100 * time.Millisecond, time.Second
	}
	resultDurable, ackWait, process, nakMin, nakMax := consumer("revn06s5-result")
	return AssemblyConfig{
		Store:       MongoStoreOptions{Database: database, CompletionReceiptTTL: time.Hour},
		Engine:      options,
		Prefix:      prefix,
		Stream:      fnats.JetStreamConfig{Name: stream, Subjects: []string{prefix + ".>"}, Storage: fnats.JetStreamStorageFile, MaxAge: 30 * time.Minute, Duplicates: 2 * time.Minute, Replicas: 1, MaxBytes: 64 << 20},
		Completions: CompletionConsumerConfig{Stream: stream, Durable: resultDurable, SubjectPrefix: prefix, AckWait: ackWait, ProcessTimeout: process, NakBackoffMin: nakMin, NakBackoffMax: nakMax},
		// 测试流同时承载启动 / 原生完成 effect 的 subject（本用例不用它们，但 Assembly 必须订阅成功）。
		Starts: NestStartConsumerConfig{Stream: stream, Durable: "revn06s5-start", EffectPrefix: prefix + ".effect", AckWait: ackWait, ProcessTimeout: process, NakBackoffMin: nakMin, NakBackoffMax: nakMax},
	}
}

// TestRealSagaCrossProcessChild 只在父用例拉起的子进程里运行（REVN06S5_CHILD=1）：一个完整的协调器 Assembly
// 加两个 Mongo 步骤消费者，跑到被 SIGKILL 或收到 SIGTERM。
func TestRealSagaCrossProcessChild(t *testing.T) {
	if os.Getenv("REVN06S5_CHILD") != "1" {
		t.Skip("child process of TestRealSagaCrossProcessKillRecovers")
	}
	database, stream, prefix, owner := os.Getenv("REVN06S5_DB"), os.Getenv("REVN06S5_STREAM"), os.Getenv("REVN06S5_PREFIX"), os.Getenv("REVN06S5_OWNER")
	mongoClient, err := driver.NewClient(fmongo.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")), driver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	natsClient, err := ndriver.NewClient(fnats.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")), ndriver.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	jetStream, err := ndriver.NewJetStreamClient(natsClient)
	if err != nil {
		t.Fatal(err)
	}
	definition := crossProcessDefinition(prefix)
	asm, err := Assemble(mongoClient, jetStream, crossProcessAssembly(database, stream, prefix, owner), definition)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := asm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	inbox, err := NewMongoCommandInbox(mongoClient, database, "_saga_step_inbox", CommandInboxOptions{ReceiptTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	// 业务写：effects 按操作实例（IdempotencyKey）计提交次数，executions 按 CommandID 计。两者都在 handler 的
	// Mongo 事务里，只有提交了的执行才计数。handler 自己不做按操作的幂等（$inc 每次提交都加一），所以 effects.commits
	// 就是这个操作实例生效的次数；框架的收件箱契约（SAGA.md「原生步骤执行契约」）要求它恰好是 1。
	handler := func(txCtx context.Context, command Command) (Completion, error) {
		// 先写后等：被杀时在途的事务已经写了 effects / executions，锁在服务端保留到事务过期。
		defer time.Sleep(time.Duration(rand.IntN(300)) * time.Millisecond)
		var ignored bson.M
		upsert := fmongo.FindOneAndUpdateOption{Upsert: true, ReturnAfter: true}
		if err := mongoClient.Database(database).Collection("effects").FindOneAndUpdate(txCtx, bson.M{"_id": command.IdempotencyKey}, bson.M{"$inc": bson.M{"commits": 1}}, &ignored, upsert); err != nil {
			return Completion{}, err
		}
		if err := mongoClient.Database(database).Collection("executions").FindOneAndUpdate(txCtx, bson.M{"_id": command.ID}, bson.M{"$inc": bson.M{"commits": 1}, "$set": bson.M{"owner": owner}}, &ignored, upsert); err != nil {
			return Completion{}, err
		}
		return Completion{Success: true}, nil
	}
	var subs []fnats.IJetStreamSubscription
	for _, step := range definition.Steps {
		sub, err := SubscribeMongoStep(ctx, jetStream, asm.Transport, inbox, StepConsumerConfig{Stream: stream, Durable: "revn06s5-step-" + step.Name, Topic: step.ForwardTopic, AckWait: 5 * time.Second, NakBackoffMin: 100 * time.Millisecond, NakBackoffMax: time.Second}, handler)
		if err != nil {
			t.Fatal(err)
		}
		subs = append(subs, sub)
	}
	fmt.Println("REVN06S5_READY " + owner)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM)
	<-stop
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	for _, sub := range subs {
		sub.Drain()
	}
	_ = asm.Stop(stopCtx)
	natsClient.Close()
	_ = mongoClient.Close(stopCtx)
}

type crossProcessChild struct {
	owner string
	cmd   *exec.Cmd
	log   string
	ready chan struct{}
}

func startCrossProcessChild(t *testing.T, env []string, owner, logDir string) *crossProcessChild {
	t.Helper()
	child := &crossProcessChild{owner: owner, log: filepath.Join(logDir, owner+".log"), ready: make(chan struct{})}
	logFile, err := os.Create(child.log)
	if err != nil {
		t.Fatal(err)
	}
	child.cmd = exec.Command(os.Args[0], "-test.run=^TestRealSagaCrossProcessChild$", "-test.v", "-test.count=1", "-test.timeout=10m")
	child.cmd.Env = append(append(os.Environ(), env...), "REVN06S5_CHILD=1", "REVN06S5_OWNER="+owner)
	child.cmd.Stderr = logFile
	stdout, err := child.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		scanner := bufio.NewScanner(stdout)
		signalled := false
		for scanner.Scan() {
			line := scanner.Text()
			_, _ = fmt.Fprintln(logFile, line)
			if !signalled && strings.HasPrefix(line, "REVN06S5_READY") {
				signalled = true
				close(child.ready)
			}
		}
	}()
	t.Cleanup(func() {
		if child.cmd.ProcessState == nil {
			_ = child.cmd.Process.Kill()
			_ = child.cmd.Wait()
		}
		_ = logFile.Close()
	})
	select {
	case <-child.ready:
	case <-time.After(60 * time.Second):
		t.Fatalf("child %s did not become ready; log %s", owner, child.log)
	}
	return child
}

func deleteRevn06s5Stream(t *testing.T, url, stream string) {
	t.Helper()
	nc, err := gonats.Connect(url, gonats.Timeout(2*time.Second))
	if err != nil {
		t.Errorf("cleanup: connect: %v", err)
		return
	}
	defer nc.Close()
	js, err := gojs.New(nc)
	if err != nil {
		t.Errorf("cleanup: JetStream: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := js.DeleteStream(ctx, stream); err != nil && !errors.Is(err, gojs.ErrStreamNotFound) {
		t.Errorf("cleanup: delete stream %s: %v", stream, err)
	}
}

func TestRealSagaCrossProcessKillRecovers(t *testing.T) {
	natsURL := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if natsURL == "" {
		t.Skip("ROOST_DATAENGINE_IT_NATS_URL is not set; source ~/.roost-it/roost-dataengine-it/env.sh")
	}
	client, database := realRevn06s5Mongo(t)
	suffix := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano())
	stream, prefix := "REVN06S5_"+suffix, "revn06s5x"+strings.ReplaceAll(suffix, "_", "x")
	t.Cleanup(func() { deleteRevn06s5Stream(t, natsURL, stream) })
	logDir := os.Getenv("REVN06S5_LOG_DIR")
	if logDir == "" {
		logDir = t.TempDir()
	}
	env := []string{"REVN06S5_DB=" + database, "REVN06S5_STREAM=" + stream, "REVN06S5_PREFIX=" + prefix}

	a := startCrossProcessChild(t, env, "coordinator-a", logDir)
	b := startCrossProcessChild(t, env, "coordinator-b", logDir)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	store, err := NewMongoStore(client, MongoStoreOptions{Database: database, CompletionReceiptTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	starter, err := NewEngine(store, PublishFunc(func(context.Context, Command) error { return nil }), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := starter.Register(crossProcessDefinition(prefix)); err != nil {
		t.Fatal(err)
	}
	const sagas = 60
	for i := 0; i < sagas; i++ {
		if _, err := starter.StartSaga(ctx, StartRequest{Type: "revn06s5", DefinitionVersion: 1, BusinessKey: fmt.Sprintf("order-%03d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	sagasColl := client.Database(database).Collection(defaultSagaCollection)
	count := func(filter bson.M) int64 {
		n, err := sagasColl.CountDocuments(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	// 等一部分 saga 完成、其余在途时强杀 A：A 手里有协调器租约、outbox 租约、未 ack 的步骤 / 结果消息和在途的步骤事务。
	for count(bson.M{"status": StatusCompleted}) < sagas/4 {
		if ctx.Err() != nil {
			t.Fatalf("no progress before the kill; logs in %s", logDir)
		}
		time.Sleep(20 * time.Millisecond)
	}
	completedAtKill := count(bson.M{"status": StatusCompleted})
	heldByA := count(bson.M{"lease_owner": a.owner, "lease_until": bson.M{"$gt": time.Now().UTC()}})
	if err := a.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = a.cmd.Wait()
	killedAt := time.Now()
	t.Logf("SIGKILL coordinator-a with %d/%d sagas completed, %d saga leases held by it", completedAtKill, sagas, heldByA)

	for count(bson.M{"status": StatusCompleted}) < sagas {
		if ctx.Err() != nil {
			var stuck []recordDoc
			_ = sagasColl.Find(context.Background(), bson.M{"status": bson.M{"$ne": StatusCompleted}}, &stuck)
			t.Fatalf("only %d/%d sagas completed after the kill; first stuck: %+v; logs in %s", count(bson.M{"status": StatusCompleted}), sagas, firstOf(stuck), logDir)
		}
		time.Sleep(50 * time.Millisecond)
	}
	recovered := time.Since(killedAt)

	// outbox：完成时 CloseOperation 删掉同一操作残留的命令；被杀进程领取后未 ack 的条目也在其中。
	outbox := client.Database(database).Collection(defaultOutboxCollection)
	for {
		n, err := outbox.CountDocuments(ctx, bson.M{})
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("outbox still holds %d commands after every saga completed", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := count(bson.M{"completed_steps": bson.M{"$ne": 2}}); n != 0 {
		t.Errorf("%d completed sagas do not have both steps recorded", n)
	}
	if n := count(bson.M{"lease_owner": bson.M{"$exists": true, "$ne": ""}}); n != 0 {
		t.Errorf("%d sagas still carry a coordinator lease after completing", n)
	}
	type counted struct {
		ID      string `bson:"_id"`
		Commits int    `bson:"commits"`
		Owner   string `bson:"owner"`
	}
	var effects, executions []counted
	if err := client.Database(database).Collection("effects").Find(ctx, bson.M{}, &effects); err != nil {
		t.Fatal(err)
	}
	if err := client.Database(database).Collection("executions").Find(ctx, bson.M{}, &executions); err != nil {
		t.Fatal(err)
	}
	if len(effects) != sagas*2 {
		t.Errorf("business effects for %d operations, want %d (one per saga step)", len(effects), sagas*2)
	}
	rerunOps, byB := 0, 0
	for _, effect := range effects {
		if effect.Commits != 1 {
			t.Errorf("operation %s committed its business transaction %d times, want exactly once per operation", effect.ID, effect.Commits)
		}
		if effect.Commits > 1 {
			rerunOps++
			var owners []string
			for _, execution := range executions {
				if strings.HasPrefix(execution.ID, effect.ID+":") {
					owners = append(owners, execution.ID+"@"+execution.Owner)
				}
			}
			t.Logf("operation %s committed by %d attempts: %v", effect.ID, effect.Commits, owners)
		}
	}
	for _, execution := range executions {
		if execution.Commits != 1 {
			t.Errorf("command %s committed its business transaction %d times, want at most once per CommandID", execution.ID, execution.Commits)
		}
		if execution.Owner == b.owner {
			byB++
		}
	}
	receipts, err := client.Database(database).Collection(defaultCompletionCollection).CountDocuments(ctx, bson.M{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("recovered in %v: %d sagas completed, %d operations, %d committed step executions (%d by coordinator-b), %d operations committed by more than one attempt (want 0), %d coordinator receipts",
		recovered.Round(time.Millisecond), sagas, len(effects), len(executions), byB, rerunOps, receipts)
	if err := b.cmd.Process.Signal(syscall.SIGTERM); err == nil {
		done := make(chan struct{})
		go func() { _ = b.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Errorf("coordinator-b did not stop within 20s of SIGTERM")
		}
	}
}

func firstOf(records []recordDoc) any {
	if len(records) == 0 {
		return nil
	}
	return records[0]
}
