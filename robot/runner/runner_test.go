package runner_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/cube-core/robot/action"
	"github.com/tjbdwanghaibo/cube-core/robot/runner"
	"github.com/tjbdwanghaibo/cube-core/robot/scenario"
	"github.com/tjbdwanghaibo/cube-core/robot/transport"
)

// startEcho serves the packet framing, echoing every request.
func startEcho(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				for {
					packet, err := transport.ReadPacketFrom(conn, 0)
					if err != nil {
						return
					}
					if err := transport.WritePacketsTo(conn, []*transport.Packet{packet}); err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		wg.Wait()
	})
	return listener.Addr().String()
}

type pingReq struct {
	Nonce int64 `json:"nonce"`
}

const msgPing = 7

func newRunner(t *testing.T, cfg runner.Config, endpoint string, extraScenario scenario.Scenario) *runner.Runner {
	t.Helper()
	cfg.Transport = transport.Config{Endpoint: endpoint}
	scenarios := scenario.NewRegistry()
	actions := action.NewRegistry()
	r := runner.New(cfg,
		runner.WithScenarioRegistry(scenarios),
		runner.WithActionRegistry(actions),
		runner.WithIdentityProvider(func(index int) (int64, error) { return int64(1000 + index), nil }),
	)
	action.MustRegisterCall[pingReq, pingReq](actions, r.Protocols(), "ping_call", msgPing)
	scenarios.MustRegister(scenario.New("ping", scenario.Sequence(
		scenario.Action(action.NameConnect, nil),
		scenario.Action("ping_call", map[string]any{"nonce": 1}),
	)))
	if extraScenario != nil {
		scenarios.MustRegister(extraScenario)
	}
	return r
}

func TestPoolExecutorRunsEveryRobotOnce(t *testing.T) {
	endpoint := startEcho(t)
	r := newRunner(t, runner.Config{Count: 8, Scenario: "ping", Ramp: runner.Ramp{Step: 4, Interval: time.Millisecond}}, endpoint, nil)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats := r.Stats()
	if stats.Started != 8 || stats.Success != 8 || stats.Failure != 0 || stats.Canceled != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.Started != stats.Success+stats.Failure+stats.Canceled {
		t.Fatalf("accounting broken: %+v", stats)
	}
}

func TestLoopingExecutorStopsAndCountsCanceled(t *testing.T) {
	endpoint := startEcho(t)
	slow := scenario.New("slow", scenario.Sequence(
		scenario.Action(action.NameConnect, nil),
		scenario.Wait(5*time.Second), // guaranteed to be interrupted
	))
	r := newRunner(t, runner.Config{
		Executor: runner.ExecutorLooping,
		Count:    3,
		Scenario: "slow",
		Duration: 150 * time.Millisecond,
	}, endpoint, slow)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats := r.Stats()
	// The run stopped mid-scenario: the robots must be accounted as
	// canceled, not silently vanish (Started == Success+Failure+Canceled).
	if stats.Canceled != 3 || stats.Failure != 0 {
		t.Fatalf("canceled accounting = %+v", stats)
	}
	if stats.Started != stats.Success+stats.Failure+stats.Canceled {
		t.Fatalf("accounting broken: %+v", stats)
	}
}

func TestStagedRampUpAndDown(t *testing.T) {
	endpoint := startEcho(t)
	loop := scenario.New("loop", scenario.Sequence(
		scenario.Action(action.NameConnect, nil),
		scenario.Wait(20*time.Millisecond),
	))
	r := newRunner(t, runner.Config{
		Executor: runner.ExecutorLooping,
		Count:    2,
		Scenario: "loop",
		Stages: []runner.Stage{
			{Target: 6, Duration: 300 * time.Millisecond},
			{Target: 1, Duration: 300 * time.Millisecond},
		},
	}, endpoint, loop)
	runDone := make(chan error, 1)
	go func() { runDone <- r.Run(context.Background()) }()
	waitOnline := func(cond func(int64) bool, what string) {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if cond(r.Stats().Online) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("%s never reached (online=%d)", what, r.Stats().Online)
	}
	waitOnline(func(n int64) bool { return n >= 6 }, "stage ramp-up to 6")
	waitOnline(func(n int64) bool { return n <= 1 }, "stage ramp-down to 1")
	if err := awaitChan(t, runDone, "the load-test run to finish"); err != nil {
		t.Fatal(err)
	}
	if stats := r.Stats(); stats.Online != 0 {
		t.Fatalf("robots leaked after stages: %+v", stats)
	}
}

func TestArrivalRateExecutorLaunchesFreshRobots(t *testing.T) {
	endpoint := startEcho(t)
	r := newRunner(t, runner.Config{
		Executor: runner.ExecutorArrivalRate,
		Rate:     100, // 100/s for ~150ms => ~15 robots
		Scenario: "ping",
		Duration: 150 * time.Millisecond,
	}, endpoint, nil)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats := r.Stats()
	if stats.Started < 5 {
		t.Fatalf("arrival-rate too slow: %+v", stats)
	}
	if stats.Started != stats.Success+stats.Failure+stats.Canceled {
		t.Fatalf("accounting broken: %+v", stats)
	}
}

func TestRunnerUnknownScenarioFails(t *testing.T) {
	r := runner.New(runner.Config{Scenario: "nope"})
	if err := r.Run(context.Background()); err == nil {
		t.Fatal("unknown scenario accepted")
	}
}

// BenchmarkTenThousandBots measures launching and draining 10k single-shot
// robots against a loopback echo server (the sizing claim behind
// "goroutine-per-bot supports万级 in one process").
func BenchmarkTenThousandBots(b *testing.B) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				for {
					packet, err := transport.ReadPacketFrom(conn, 0)
					if err != nil {
						return
					}
					if err := transport.WritePacketsTo(conn, []*transport.Packet{packet}); err != nil {
						return
					}
				}
			}()
		}
	}()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scenarios := scenario.NewRegistry()
		actions := action.NewRegistry()
		r := runner.New(runner.Config{
			Count:     10000,
			Scenario:  "ping",
			Ramp:      runner.Ramp{Step: 2000, Interval: time.Millisecond},
			Transport: transport.Config{Endpoint: listener.Addr().String()},
		},
			runner.WithScenarioRegistry(scenarios),
			runner.WithActionRegistry(actions),
		)
		action.MustRegisterCall[pingReq, pingReq](actions, r.Protocols(), "ping_call", msgPing)
		scenarios.MustRegister(scenario.New("ping", scenario.Sequence(
			scenario.Action(action.NameConnect, nil),
			scenario.Action("ping_call", map[string]any{"nonce": 1}),
		)))
		if err := r.Run(context.Background()); err != nil {
			b.Fatal(err)
		}
		if stats := r.Stats(); stats.Success != 10000 {
			b.Fatalf("stats = %+v", stats)
		}
	}
}

// awaitChan receives from ch with an upper bound. A bare receive made a broken
// property fail as a go test timeout — a stack dump after the default ten
// minutes, naming no expectation — so every wait that IS the assertion is
// bounded and says what it was waiting for.
func awaitChan[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}
