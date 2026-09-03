package dataengine

import (
	"context"
	"errors"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	"github.com/tjbdwanghaibo/roost-kit/internal/mongofake"
	"github.com/tjbdwanghaibo/roost-kit/mods"
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

func TestDataEngineModReadsProjectionBatchByteLimit(t *testing.T) {
	cfg := viper.New()
	cfg.Set("persistence.engine", "dataengine")
	cfg.Set("dataengine.projection.batch_bytes", 2<<20)
	mod := NewMod(WithEntityAccess(entity.NewManagerAccess(entity.NewEntityManager())))
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if got := mod.cfg.projector.ReplayBatchBytes; got != 2<<20 {
		t.Fatalf("projection batch bytes=%d", got)
	}
}

func TestDataEngineModDefaultsToCanonicalWALWriterV2(t *testing.T) {
	cfg := viper.New()
	cfg.Set("persistence.engine", "dataengine")
	mod := NewMod(WithEntityAccess(entity.NewManagerAccess(entity.NewEntityManager())))
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if got := mod.cfg.wal.WriterVersion; got != 2 {
		t.Fatalf("writer version=%d, want 2", got)
	}

	cfg.Set("dataengine.wal.writer_version", 1)
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if got := mod.cfg.wal.WriterVersion; got != 1 {
		t.Fatalf("explicit compatibility writer version=%d, want 1", got)
	}
}

func TestDataEngineModKeepsProjectionBatchByteDefaultForNonPositiveValues(t *testing.T) {
	for _, test := range []struct {
		name  string
		value int
	}{
		{name: "zero", value: 0},
		{name: "negative", value: -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := viper.New()
			cfg.Set("persistence.engine", "dataengine")
			cfg.Set("dataengine.projection.batch_bytes", test.value)
			mod := NewMod(WithEntityAccess(entity.NewManagerAccess(entity.NewEntityManager())))
			if err := mod.Init(cfg); err != nil {
				t.Fatal(err)
			}
			if got, want := mod.cfg.projector.ReplayBatchBytes, 4<<20; got != want {
				t.Fatalf("projection batch bytes=%d want default %d", got, want)
			}
		})
	}
}

func TestDataEngineModRecoversBeforeReadyAndOwnsNestOptions(t *testing.T) {
	cfg := viper.New()
	cfg.Set("persistence.engine", "dataengine")
	cfg.Set("dataengine.wal.writer_version", 2)
	cfg.Set("dataengine.wal.dir", t.TempDir())
	mongoClient := mongofake.NewClient()
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
