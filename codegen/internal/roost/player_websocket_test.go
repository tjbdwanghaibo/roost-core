package roost

import (
	"strings"
	"testing"
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
		"\t\tWebSocket: gateway.WebSocketConfig{Addr: tcp.WebSocket.Addr, Path: tcp.WebSocket.Path, AllowedOrigins: tcp.WebSocket.AllowedOrigins},\n",
		" options.TCP.WebSocket=gateway.WebSocketConfig{Addr:settings.Gate.WebSocket.Addr,Path:settings.Gate.WebSocket.Path,AllowedOrigins:settings.Gate.WebSocket.AllowedOrigins}\n",
		"`config:\"websocket\"`",
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
		for _, key := range []string{"addr", "path", "allowed_origins"} {
			if _, ok := with.Lookup(prefix + key); !ok {
				t.Errorf("%s with the websocket transport does not declare %s%s", service, prefix, key)
			}
			if _, ok := without.Lookup(prefix + key); ok {
				t.Errorf("%s without the websocket transport declares %s%s", service, prefix, key)
			}
		}
		// 不声明缺省值：gateway.DefaultTCPConfig 的 WebSocket 是零值（空路径取 /ws），声明的缺省要与它逐字段相同。
		if key, _ := with.Lookup(prefix + "path"); key.Default != "" {
			t.Errorf("%spath default = %q, want none (gateway.DefaultTCPConfig leaves it empty)", prefix, key.Default)
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
