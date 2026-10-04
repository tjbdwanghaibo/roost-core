// RR-20261004-NC-10：配置的调用预算从服务发现开始生效。
package servicerpc

import (
	"context"
	"errors"
	"testing"
	"time"

	fetcd "github.com/tjbdwanghaibo/roost-core/etcd"
)

type budgetDiscovery struct {
	fetcd.IDiscovery
	inspect func(context.Context) ([]*fetcd.ServiceInfo, error)
}

func (d budgetDiscovery) Discover(ctx context.Context, _ string) ([]*fetcd.ServiceInfo, error) {
	return d.inspect(ctx)
}

func TestRPCBudgetDiscoveredCallTimeoutIncludesDiscovery(t *testing.T) {
	sentinel := errors.New("probe refused discovery")
	deadlinePresent := false
	d := budgetDiscovery{inspect: func(ctx context.Context) ([]*fetcd.ServiceInfo, error) {
		_, deadlinePresent = ctx.Deadline()
		return nil, sentinel
	}}
	b := &recordingBus{}
	client := NewDiscoveredBusClient(b, "game", 20*time.Millisecond, d)
	err := client.CallDiscoveredChecked(context.Background(), "join", nil, statusResponse{}, "fallback")
	if !errors.Is(err, sentinel) || len(b.calls) != 0 {
		t.Fatalf("error=%v transport=%v", err, b.calls)
	}
	t.Logf("configured_timeout=20ms discovery_deadline_present=%v", deadlinePresent)
	if !deadlinePresent {
		t.Fatal("discovery received an unbounded context before the configured call timeout started")
	}
}

func TestRPCBudgetDiscoveredCallPreservesCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	expected, _ := ctx.Deadline()
	d := budgetDiscovery{inspect: func(got context.Context) ([]*fetcd.ServiceInfo, error) {
		actual, ok := got.Deadline()
		if !ok || actual.After(expected) {
			t.Error("caller deadline lost")
		}
		return nil, context.DeadlineExceeded
	}}
	err := NewDiscoveredBusClient(&recordingBus{}, "game", time.Second, d).CallDiscoveredChecked(ctx, "join", nil, statusResponse{}, "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
}

func TestRPCBudgetDiscoveryWaitUsesConfiguredBudget(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	budgetDeadline := started.Add(20 * time.Millisecond)
	var actualDeadline time.Time
	d := budgetDiscovery{inspect: func(ctx context.Context) ([]*fetcd.ServiceInfo, error) {
		actualDeadline, _ = ctx.Deadline()
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	err := NewDiscoveredBusClient(&recordingBus{}, "game", 20*time.Millisecond, d).CallDiscoveredChecked(parent, "join", nil, statusResponse{}, "")
	t.Logf("configured_budget=20ms observed_wait=%v discovery_budget=%v err=%v", time.Since(started), actualDeadline.Sub(started), err)
	if !errors.Is(err, context.DeadlineExceeded) || actualDeadline.IsZero() || actualDeadline.After(budgetDeadline.Add(5*time.Millisecond)) {
		t.Fatal("cooperative discovery waited on the longer parent budget instead of the configured call budget")
	}
}

func TestRPCBudgetDiscoveryFiltersAndTransportRefuses(t *testing.T) {
	for _, mode := range []string{"empty", "invalid_candidates", "valid", "business_error"} {
		t.Run(mode, func(t *testing.T) {
			infos := []*fetcd.ServiceInfo{nil, {Sid: 0}}
			if mode == "empty" {
				infos = nil
			}
			if mode == "valid" || mode == "business_error" {
				infos = append(infos, &fetcd.ServiceInfo{Sid: 6})
			}
			b := &recordingBus{}
			client := NewDiscoveredBusClient(b, "game", time.Second, &fakeDiscovery{infos: infos})
			resp := statusResponse{}
			if mode == "business_error" {
				resp.Code = 42
				resp.Reason = "denied"
			}
			err := client.CallDiscoveredChecked(context.Background(), "join", nil, resp, "")
			if mode == "empty" || mode == "invalid_candidates" {
				if err == nil || len(b.calls) != 0 {
					t.Fatalf("err=%v calls=%v", err, b.calls)
				}
			} else if b.lastSid != 6 || (mode == "valid" && err != nil) || (mode == "business_error" && err == nil) {
				t.Fatalf("sid=%d err=%v", b.lastSid, err)
			}
		})
	}
}

type inspectBudgetBus struct {
	recordingBus
	inspect func(context.Context) error
}

func (b *inspectBudgetBus) CallTo(ctx context.Context, _ string, _ int32, _ string, _ any, _ any) error {
	return b.inspect(ctx)
}

type inspectBudgetPicker struct{ inspect func(context.Context) }

func (p inspectBudgetPicker) Pick(ctx context.Context, _ string, _ []*fetcd.ServiceInfo, _ uint64) (int32, error) {
	p.inspect(ctx)
	return 7, nil
}

func TestRPCBudgetDiscoveryPickerTransportShareDeadline(t *testing.T) {
	for _, mode := range []string{"nil_context", "short_parent", "normal"} {
		t.Run(mode, func(t *testing.T) {
			var parent context.Context
			if mode != "nil_context" {
				parent = context.Background()
			}
			if mode == "short_parent" {
				var cancel context.CancelFunc
				parent, cancel = context.WithTimeout(parent, time.Second)
				defer cancel()
			}
			var deadline time.Time
			discovery := budgetDiscovery{inspect: func(ctx context.Context) ([]*fetcd.ServiceInfo, error) {
				var ok bool
				deadline, ok = ctx.Deadline()
				if !ok {
					t.Fatal("discovery without deadline")
				}
				if mode == "short_parent" {
					expected, _ := parent.Deadline()
					if !deadline.Equal(expected) {
						t.Fatal("short parent changed")
					}
				}
				return []*fetcd.ServiceInfo{{Sid: 7}}, nil
			}}
			check := func(ctx context.Context) {
				got, _ := ctx.Deadline()
				if !got.Equal(deadline) {
					t.Fatalf("budget reset: %v -> %v", deadline, got)
				}
			}
			b := &inspectBudgetBus{inspect: func(ctx context.Context) error { check(ctx); return nil }}
			client := NewDiscoveredBusClient(b, "game", time.Minute, discovery)
			client.SetDiscoveryPicker(inspectBudgetPicker{inspect: check})
			if err := client.CallDiscoveredChecked(parent, "join", nil, statusResponse{}, ""); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRPCBudgetLateDiscoveryDoesNotStartTransport(t *testing.T) {
	b := &recordingBus{}
	// RR-20261004-07（复审 S4）：discovery 若拿不到带期限的 ctx 会一直等；testDone 让它
	// 在测试结束时退出，有界等待让这种回退变成断言失败而不是挂到 -timeout。
	testDone := make(chan struct{})
	t.Cleanup(func() { close(testDone) })
	d := budgetDiscovery{inspect: func(ctx context.Context) ([]*fetcd.ServiceInfo, error) {
		select {
		case <-ctx.Done():
		case <-testDone:
		}
		// 即使依赖在取消后返回成功候选，也不能再开始业务传输。
		return []*fetcd.ServiceInfo{{Sid: 7}}, nil
	}}
	result := make(chan error, 1)
	go func() {
		result <- NewDiscoveredBusClient(b, "game", 10*time.Millisecond, d).CallDiscoveredChecked(context.Background(), "join", nil, statusResponse{}, "")
	}()
	var err error
	select {
	case err = <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("discovery was not bounded by the configured 10ms call timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) || len(b.calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, b.calls)
	}
}
