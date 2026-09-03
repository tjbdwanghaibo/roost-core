package nats

import (
	"github.com/tjbdwanghaibo/roost-core/bus"
	"testing"
	"time"

	"github.com/spf13/viper"
)

func TestJetStreamRPCConfigFromViper(t *testing.T) {
	cfg := viper.New()
	cfg.Set("nats.rpc.transport", "jetstream")
	cfg.Set("nats.rpc.request_stream", "RPC_REQ")
	cfg.Set("nats.rpc.response_stream", "RPC_RESP")
	cfg.Set("nats.rpc.ack_wait", "3s")
	cfg.Set("nats.rpc.max_deliver", 9)
	cfg.Set("nats.rpc.request_ttl", "11s")
	cfg.Set("nats.rpc.call_timeout", "12s")
	cfg.Set("nats.rpc.stream_max_age", "13m")
	cfg.Set("nats.rpc.duplicates", "14s")
	cfg.Set("nats.rpc.replicas", 2)
	cfg.Set("nats.rpc.max_bytes", int64(1024))

	got, enabled := jetStreamRPCConfigFromViper(cfg)
	if !enabled {
		t.Fatal("jetstream rpc config should be enabled")
	}
	if got.RequestStream != "RPC_REQ" || got.ResponseStream != "RPC_RESP" {
		t.Fatalf("streams = %q/%q", got.RequestStream, got.ResponseStream)
	}
	if got.AckWait != 3*time.Second || got.MaxDeliver != 9 || got.RequestTTL != 11*time.Second || got.CallTimeout != 12*time.Second {
		t.Fatalf("retry/timeouts = %+v", got)
	}
	if got.StreamMaxAge != 13*time.Minute || got.Duplicates != 14*time.Second || got.Replicas != 2 || got.MaxBytes != 1024 {
		t.Fatalf("stream options = %+v", got)
	}
}

func TestJetStreamRPCConfigFromViperDisabledByDefault(t *testing.T) {
	got, enabled := jetStreamRPCConfigFromViper(viper.New())
	if enabled {
		t.Fatalf("jetstream rpc config should be disabled by default: %+v", got)
	}
	if got != (bus.JetStreamRPCConfig{}) {
		t.Fatalf("disabled config = %+v", got)
	}
}
