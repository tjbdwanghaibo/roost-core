package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
)

// U-0109 · C2（空洞测试）· nightly gap map `dataengine/engine` 14/20（其中 7 条在 P3b 新增的 assembly.go）。
//
// Assemble 是 kit Mod 的唯一入口：缺依赖必须在 Provide 时点名拒绝，而不是等到
// Start 时 nil 解引用；远端投影的两个依赖要么都给要么都不给，给了的 manager 必
// 须能应用远端提交。outbox 发布器对不完整的效果必须拒绝（否则 JetStream 收到一
// 条没有去重键的消息）。

type recordingJetStream struct{ published []string }

func (*recordingJetStream) EnsureStream(context.Context, fnats.JetStreamConfig) error { return nil }
func (s *recordingJetStream) Publish(_ context.Context, subject string, _ []byte, opts fnats.JetStreamPublishOptions) (fnats.JetStreamPublishAck, error) {
	s.published = append(s.published, subject+"|"+opts.MsgID)
	return fnats.JetStreamPublishAck{}, nil
}
func (*recordingJetStream) Subscribe(context.Context, fnats.JetStreamConsumerConfig, fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	return nil, errors.New("unused")
}

// remoteManagerWithoutApplier satisfies entity.IRemoteEntityManager (every method
// panics if called) but is not an entity.RemoteCommitApplier.
type remoteManagerWithoutApplier struct{ entity.IRemoteEntityManager }

type remoteManagerWithApplier struct{ entity.IRemoteEntityManager }

func (remoteManagerWithApplier) ApplyRemoteCommits(context.Context, entity.RemoteTransactionID, []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	return nil, nil
}

type remoteProjectionStoreStub struct{}

func (remoteProjectionStoreStub) ApplyRemoteCommitsInTransaction(context.Context, []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	return nil, nil
}

func TestAssembleRefusesEachMissingDependency(t *testing.T) {
	access := entity.NewManagerAccess(entity.NewEntityManager())
	full := AssemblyDeps{Mongo: mongotest.NewClient(), JetStream: &recordingJetStream{}, Access: access}
	cfg := AssemblyConfig{Mongo: MongoStoreConfig{DefaultDatabase: "game", ServerID: 1}}
	if asm, err := Assemble(full, cfg); err != nil || asm == nil || asm.Store == nil {
		t.Fatalf("baseline Assemble = (%v, %v)", asm, err)
	}
	cases := []struct {
		name string
		deps AssemblyDeps
		text string
	}{
		{"no entity access", AssemblyDeps{Mongo: full.Mongo, JetStream: full.JetStream}, "entity access is required"},
		{"access without manager", AssemblyDeps{Mongo: full.Mongo, JetStream: full.JetStream, Access: &entity.ManagerAccess{}}, "entity access is required"},
		{"no mongo", AssemblyDeps{JetStream: full.JetStream, Access: access}, "mongo client is required"},
		{"no jetstream", AssemblyDeps{Mongo: full.Mongo, Access: access}, "jetstream client is required"},
		{"remote store without manager", AssemblyDeps{Mongo: full.Mongo, JetStream: full.JetStream, Access: access, RemoteStore: remoteProjectionStoreStub{}}, "remote projection needs both"},
		{"remote manager without store", AssemblyDeps{Mongo: full.Mongo, JetStream: full.JetStream, Access: access, RemoteManager: remoteManagerWithApplier{}}, "remote projection needs both"},
		{"remote manager without applier", AssemblyDeps{Mongo: full.Mongo, JetStream: full.JetStream, Access: access, RemoteManager: remoteManagerWithoutApplier{}, RemoteStore: remoteProjectionStoreStub{}}, "no commit applier"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			asm, err := Assemble(tc.deps, cfg)
			if err == nil || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Assemble = (%v, %v), want error containing %q", asm, err, tc.text)
			}
		})
	}
	// 成对给出且 manager 能应用远端提交：装配成功。
	remote := full
	remote.RemoteManager, remote.RemoteStore = remoteManagerWithApplier{}, remoteProjectionStoreStub{}
	if _, err := Assemble(remote, cfg); err != nil {
		t.Fatalf("Assemble with remote projection = %v", err)
	}

	var none *Assembly
	if err := none.Start(context.Background()); err == nil {
		t.Fatal("nil assembly Start returned no error")
	}
	if err := (&Assembly{}).Start(context.Background()); err == nil || !strings.Contains(err.Error(), "not assembled") {
		t.Fatalf("empty assembly Start = %v", err)
	}
	if none.Runtime() != nil || none.Shutdown(context.Background()) != nil {
		t.Fatal("nil assembly must report no runtime and shut down silently")
	}
}

func TestJetStreamOutboxPublisherRefusesIncompleteEffects(t *testing.T) {
	ctx := context.Background()
	js := &recordingJetStream{}
	publisher := &jetStreamOutboxPublisher{client: js, prefix: "roost.effect"}
	good := OutboxItem{TransactionID: "tx-1"}
	good.Effect.ID, good.Effect.Topic, good.Effect.Payload = "effect-1", "hero.changed", []byte("p")
	if err := publisher.Publish(ctx, good); err != nil {
		t.Fatalf("baseline Publish = %v", err)
	}
	if len(js.published) != 1 || js.published[0] != "roost.effect.hero.changed|effect-1" {
		t.Fatalf("published = %v, want subject under the prefix and the effect id as MsgID", js.published)
	}

	noID := good
	noID.Effect.ID = ""
	noTopic := good
	noTopic.Effect.Topic = ""
	cases := []struct {
		name      string
		publisher *jetStreamOutboxPublisher
		item      OutboxItem
	}{
		{"nil publisher", nil, good},
		{"publisher without client", &jetStreamOutboxPublisher{prefix: "roost.effect"}, good},
		{"effect without id", publisher, noID},
		{"effect without topic", publisher, noTopic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(js.published)
			if err := tc.publisher.Publish(ctx, tc.item); err == nil || !strings.Contains(err.Error(), "invalid JetStream publish") {
				t.Fatalf("Publish = %v, want invalid-publish error", err)
			}
			if len(js.published) != before {
				t.Fatal("an invalid effect reached JetStream")
			}
		})
	}
}
