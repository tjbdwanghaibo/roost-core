package roost

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
)

// access.player.transports 含 websocket 时，生成的嵌入 TCP Mod（configFromViper）与独立 Gate Mod 把
// player_access.tcp.websocket.* / player_access.gate.websocket.* 透传进 gateway.TCPConfig.WebSocket，doctor 的
// config-schema 认识这些键；不含 websocket 时 server_gen.go 里没有任何 WebSocket 字样（逐字节不变的对照见
// docs/maintenance/evidence/lobby-relay-ws-20261117）。websocket 只能跟着 tcp 声明。
func TestPlayerWebSocketTransportPassesItsKeysThrough(t *testing.T) {
	plain := gateManifestNamed("gate", "game")
	webSocket := gateManifestNamed("gate", "game")
	access := webSocket.Access["player"]
	access.Transports = []string{"tcp", "websocket"}
	webSocket.Access["player"] = access

	if server := renderPlayerTCPServer(plain); strings.Contains(server, "WebSocket") || strings.Contains(server, "websocket") {
		t.Fatal("server_gen.go of a project without the websocket transport mentions WebSocket")
	}
	server := renderPlayerTCPServer(webSocket)
	for _, want := range []string{
		"\t\tWebSocket: gateway.WebSocketConfig{Addr: tcp.WebSocket.Addr, Path: tcp.WebSocket.Path, AllowedOrigins: tcp.WebSocket.AllowedOrigins, TrustedProxies: tcp.WebSocket.TrustedProxies},\n",
		" options.TCP.WebSocket=gateway.WebSocketConfig{Addr: settings.Gate.WebSocket.Addr, Path: settings.Gate.WebSocket.Path, AllowedOrigins: settings.Gate.WebSocket.AllowedOrigins, TrustedProxies: settings.Gate.WebSocket.TrustedProxies}\n",
		"`config:\"websocket\"`",
		// 启动前与 --check-config 按运行时同一条规则检查 websocket 段（trusted_proxies 的全零前缀等）。
		"func (c *tcpConfig) ValidateConfig(bool) error {\n\treturn gateway.WebSocketConfig{Addr: c.TCP.WebSocket.Addr, Path: c.TCP.WebSocket.Path, AllowedOrigins: c.TCP.WebSocket.AllowedOrigins, TrustedProxies: c.TCP.WebSocket.TrustedProxies}.Validate(\"player_access.tcp.websocket.\")\n}\n",
		"func (c *gateConfig) ValidateConfig(bool) error {\n\treturn gateway.WebSocketConfig{Addr: c.Gate.WebSocket.Addr, Path: c.Gate.WebSocket.Path, AllowedOrigins: c.Gate.WebSocket.AllowedOrigins, TrustedProxies: c.Gate.WebSocket.TrustedProxies}.Validate(\"player_access.gate.websocket.\")\n}\n",
	} {
		if !strings.Contains(server, want) {
			t.Fatalf("server_gen.go with the websocket transport does not contain %q", want)
		}
	}
	if strings.Count(server, "`config:\"websocket\"`") != 2 {
		t.Fatal("want one websocket section in tcpConfig and one in gateConfig")
	}

	for service, prefix := range map[string]string{"game": "player_access.tcp.websocket.", "gate": "player_access.gate.websocket."} {
		with, err := serviceConfigSchema(webSocket, service)
		if err != nil {
			t.Fatal(err)
		}
		without, err := serviceConfigSchema(plain, service)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"addr", "path", "allowed_origins", "trusted_proxies"} {
			if _, ok := with.Lookup(prefix + key); !ok {
				t.Errorf("%s with the websocket transport does not declare %s%s", service, prefix, key)
			}
			if _, ok := without.Lookup(prefix + key); ok {
				t.Errorf("%s without the websocket transport declares %s%s", service, prefix, key)
			}
		}
		// 不声明缺省值：gateway.DefaultTCPConfig 的 WebSocket 是零值（空路径取 /ws），声明的缺省要与它逐字段相同。
		// 也不是 starter 键：生成器不写 websocket 段，写上 addr 才开。
		for _, name := range []string{"path", "trusted_proxies"} {
			if key, _ := with.Lookup(prefix + name); key.Default != "" || key.Starter {
				t.Errorf("%s%s default = %q, starter = %v; want no default and not written by the generator", prefix, name, key.Default, key.Starter)
			}
		}
	}
	if _, ok := allFrameworkConfigSchema().Lookup("player_access.gate.websocket.addr"); !ok {
		t.Error("the union of framework declarations does not know the websocket keys")
	}

	onlyWebSocket := gateManifestNamed("gate", "game")
	access = onlyWebSocket.Access["player"]
	access.Transports, access.Gate = []string{"websocket"}, ""
	onlyWebSocket.Access["player"] = access
	if err := onlyWebSocket.Validate(); err == nil || !strings.Contains(err.Error(), "websocket needs tcp") {
		t.Fatalf("websocket without tcp: %v, want a refusal", err)
	}
}

// webSocketManifest 是 gateManifestNamed 加上 websocket 传输；embedded 为 true 时去掉 Gate（嵌入 TCP 的玩家服务）。
func webSocketManifest(embedded bool) Manifest {
	m := gateManifestNamed("gate", "game")
	access := m.Access["player"]
	access.Transports = []string{"tcp", "websocket"}
	if embedded {
		access.Gate = ""
		delete(m.Services, "gate")
	}
	m.Access["player"] = access
	return m
}

// 部署模板：声明了 websocket 时，监听玩家连接的那个服务（Gate，或嵌入 TCP 的玩家服务）发布约定的 7080（k8s 的
// player-ws 容器与 Service 端口、NetworkPolicy、生产 compose、docker 示例）；ROOST_YAML 与 PLAYER_ACCESS_TCP 文档写明
// websocket。没有声明的工程一个字都没有。
func TestPlayerWebSocketPortIsDeployed(t *testing.T) {
	port := strconv.Itoa(playerWebSocketPort)
	for _, tc := range []struct {
		name     string
		m        Manifest
		listener string
	}{{"gate", webSocketManifest(false), "gate"}, {"embedded", webSocketManifest(true), "game"}} {
		files := renderProductionDeployment(tc.m)
		workload := files["deploy/k8s/base/"+tc.listener+".yaml"]
		for _, want := range []string{
			"            - name: player-ws\n              containerPort: " + port + "\n              protocol: TCP\n",
			"    - {name: player-ws, port: " + port + ", targetPort: player-ws, protocol: TCP}\n",
		} {
			if !strings.Contains(workload, want) {
				t.Errorf("%s: %s.yaml lacks %q:\n%s", tc.name, tc.listener, want, workload)
			}
		}
		for path, body := range files {
			if path != "deploy/k8s/base/"+tc.listener+".yaml" && strings.HasPrefix(path, "deploy/k8s/base/") && strings.Contains(body, "player-ws") {
				t.Errorf("%s: %s publishes player-ws too", tc.name, path)
			}
		}
		if policy := files["deploy/k8s/base/network-policy.yaml"]; !strings.Contains(policy, "        - {protocol: TCP, port: 7000}\n        - {protocol: TCP, port: "+port+"}\n") {
			t.Errorf("%s: network-policy.yaml does not open 7000 and %s to the player-access namespaces:\n%s", tc.name, port, policy)
		}
		if compose := renderProductionCompose(tc.m); !strings.Contains(compose, "  "+tc.listener+":\n") || !strings.Contains(compose, "    ports:\n      - \"7000:7000\"\n      - \""+port+":"+port+"\"\n") || strings.Count(compose, "    ports:\n") != 1 {
			t.Errorf("%s: docker-compose.prod.yaml:\n%s", tc.name, compose)
		}
		if readme := files["deploy/docker/README.md"]; !strings.Contains(readme, "      -p 7000:7000 \\\n      -p "+port+":"+port+" \\\n") || !strings.Contains(readme, playerWebSocketAddrKey(tc.m)+": 0.0.0.0:"+port) {
			t.Errorf("%s: deploy/docker/README.md:\n%s", tc.name, readme)
		}
		if guide := renderManifestGuide(tc.m); !strings.Contains(guide, "支持 tcp 与 websocket") {
			t.Errorf("%s: ROOST_YAML does not say websocket is supported", tc.name)
		}
		if guide := renderPlayerWebSocketGuide(tc.m); !strings.Contains(guide, playerWebSocketAddrKey(tc.m)+": 0.0.0.0:"+port) {
			t.Errorf("%s: the WebSocket guide does not name %s:\n%s", tc.name, playerWebSocketAddrKey(tc.m), guide)
		}
		// 七层负载均衡后取真实 IP 的配置（GATEWAY.md §6“部署注意”）写进生成工程的接入说明。
		if guide, key := renderPlayerWebSocketGuide(tc.m), strings.TrimSuffix(playerWebSocketAddrKey(tc.m), "addr")+"trusted_proxies"; !strings.Contains(guide, key+" 写上负载均衡自己的地址") || !strings.Contains(guide, "0.0.0.0/0、::/0") {
			t.Errorf("%s: the WebSocket guide does not explain %s:\n%s", tc.name, key, guide)
		}
	}
	plain := gateManifestNamed("gate", "game")
	for path, body := range renderProductionDeployment(plain) {
		if strings.Contains(body, "player-ws") || strings.Contains(body, port) {
			t.Errorf("%s of a project without websocket mentions the WebSocket port", path)
		}
	}
	if guide := renderManifestGuide(plain); strings.Contains(guide, "websocket") || !strings.Contains(guide, "当前\n支持 tcp。") {
		t.Error("ROOST_YAML of a project without websocket changed")
	}
}

// listenDocument 是 listenAddrProblems 的一份配置（直接给键值，不读文件）。
func listenDocument(t *testing.T, m Manifest, service string, values map[string]any) serviceConfigDocument {
	t.Helper()
	schema, err := serviceConfigSchema(m, service)
	if err != nil {
		t.Fatal(err)
	}
	return serviceConfigDocument{service: service, rel: "config." + service + ".yaml", schema: schema, source: configschema.NewMapSource(values)}
}

// WebSocket 地址参与同一台机器上的端口冲突检查：Gate 的 websocket.addr 总是算、嵌入 TCP 的跟着 player_access.tcp.enabled；
// 没写地址（空串）的不参与——relay.ws_listen 这类“不写就不开”的监听键也是这样。
func TestWebSocketAddressesTakePartInThePortCheck(t *testing.T) {
	all := allFrameworkConfigSchema()
	gate := webSocketManifest(false)
	opsOn7080 := map[string]any{"ops": map[string]any{"enabled": true, "addr": "127.0.0.1:" + strconv.Itoa(playerWebSocketPort)}}
	gateWith := func(ws map[string]any) map[string]any {
		return map[string]any{"player_access": map[string]any{"gate": map[string]any{"addr": "0.0.0.0:7000", "websocket": ws}}}
	}
	problems := listenAddrProblems([]serviceConfigDocument{
		listenDocument(t, gate, "gate", gateWith(map[string]any{"addr": "0.0.0.0:" + strconv.Itoa(playerWebSocketPort)})),
		listenDocument(t, gate, "game", opsOn7080),
	}, all)
	if len(problems) != 1 || !strings.Contains(problems[0], "player_access.gate.websocket.addr") || !strings.Contains(problems[0], "ops.addr") {
		t.Errorf("the Gate's WebSocket port against another service's ops port: %q, want one collision naming both keys", problems)
	}
	for label, ws := range map[string]map[string]any{"not written": {}, "empty": {"addr": ""}} {
		if problems := listenAddrProblems([]serviceConfigDocument{
			listenDocument(t, gate, "gate", gateWith(ws)), listenDocument(t, gate, "game", opsOn7080),
		}, all); len(problems) != 0 {
			t.Errorf("Gate WebSocket address %s: %q, want no problem (it does not listen)", label, problems)
		}
	}

	embedded := webSocketManifest(true)
	tcpWith := func(enabled bool) map[string]any {
		return map[string]any{"player_access": map[string]any{"tcp": map[string]any{"enabled": enabled, "addr": "0.0.0.0:7000", "websocket": map[string]any{"addr": ":7000"}}}}
	}
	if problems := listenAddrProblems([]serviceConfigDocument{listenDocument(t, embedded, "game", tcpWith(true))}, all); len(problems) != 1 || !strings.Contains(problems[0], "player_access.tcp.websocket.addr") {
		t.Errorf("embedded TCP and its WebSocket on one port: %q, want a collision", problems)
	}
	if problems := listenAddrProblems([]serviceConfigDocument{listenDocument(t, embedded, "game", tcpWith(false))}, all); len(problems) != 0 {
		t.Errorf("player_access.tcp.enabled false: %q, want no listener checked", problems)
	}

	// 声明上“不写就不开”的监听键（relay.ws_listen 的写法：listen 总是、没有缺省值）：没写时不参与，写了照常比较。
	optional := configschema.Schema{Keys: []configschema.Key{{Name: "relay.ws_listen", Kind: configschema.KindString, Listen: "always"}}}
	relayDoc := func(values map[string]any) serviceConfigDocument {
		return serviceConfigDocument{service: "relay", rel: "config.relay.yaml", schema: optional, source: configschema.NewMapSource(values)}
	}
	if problems := listenAddrProblems([]serviceConfigDocument{relayDoc(map[string]any{}), listenDocument(t, gate, "game", opsOn7080)}, all); len(problems) != 0 {
		t.Errorf("relay.ws_listen not written: %q, want no problem", problems)
	}
	if problems := listenAddrProblems([]serviceConfigDocument{
		relayDoc(map[string]any{"relay": map[string]any{"ws_listen": "0.0.0.0:" + strconv.Itoa(playerWebSocketPort)}}), listenDocument(t, gate, "game", opsOn7080),
	}, all); len(problems) != 1 {
		t.Errorf("relay.ws_listen on the ops port: %q, want a collision", problems)
	}
}

// 没有声明 websocket 的工程写了 websocket 的键：roost config check 与 doctor 的 config-schema 都报失败并给出命令；
// 声明了 websocket 的工程照常（键有读取方）。
func TestWebSocketKeysWithoutTheTransportAreRefused(t *testing.T) {
	const keys = "player_access:\n  tcp:\n    websocket:\n      addr: 0.0.0.0:7080\n"
	plain := gateManifestNamed("gate", "game")
	access := plain.Access["player"]
	access.Gate = ""
	plain.Access["player"] = access
	delete(plain.Services, "gate")
	bad, err := checkConfigFileText([]byte(keys), false, plain, "game", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(bad, "\n"), "player_access.tcp.websocket.addr is a WebSocket key, but roost.yaml access.player.transports has no websocket") {
		t.Errorf("config check of a project without websocket: %q, want the WebSocket refusal", bad)
	}
	bad, err = checkConfigFileText([]byte(keys), false, webSocketManifest(true), "game", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(bad, "\n"), "is a WebSocket key") {
		t.Errorf("config check of a project with websocket: %q, want the keys accepted", bad)
	}

	root, m := newLobbyProject(t)
	const rel = "configs/service/config.lobby.yaml"
	original := readProjectFile(t, root, rel)
	writeProjectFile(t, root, rel, original+keys)
	var item CheckItem
	for _, found := range checkConfigDeclarations(root, m, nil) {
		if found.Name == "config-schema:lobby" {
			item = found
		}
	}
	if item.Status != StatusFail || !strings.Contains(item.Detail, filepath.ToSlash(rel)+": config: player_access.tcp.websocket.addr is a WebSocket key") || !strings.Contains(item.Detail, "roost add transport websocket") {
		t.Errorf("doctor config-schema:lobby = %s %s; want FAIL naming the WebSocket key and the command", item.Status, item.Detail)
	}
}

// 七层负载均衡后取真实 IP（GATEWAY.md §6“部署注意”）：可信代理列表写成 0.0.0.0/0 等于让任何客户端伪造地址。生成工程
// （独立 Gate + websocket）编译出的进程按运行时同一条规则检查：doctor 的 config-check 与 roost config check 在启动前
// 拒绝 Gate 的 player_access.gate.websocket.trusted_proxies 与玩家服务的 player_access.tcp.websocket.trusted_proxies，
// 并点名键；写成负载均衡自己的网段时通过。规则只在 wsconn 一处，生成器不 import 运行时，经生成的 ValidateConfig 交给进程。
func TestGeneratedProjectRefusesTrustedProxiesThatTrustEveryone(t *testing.T) {
	if testing.Short() {
		t.Skip("generates a project, installs modules and compiles it")
	}
	t.Parallel()
	root, roost, _ := moduleProject(t, "wsproxy")
	roost("module", "add", "login", "enter", "portal")
	roost("add", "transport", "websocket")
	roost("add", "gate")
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	process, cleanup := newProjectProcess(ctx, root)
	defer cleanup()
	checkItems := func() map[string]CheckItem {
		items := map[string]CheckItem{}
		for _, item := range checkConfigRules(root, m, process) {
			items[item.Name] = item
		}
		return items
	}
	for name, item := range checkItems() {
		if item.Status != StatusOK {
			t.Fatalf("fresh project %s: %s %s", name, item.Status, item.Detail)
		}
	}
	// setTrustedProxies 写 player_access.<section>.websocket.trusted_proxies（第一次在段名下插入 websocket 子段，之后改值）。
	setTrustedProxies := func(service, section, value string) {
		t.Helper()
		path := filepath.Join(root, "configs", "service", "config."+service+".yaml")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		const written = "\n    websocket:\n      trusted_proxies: "
		if i := strings.Index(text, written); i >= 0 {
			start := i + len(written)
			text = text[:start] + value + text[start+strings.Index(text[start:], "\n"):]
		} else {
			const anchor = "\nplayer_access:\n"
			at := strings.Index(text, anchor)
			if at < 0 {
				t.Fatalf("config.%s.yaml has no player_access section:\n%s", service, text)
			}
			sectionAt := strings.Index(text[at+len(anchor):], "  "+section+":\n")
			if sectionAt < 0 {
				t.Fatalf("config.%s.yaml has no player_access.%s section:\n%s", service, section, text)
			}
			insert := at + len(anchor) + sectionAt + len("  "+section+":\n")
			text = text[:insert] + "    websocket:\n      trusted_proxies: " + value + "\n" + text[insert:]
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	setTrustedProxies("gate", "gate", "10.0.0.0/8, 0.0.0.0/0")
	setTrustedProxies("game", "tcp", "::/0")
	items := checkItems()
	for service, key := range map[string]string{"gate": "player_access.gate.websocket.trusted_proxies", "game": "player_access.tcp.websocket.trusted_proxies"} {
		item := items["config-check:"+service]
		if item.Status != StatusFail || !strings.Contains(item.Detail, key) || !strings.Contains(item.Detail, "trusts every address") {
			t.Errorf("config-check:%s with a trusted proxy list that trusts everyone = %s %q; want a FAIL naming %s", service, item.Status, item.Detail, key)
		}
	}
	var out strings.Builder
	if err := Run([]string{"config", "check", "--service", "gate", "--root", root}, &out, &out); err == nil || !strings.Contains(out.String(), "player_access.gate.websocket.trusted_proxies") {
		t.Errorf("roost config check --service gate: %v\n%s\nwant a refusal naming player_access.gate.websocket.trusted_proxies", err, out.String())
	}

	setTrustedProxies("gate", "gate", "10.0.0.0/8, 192.168.1.10")
	setTrustedProxies("game", "tcp", "10.0.0.0/8")
	for name, item := range checkItems() {
		if item.Status != StatusOK {
			t.Errorf("load balancer networks as trusted proxies: %s = %s %s, want OK", name, item.Status, item.Detail)
		}
	}
}
