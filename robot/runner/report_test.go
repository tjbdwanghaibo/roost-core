package runner_test

import (
	"context"
	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/robot"
	"github.com/tjbdwanghaibo/roost-core/robot/action"
	"github.com/tjbdwanghaibo/roost-core/robot/protocol"
	"github.com/tjbdwanghaibo/roost-core/robot/runner"
	"github.com/tjbdwanghaibo/roost-core/robot/scenario"
	"github.com/tjbdwanghaibo/roost-core/robot/transport"
	"net"
	"sync"
	"testing"
)

func TestReportsAreRunLocalAcrossReconnect(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var connections sync.WaitGroup
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer connections.Done()
				defer conn.Close()
				for {
					p, err := wire.Read(conn, 0)
					if err != nil {
						return
					}
					if err = wire.Write(conn, []*wire.Packet{p}, 0); err != nil {
						return
					}
				}
			}()
		}
	}()
	defer func() { listener.Close(); <-acceptDone; connections.Wait() }()
	protocols := protocol.NewRegistry(protocol.JSONCodec{})
	if err := protocol.RegisterMessage[string](protocols, 101); err != nil {
		t.Fatal(err)
	}
	scenarios := scenario.NewRegistry()
	scenarios.MustRegister(scenario.New("reconnect", scenario.NodeFunc(func(ctx context.Context, rb *robot.Context) error {
		for range 2 {
			if err := rb.Do(ctx, action.NameConnect, nil); err != nil {
				return err
			}
			if _, err := rb.Session().Call(ctx, 101, 101, "x"); err != nil {
				return err
			}
		}
		return nil
	})))
	makeRun := func() *runner.Runner {
		return runner.New(runner.Config{Count: 2, Scenario: "reconnect", Transport: transport.Config{Endpoint: listener.Addr().String()}}, runner.WithProtocolRegistry(protocols), runner.WithScenarioRegistry(scenarios))
	}
	first, second := makeRun(), makeRun()
	for _, r := range []*runner.Runner{first, second} {
		if err := r.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if r.Stats().Success != 2 {
			t.Fatal(r.Stats())
		}
		m := r.MessageReport().Messages
		if len(m) != 1 || m[0].OK != 4 || m[0].Inflight != 0 {
			t.Fatal(m)
		}
		traffic := r.TrafficReport()
		expected := int64(4 * (wire.HeaderSize + len(`"x"`)))
		if traffic.MeasuredConnections != 4 || traffic.ReadBytes != expected || traffic.WrittenBytes != expected {
			t.Fatal(traffic)
		}
	}
	if first.MessageReport().Messages[0].OK != 4 {
		t.Fatal("later run contaminated earlier report")
	}
}
