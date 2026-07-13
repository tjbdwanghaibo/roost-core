package nats

import (
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/cube-core/nats"

	gojs "github.com/nats-io/nats.go/jetstream"
)

func TestJetStreamStreamConfigMapping(t *testing.T) {
	got := toJetStreamStreamConfig(fnats.JetStreamConfig{
		Name:       "CUBE_DOMAIN_EVENTS",
		Subjects:   []string{"cube.domain.>"},
		Storage:    fnats.JetStreamStorageMemory,
		MaxAge:     time.Hour,
		Duplicates: time.Minute,
		Replicas:   2,
		MaxBytes:   1024,
	})

	if got.Name != "CUBE_DOMAIN_EVENTS" || got.Subjects[0] != "cube.domain.>" {
		t.Fatalf("stream identity mismatch: %+v", got)
	}
	if got.Storage != gojs.MemoryStorage {
		t.Fatalf("storage=%v, want memory", got.Storage)
	}
	if got.MaxAge != time.Hour || got.Duplicates != time.Minute || got.Replicas != 2 || got.MaxBytes != 1024 {
		t.Fatalf("stream limits mismatch: %+v", got)
	}
}

func TestJetStreamConsumerConfigMappingDefaults(t *testing.T) {
	got := toJetStreamConsumerConfig(fnats.JetStreamConsumerConfig{
		Name:          "task-progress",
		Durable:       "task-progress",
		FilterSubject: "cube.domain.battle.settled",
		DeliverPolicy: fnats.JetStreamDeliverNew,
		AckWait:       3 * time.Second,
		MaxDeliver:    5,
	})

	if got.Name != "task-progress" || got.Durable != "task-progress" {
		t.Fatalf("consumer identity mismatch: %+v", got)
	}
	if got.FilterSubject != "cube.domain.battle.settled" {
		t.Fatalf("filter=%q", got.FilterSubject)
	}
	if got.DeliverPolicy != gojs.DeliverNewPolicy {
		t.Fatalf("deliver policy=%v, want new", got.DeliverPolicy)
	}
	if got.AckPolicy != gojs.AckExplicitPolicy || got.AckWait != 3*time.Second || got.MaxDeliver != 5 {
		t.Fatalf("ack config mismatch: %+v", got)
	}
}
