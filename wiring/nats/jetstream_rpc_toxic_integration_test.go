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
	"strings"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// rpcToxiproxy 驱动本用例在环境 toxiproxy 上自建的代理（每个隔离 NATS 节点一个，唯一命名、临时端口），只在它们上面加删 toxic，
// 结束时删除；不用共享的 nats-1..3 代理，也不调 POST /reset（它会清掉其他会话的故障）。总线忽略发现的服务器，只连这些代理。
type rpcToxiproxy struct {
	base    string
	proxies []string
}

func (c rpcToxiproxy) call(t *testing.T, method, path string, body any) []byte {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, strings.TrimRight(c.base, "/")+path, &payload)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("toxiproxy %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	if resp.StatusCode >= 300 && !(method == http.MethodDelete && resp.StatusCode == http.StatusNotFound) {
		t.Fatalf("toxiproxy %s %s: status %d %s", method, path, resp.StatusCode, out.String())
	}
	return out.Bytes()
}

// newRPCToxiproxy creates one proxy per direct NATS URL in directURLs and
// returns the client plus the nats:// URL list that goes through them.
func newRPCToxiproxy(t *testing.T, api, directURLs string) (rpcToxiproxy, string) {
	t.Helper()
	proxy := rpcToxiproxy{base: api}
	var proxied []string
	for index, raw := range strings.Split(directURLs, ",") {
		upstream := strings.TrimPrefix(strings.TrimSpace(raw), "nats://")
		if upstream == "" {
			continue
		}
		name := fmt.Sprintf("kit-nats-rpc-toxic-%d-%d-%d", os.Getpid(), time.Now().UnixNano(), index)
		var created struct {
			Listen string `json:"listen"`
		}
		body := proxy.call(t, http.MethodPost, "/proxies", map[string]any{"name": name, "listen": "127.0.0.1:0", "upstream": upstream, "enabled": true})
		if err := json.Unmarshal(body, &created); err != nil || created.Listen == "" {
			t.Fatalf("toxiproxy create %s: %v %s", name, err, body)
		}
		t.Cleanup(func() { proxy.call(t, http.MethodDelete, "/proxies/"+name, nil) })
		proxy.proxies = append(proxy.proxies, name)
		proxied = append(proxied, "nats://"+created.Listen)
	}
	if len(proxied) == 0 {
		t.Fatalf("no NATS URL in %q", directURLs)
	}
	return proxy, strings.Join(proxied, ",")
}

// heal removes every toxic on this test's own proxies and nothing else.
func (c rpcToxiproxy) heal(t *testing.T) {
	t.Helper()
	for _, name := range c.proxies {
		var toxics []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(c.call(t, http.MethodGet, "/proxies/"+name+"/toxics", nil), &toxics); err != nil {
			t.Fatalf("toxiproxy list toxics of %s: %v", name, err)
		}
		for _, toxic := range toxics {
			c.call(t, http.MethodDelete, "/proxies/"+name+"/toxics/"+toxic.Name, nil)
		}
	}
}

// deleteRPCStreams removes the streams this run created. It connects to the
// direct NATS URL when the environment exports one, so a toxic left on the
// proxy cannot block the cleanup; streams that were never created are fine.
func deleteRPCStreams(t *testing.T, proxiedURL string, streams ...string) {
	t.Helper()
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		url = proxiedURL
	}
	client, err := gonats.Connect(url, gonats.Timeout(2*time.Second))
	if err != nil {
		t.Errorf("cleanup: connect NATS to delete %v: %v", streams, err)
		return
	}
	defer client.Close()
	js, err := gojs.New(client)
	if err != nil {
		t.Errorf("cleanup: JetStream: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, stream := range streams {
		if err := js.DeleteStream(ctx, stream); err != nil && !errors.Is(err, gojs.ErrStreamNotFound) {
			t.Errorf("cleanup: delete stream %s: %v", stream, err)
		}
	}
}

func (c rpcToxiproxy) blackholeNATS(t *testing.T) {
	t.Helper()
	for _, name := range c.proxies {
		c.call(t, http.MethodPost, "/proxies/"+name+"/toxics", map[string]any{
			"name": "halfopen", "type": "timeout", "stream": "downstream", "toxicity": 1.0, "attributes": map[string]any{"timeout": 0},
		})
	}
}

// 故障矩阵：NATS 半开（连接在、一个字节都不回）时 JetStream RPC 调用在调用方期限内返回——不卡在发布确认或响应消费者上；
// 网络恢复后同一条总线照常服务。调用方写的超时就是契约。
func TestToxicJetStreamRPCCallHonoursItsDeadlineWhileHalfOpen(t *testing.T) {
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" {
		t.Skip("set ROOST_DATAENGINE_IT=1 or use scripts/integration/dataengine-env.sh test")
	}
	api, directURL := os.Getenv("ROOST_DATAENGINE_IT_TOXIPROXY_URL"), os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if api == "" || directURL == "" {
		if os.Getenv("ROOST_IT_TOXIPROXY") == "1" {
			t.Fatal("ROOST_IT_TOXIPROXY=1 but the environment exported no toxiproxy")
		}
		t.Skip("toxiproxy-server not installed; network fault tests need it")
	}
	proxy, natsURL := newRPCToxiproxy(t, api, directURL)
	t.Cleanup(func() { proxy.heal(t) })

	suffix := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano())
	cfg := viper.New()
	cfg.Set("sid", int32(903))
	cfg.Set("server_type", "rpcit")
	cfg.Set("nats.url", natsURL)
	cfg.Set("nats.ignore_discovered_servers", true)
	cfg.Set("nats.rpc.transport", "jetstream")
	// 主题前缀也按轮次唯一：流名已经唯一，但两轮的流若共用 `roost.rpc.>` 主题，
	// 在持久化的本机环境里第二轮会被 JetStream 以 "subjects overlap" 拒绝。
	cfg.Set("nats.prefix", "roostit"+suffix)
	requestStream, responseStream := "ROOST_IT_RPC_REQ_"+suffix, "ROOST_IT_RPC_RESP_"+suffix
	cfg.Set("nats.rpc.request_stream", requestStream)
	cfg.Set("nats.rpc.response_stream", responseStream)
	// OPEN-ITEMS B45：两条流按轮次唯一命名，之前每跑一次就在共享 NATS 上留下两条。
	// 这条 Cleanup 先于 Mod 的 Stop 注册，所以在 Mod 停下、代理复位之后才执行。
	t.Cleanup(func() { deleteRPCStreams(t, natsURL, requestStream, responseStream) })
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

	proxy.heal(t)
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
