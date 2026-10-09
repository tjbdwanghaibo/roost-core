package gateway

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"testing"
)

// RR-20261004-NC-06：观察回调失败不能让 endpoint panic 突破稳定错误边界。
func TestRecoverIsolatesReporterFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
	}{{"string", "reporter failed"}, {"error", errors.New("reporter failed")}, {"nil", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			reports := 0
			endpoint := Recover(func(context.Context, any) { reports++; panic(tc.value) })(EndpointFunc(func(context.Context, Session, Request) (any, error) { panic("private endpoint detail") }))
			var escaped any
			var ret any
			var err error
			func() {
				defer func() { escaped = recover() }()
				ret, err = endpoint.Handle(context.Background(), nil, Request{})
			}()
			if escaped != nil || ret != nil || !errors.Is(err, ErrEndpointPanic) || reports != 1 {
				t.Fatalf("ret=%v err=%v escaped=%v reports=%d", ret, err, escaped, reports)
			}
		})
	}
}

type panicDiagnosticWriter struct{}

func (panicDiagnosticWriter) Write([]byte) (int, error) { panic("diagnostic sink failed") }

func TestRecoverIsolatesDiagnosticFailure(t *testing.T) {
	previous := slog.Default()
	previousOutput := log.Default().Writer()
	slog.SetDefault(slog.New(slog.NewTextHandler(panicDiagnosticWriter{}, nil)))
	// slog.SetDefault 同时替换标准 log 的 writer，恢复旧 defaultHandler 时不会自动恢复它。
	// 恶意诊断 sink 只属于本回归，不能泄漏给后续 TCP 启动与生命周期测试。
	t.Cleanup(func() { slog.SetDefault(previous); log.SetOutput(previousOutput) })
	endpoint := Recover(func(context.Context, any) { panic("report failed") })(EndpointFunc(func(context.Context, Session, Request) (any, error) { panic("endpoint") }))
	_, err := endpoint.Handle(context.Background(), nil, Request{})
	if !errors.Is(err, ErrEndpointPanic) {
		t.Fatalf("error=%v", err)
	}
}

func TestRecoverPreservesNormalContract(t *testing.T) {
	t.Run("nil_reporter", func(t *testing.T) {
		_, err := Recover(nil)(EndpointFunc(func(context.Context, Session, Request) (any, error) { panic(nil) })).Handle(context.Background(), nil, Request{})
		if !errors.Is(err, ErrEndpointPanic) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("original_context_and_panic", func(t *testing.T) {
		type key struct{}
		ctx := context.WithValue(context.Background(), key{}, "request")
		reports := 0
		_, err := Recover(func(got context.Context, v any) {
			reports++
			if got != ctx || v != "endpoint" {
				t.Error("report context or panic lost")
			}
		})(EndpointFunc(func(context.Context, Session, Request) (any, error) { panic("endpoint") })).Handle(ctx, nil, Request{})
		if !errors.Is(err, ErrEndpointPanic) || reports != 1 {
			t.Fatalf("error=%v reports=%d", err, reports)
		}
	})
	t.Run("healthy", func(t *testing.T) {
		businessErr := errors.New("business")
		reports := 0
		ret, err := Recover(func(context.Context, any) { reports++ })(EndpointFunc(func(context.Context, Session, Request) (any, error) { return 42, businessErr })).Handle(context.Background(), nil, Request{})
		if ret != 42 || err != businessErr || reports != 0 {
			t.Fatalf("ret=%v err=%v reports=%d", ret, err, reports)
		}
	})
}
