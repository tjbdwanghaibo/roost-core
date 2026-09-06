//go:build integration

package nats

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/bus"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	"github.com/tjbdwanghaibo/roost-kit/mods"
)

type rpcToxiproxy struct{ base string }

func (c rpcToxiproxy) do(t *testing.T, method, path string, body any) {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, c.base+path, &payload)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("toxiproxy %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("toxiproxy %s %s: status %d", method, path, resp.StatusCode)
	}
}

func (c rpcToxiproxy) blackholeNATS(t *testing.T) {
	t.Helper()
	for index := 1; index <= 3; index++ {
		c.do(t, http.MethodPost, fmt.Sprintf("/proxies/nats-%d/toxics", index), map[string]any{
			"name": "halfopen", "type": "timeout", "stream": "downstream", "toxicity": 1.0, "attributes": map[string]any{"timeout": 0},
		})
	}
}

// Fault matrix, fifth slice: a JetStream RPC call while NATS is half-open
// (connections up, no byte comes back) must return within the caller's
// deadline — not hang on the publish acknowledgement or on the response
// consumer — and the same bus must serve calls again once the network heals.
// This is the RPC path's version of the Redis finding (U-0061): the timeout
// the caller wrote is the contract.
func TestToxicJetStreamRPCCallHonoursItsDeadlineWhileHalfOpen(t *testing.T) {
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" {
		t.Skip("set ROOST_DATAENGINE_IT=1 or use scripts/integration/dataengine-env.sh test")
	}
	api, natsURL := os.Getenv("ROOST_DATAENGINE_IT_TOXIPROXY_URL"), os.Getenv("ROOST_DATAENGINE_IT_NATS_PROXIED_URL")
	if api == "" || natsURL == "" {
		if os.Getenv("ROOST_IT_TOXIPROXY") == "1" {
			t.Fatal("ROOST_IT_TOXIPROXY=1 but the environment exported no toxiproxy")
		}
		t.Skip("toxiproxy-server not installed; network fault tests need it")
	}
	proxy := rpcToxiproxy{base: api}
	proxy.do(t, http.MethodPost, "/reset", nil)
	t.Cleanup(func() { proxy.do(t, http.MethodPost, "/reset", nil) })

	suffix := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano())
	cfg := viper.New()
	cfg.Set("sid", int32(903))
	cfg.Set("server_type", "rpcit")
	cfg.Set("nats.url", natsURL)
	cfg.Set("nats.ignore_discovered_servers", true)
	cfg.Set("nats.rpc.transport", "jetstream")
	cfg.Set("nats.rpc.request_stream", "ROOST_IT_RPC_REQ_"+suffix)
	cfg.Set("nats.rpc.response_stream", "ROOST_IT_RPC_RESP_"+suffix)
	cfg.Set("nats.rpc.call_timeout", 500*time.Millisecond)
	cfg.Set("nats.rpc.setup_timeout", 20*time.Second)
	cfg.Set("nats.rpc.max_bytes", int64(16<<20))

	mod := NewNatsMod(nil)
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	registry := app.NewRegistry(cfg)
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mod.StopWithContext(context.Background()) })
	ibus, ok := app.Lookup[bus.IBus](registry, mods.ModBus)
	if !ok {
		t.Fatal("bus capability not published")
	}
	b := ibus.(*bus.Bus)
	if err := b.HandleRpc("Ping", func(*bus.RpcContext) (any, error) { return map[string]string{"pong": "ok"}, nil }); err != nil {
		t.Fatal(err)
	}
	var resp map[string]string
	baseline := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return b.CallReliable(ctx, "rpcit", "Ping", map[string]string{}, &resp)
	}
	if err := baseline(); err != nil || resp["pong"] != "ok" {
		t.Fatalf("baseline reliable call: resp=%v err=%v", resp, err)
	}

	proxy.blackholeNATS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	started := time.Now()
	err := b.CallReliable(ctx, "rpcit", "Ping", map[string]string{}, &resp)
	cancel()
	elapsed := time.Since(started)
	t.Logf("reliable call while half-open: err=%v elapsed=%s", err, elapsed)
	if err == nil {
		t.Fatal("a call whose bytes never come back reported success")
	}
	if !errors.Is(err, fnats.ErrTimeout) && !errors.Is(err, context.DeadlineExceeded) {
		t.Logf("note: error is neither ErrTimeout nor DeadlineExceeded: %v", err)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("call took %s under a 500ms deadline; the caller's timeout was not honoured", elapsed)
	}

	proxy.do(t, http.MethodPost, "/reset", nil)
	deadline := time.Now().Add(45 * time.Second)
	for {
		if err := baseline(); err == nil && resp["pong"] == "ok" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reliable calls did not recover after the network healed: %v", baseline())
		}
		time.Sleep(500 * time.Millisecond)
	}
}
