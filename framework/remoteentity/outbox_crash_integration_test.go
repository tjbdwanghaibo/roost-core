//go:build integration && (darwin || linux)

package remoteentity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
)

// E14：在真实 Mongo 返回持久回执后卡住子进程，再 SIGKILL。测试包装器只阻止
// CommitRemote 返回给 Manager，不修改生产代码；父进程从 Mongo 独立确认 Applied。
// 同 sid 新 Assembly 启动恢复 outbox，真实 Redis L2 与 JetStream 只读端必须收敛。
type crashAfterPersistence struct {
	*MongoCommitter
	ready string
}

func (s *crashAfterPersistence) CommitRemote(ctx context.Context, commit entity.RemoteCommit) (entity.RemoteCommitReceipt, error) {
	receipt, err := s.MongoCommitter.CommitRemote(ctx, commit)
	if err != nil {
		return receipt, err
	}
	if err := os.WriteFile(s.ready, []byte("applied"), 0600); err != nil {
		return receipt, err
	}
	<-ctx.Done()
	return receipt, ctx.Err()
}

type outboxCrashInput struct {
	Database string
	Prefix   string
	Commit   entity.RemoteCommit
}

func TestRemoteOutboxCrashHelper(t *testing.T) {
	path := os.Getenv("ROOST_OUTBOX_CRASH_INPUT")
	if path == "" {
		t.Skip("subprocess helper")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var input outboxCrashInput
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: input.Commit.Kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	mongo, err := mongodriver.NewClient(fmongo.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer mongo.Close(context.Background())
	storage := &crashAfterPersistence{MongoCommitter: NewMongoCommitter(mongo, input.Database, 1000, 0), ready: path + ".ready"}
	backend, err := NewBackend(newRemoteTestLoader(), storage)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.SnapshotL2KeyPrefix = input.Prefix
	assembly, err := Assemble(AssemblyDeps{Redis: realRemoteRedis(t), Backend: backend}, cfg, 1000, MongoBackendConfig{})
	if err != nil {
		t.Fatal(err)
	}
	env := &step4JetStream{t: t, ctx: ctx, url: os.Getenv("ROOST_DATAENGINE_IT_NATS_URL"), prefix: input.Prefix}
	if err := assembly.Start(ctx, env.bus(1000)); err != nil {
		t.Fatal(err)
	}
	_, err = assembly.Manager.ApplyRemoteCommits(ctx, input.Commit.TransactionID, []entity.RemoteCommit{input.Commit})
	t.Fatalf("parent must kill the process before the commit returns: %v", err)
}

func TestRealRemoteOutboxRecoversAfterCommitBeforePublicationCrash(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	fixture := newRejectionRaceFixture(t, 94731, remoteTestTxID(211))
	env := newStep4JetStream(t, ctx)
	r := realRemoteRedis(t)
	commit := fixture.commits[0]
	key := commit.Snapshots[0].Key
	cfg := DefaultConfig()
	cfg.SnapshotL2KeyPrefix = env.prefix
	cfg.CachedMaxStaleness = 100 * time.Millisecond
	b27DeleteKeys(t, r, env.prefix+":"+remoteSnapshotL2Key(key))
	l2, err := NewSnapshotL2StoreFromConfig(r, cfg)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewSnapshotClient(cfg, SnapshotClientDeps{ConsumerSID: 1001, L2: l2})
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Start(env.bus(1001)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Stop(context.Background()) })
	path := filepath.Join(t.TempDir(), "commit.json")
	data, err := json.Marshal(outboxCrashInput{Database: fixture.database, Prefix: env.prefix, Commit: commit})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRemoteOutboxCrashHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "ROOST_OUTBOX_CRASH_INPUT="+path)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		t.Logf("crashed child: %s", output.String())
	})
	for {
		if _, err := os.Stat(path + ".ready"); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("child did not reach the persisted boundary")
		case <-time.After(10 * time.Millisecond):
		}
	}
	status, err := fixture.store.CommitStatus(ctx, commit.TransactionID)
	if err != nil || status.State != entity.RemoteCommitApplied {
		t.Fatalf("before kill: status=%+v err=%v", status, err)
	}
	if got, found, err := reader.ReadSnapshot(ctx, entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadCached}); err != nil || found {
		t.Fatalf("publication happened before the crash boundary: version=%d found=%v err=%v", got.StateVersion, found, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	waited = true
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != -1 {
		t.Fatalf("expected killed child, got %v", err)
	}
	backend, err := NewBackend(newRemoteTestLoader(), fixture.store)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := Assemble(AssemblyDeps{Redis: r, Backend: backend}, cfg, 1000, MongoBackendConfig{})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := restarted.Start(ctx, env.bus(1000)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Stop(context.Background()) })
	status, err = fixture.store.CommitStatus(ctx, commit.TransactionID)
	if err != nil || status.State != entity.RemoteCommitCommitted {
		t.Fatalf("after restart: status=%+v err=%v", status, err)
	}

	// 之前的 miss 本身也受 cached_max_staleness 保护；在既定收敛预算内等它失效，
	// 不把异步兴趣发送恰好拖慢一次读取当作“立即看到”的保证。
	deadline := started.Add(cfg.CachedMaxStaleness + 1500*time.Millisecond)
	for {
		got, found, err := reader.ReadSnapshot(ctx, entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadCached})
		if err != nil {
			t.Fatal(err)
		}
		if found && got.StateVersion == commit.NextVersion && bytes.Equal(got.Payload.BytesCopy(), commit.Snapshots[0].Data) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reader did not converge: version=%d found=%v", got.StateVersion, found)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if elapsed := time.Since(started); elapsed > cfg.CachedMaxStaleness+1500*time.Millisecond {
		t.Fatalf("restart convergence exceeded the E14 budget: %v", elapsed)
	} else {
		t.Logf("Applied before SIGKILL; Committed and reader version=%d after same-sid restart in %s", commit.NextVersion, elapsed)
	}
	if pending, err := fixture.store.PendingRemoteCommits(ctx, 10); err != nil || len(pending) != 0 {
		t.Fatalf("outbox did not drain: pending=%d err=%v", len(pending), err)
	}
}
