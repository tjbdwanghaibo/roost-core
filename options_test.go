package nats

import (
	"bytes"
	fnats "github.com/tjbdwanghaibo/cube-core/nats"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
)

var errNatsDisconnectForTest = errors.New("disconnect failed")

func TestHandleNatsDisconnectLogsExpectedCloseAsInfo(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(restore) })

	var called atomic.Bool
	cfg := fnats.DefaultConfig("nats://test")
	cfg.OnDisconnect = func(error) { called.Store(true) }
	state := &natsLifecycleState{}
	state.draining.Store(true)

	handleNatsDisconnect(state, cfg, nil)

	logs := buf.String()
	if strings.Contains(logs, `"level":"ERROR"`) {
		t.Fatalf("expected graceful disconnect not to log ERROR, got %s", logs)
	}
	if !strings.Contains(logs, `"level":"INFO"`) {
		t.Fatalf("expected graceful disconnect to log INFO, got %s", logs)
	}
	if !called.Load() {
		t.Fatalf("OnDisconnect was not called")
	}
}

func TestHandleNatsDisconnectLogsUnexpectedErrorAsError(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(restore) })

	handleNatsDisconnect(&natsLifecycleState{}, fnats.DefaultConfig("nats://test"), errNatsDisconnectForTest)

	if !strings.Contains(buf.String(), `"level":"ERROR"`) {
		t.Fatalf("expected unexpected disconnect to log ERROR, got %s", buf.String())
	}
}
