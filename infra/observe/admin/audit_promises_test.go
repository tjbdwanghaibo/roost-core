package admin

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestAdminExecutionWritesCorrelatedAuditWithoutPayload(t *testing.T) {
	for _, kind := range []string{"ok", "error", "panic", "unknown", "missing"} {
		t.Run(kind, func(t *testing.T) {
			var buf bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
			defer slog.SetDefault(old)
			r := NewRegistry()
			if kind != "missing" {
				_ = r.Register(CommandDef{Name: "b7", Handler: func(context.Context, Command) (Result, error) {
					switch kind {
					case "error":
						return Result{}, errors.New("failed")
					case "panic":
						panic("failed")
					case "unknown":
						return Result{}, context.DeadlineExceeded
					}
					return Result{}, nil
				}})
			}
			result, _ := r.Execute(context.Background(), Command{Name: "b7", TraceID: "trace-b7", Operator: "operator-b7", Source: "source-b7", Payload: []byte(`{"secret":"do-not-log-b7"}`)})
			got := buf.String()
			for _, want := range []string{"admin command started", "admin command finished", "trace-b7", "operator-b7", "source-b7", "outcome", "duration_ms"} {
				if !strings.Contains(got, want) {
					t.Errorf("audit missing %q: %s", want, got)
				}
			}
			if strings.Contains(got, "do-not-log-b7") {
				t.Fatal("audit leaked payload")
			}
			if result.TraceID != "trace-b7" {
				t.Fatalf("failure loses trace: %+v", result)
			}
		})
	}
}
