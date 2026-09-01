package dataengine

import (
	"context"
	"errors"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/cube-core/app"
	"github.com/tjbdwanghaibo/cube-core/entity"
	fmongo "github.com/tjbdwanghaibo/cube-core/mongo"
	fnats "github.com/tjbdwanghaibo/cube-core/nats"
	kitcheckpoint "github.com/tjbdwanghaibo/cube-kit/checkpoint"
	"github.com/tjbdwanghaibo/cube-kit/mods"
	kitnestwal "github.com/tjbdwanghaibo/cube-kit/nestwal"
)

type modJetStream struct{ streams int }

func (stream *modJetStream) EnsureStream(context.Context, fnats.JetStreamConfig) error {
	stream.streams++
	return nil
}
func (*modJetStream) Publish(context.Context, string, []byte, fnats.JetStreamPublishOptions) (fnats.JetStreamPublishAck, error) {
	return fnats.JetStreamPublishAck{}, nil
}
func (*modJetStream) Subscribe(context.Context, fnats.JetStreamConsumerConfig, fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	return nil, errors.New("unused")
}

func TestPersistenceModulesAreExclusiveAndDefaultRemainsCheckpoint(t *testing.T) {
	defaultConfig := viper.New()
	dataMod := NewMod()
	if err := dataMod.Init(defaultConfig); err != nil {
		t.Fatal(err)
	}
	if err := dataMod.Provide(app.NewRegistry(defaultConfig)); err != nil || dataMod.Runtime() != nil {
		t.Fatalf("default dataengine provide err=%v runtime=%v", err, dataMod.Runtime())
	}

	dataConfig := viper.New()
	dataConfig.Set("persistence.engine", "dataengine")
	checkpointMod := kitcheckpoint.NewMod()
	legacyWALMod := kitnestwal.NewMod(false)
	if err := checkpointMod.Init(dataConfig); err != nil {
		t.Fatal(err)
	}
	if err := legacyWALMod.Init(dataConfig); err != nil {
		t.Fatal(err)
	}
	emptyRegistry := app.NewRegistry(dataConfig)
	if err := checkpointMod.Provide(emptyRegistry); err != nil || checkpointMod.Backend() != nil {
		t.Fatalf("inactive checkpoint err=%v backend=%v", err, checkpointMod.Backend())
	}
	if err := legacyWALMod.Provide(emptyRegistry); err != nil || legacyWALMod.Runtime() != nil {
		t.Fatalf("inactive legacy WAL err=%v runtime=%v", err, legacyWALMod.Runtime())
	}
}

func TestDataEngineModRecoversBeforeReadyAndOwnsNestOptions(t *testing.T) {
	cfg := viper.New()
	cfg.Set("persistence.engine", "dataengine")
	cfg.Set("dataengine.wal.writer_version", 2)
	cfg.Set("dataengine.wal.dir", t.TempDir())
	database := &mongoStoreFakeDatabase{}
	mongoClient := &mongoStoreFakeClient{db: database}
	jetStream := &modJetStream{}
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModMongo, fmongo.IMongo(mongoClient)); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(mods.ModNatsJetStream, fnats.IJetStream(jetStream)); err != nil {
		t.Fatal(err)
	}
	access := entity.NewManagerAccess(entity.NewEntityManager())
	mod := NewMod(WithEntityAccess(access))
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if mod.Runtime() != nil || len(mod.NestOptions()) == 0 {
		t.Fatalf("runtime=%v options=%d; Provide must expose a lazy committer without opening WAL", mod.Runtime(), len(mod.NestOptions()))
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	defer mod.Stop()
	if mod.Runtime() == nil || !mod.Runtime().Ready() || len(mod.NestOptions()) == 0 || jetStream.streams != 1 {
		t.Fatalf("runtime=%v ready=%v options=%d streams=%d", mod.Runtime(), mod.Runtime() != nil && mod.Runtime().Ready(), len(mod.NestOptions()), jetStream.streams)
	}
}

func TestPersistenceModulesRejectForcedDoubleWrite(t *testing.T) {
	cfg := viper.New()
	cfg.Set("checkpoint.enabled", true)
	cfg.Set("dataengine.enabled", true)
	for name, init := range map[string]func(*viper.Viper) error{
		"checkpoint": kitcheckpoint.NewMod().Init,
		"legacy-wal": kitnestwal.NewMod(false).Init,
		"dataengine": NewMod().Init,
	} {
		t.Run(name, func(t *testing.T) {
			if err := init(cfg); !errors.Is(err, mods.ErrPersistenceEngineSelection) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestValidateCutoverChecksBothRollbackBoundaries(t *testing.T) {
	if err := ValidateCutover("checkpoint", "dataengine", CutoverState{CheckpointPending: 1}); err == nil {
		t.Fatal("cutover accepted pending checkpoint work")
	}
	if err := ValidateCutover("checkpoint", "dataengine", CutoverState{}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCutover("dataengine", "checkpoint", CutoverState{}); err == nil {
		t.Fatal("rollback accepted patch-only generated DAOs")
	}
	if err := ValidateCutover("dataengine", "checkpoint", CutoverState{
		CheckpointRollbackSupported: true, ProjectorHealthy: true, OutboxStagingDurable: true,
	}); err != nil {
		t.Fatal(err)
	}
}
