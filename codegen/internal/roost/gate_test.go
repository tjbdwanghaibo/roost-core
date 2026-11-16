package roost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// roost add gate 在装了 login / enter / portal 的 game 工程上：声明 Gate 服务与 access.player.gate，玩家服务的
// bootstrap 走 Game Ingress（认证器取登录模块的 NewAuthenticator），Gate 服务装不自动登记的 etcd 与生成的 Gate Mod；
// 玩家服务配置关嵌入 TCP、写 player_access.gate 与 login.presence.addr，Gate 配置写监听 / 登记地址并打开单实例锁；
// k8s 的 7000 端口跟到 Gate 上。doctor 的 gate:* 全部通过；再跑一次什么都不改，换个名字被拒绝。
func TestAddGateTurnsEmbeddedTCPIntoAStandaloneGate(t *testing.T) {
	if testing.Short() {
		t.Skip("generates a project and installs modules")
	}
	t.Parallel()
	root, roost, try := moduleProject(t, "gated")
	if out, err := try("add", "gate"); err == nil || !strings.Contains(err.Error(), "no player TCP yet") {
		t.Fatalf("roost add gate before player TCP: err = %v, want a refusal naming the missing player TCP\n%s", err, out)
	}
	roost("module", "add", "login", "enter", "portal")
	out := roost("add", "gate")
	for _, want := range []string{"created/updated: configs/service/config.gate.yaml", "game now runs Game Ingress"} {
		if !strings.Contains(out, want) {
			t.Fatalf("roost add gate did not print %q:\n%s", want, out)
		}
	}
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Access["player"].Gate != "gate" || strings.Join(manifest.Services["gate"].Mods, ",") != "etcd,nats" ||
		strings.Join(manifest.Services["gate"].Uses, ",") != "account" || !contains(manifest.Services["game"].Mods, "etcd") {
		t.Fatalf("manifest after add gate: access %+v, gate %+v, game mods %v", manifest.Access["player"], manifest.Services["gate"], manifest.Services["game"].Mods)
	}
	read := func(rel string) string {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	bootstrap := read("internal/bootstrap/generated.go")
	for _, want := range []string{
		"return NewWithOptions(Options{GameIngress: gateIngressOptions()})",
		"options.Authenticator = accessplayertcp.NewAuthenticator()",
		"options.RegisterDiscovery = true",
		gateEtcdConstructor + ",\n\t\tkitnats.NewNatsMod(nil),\n\t\tsvcaccount.NewClientMod(),\n\t\t" + gateModConstructor + ")",
	} {
		if !strings.Contains(bootstrap, want) {
			t.Fatalf("bootstrap does not contain %q:\n%s", want, bootstrap)
		}
	}
	server := read("internal/access/player/tcp/server_gen.go")
	for _, want := range []string{"func NewGateMod() *GateMod", "type gateConfig struct", "type ingressGateConfig struct", "mod.options.Config.Namespace = gate.Gate.Namespace"} {
		if !strings.Contains(server, want) {
			t.Fatalf("server_gen.go does not contain %q", want)
		}
	}
	gameConfig, gateConfig := read("configs/service/config.game.yaml"), read("configs/service/config.gate.yaml")
	for _, want := range []string{"  gate:\n", "    namespace: gated.access\n", "    advertise_addr: 127.0.0.1:7000\n", "login:\n  presence:\n    addr: 127.0.0.1:7000\n"} {
		if !strings.Contains(gameConfig, want) {
			t.Fatalf("config.game.yaml does not contain %q:\n%s", want, gameConfig)
		}
	}
	for _, want := range []string{"    namespace: gated.access\n", "    addr: 0.0.0.0:7000\n", "    advertise_addr: 127.0.0.1:7000\n", "  enabled: true\n  key_prefix: /gated/singleton\n", "etcd:\n"} {
		if !strings.Contains(gateConfig, want) {
			t.Fatalf("config.gate.yaml does not contain %q:\n%s", want, gateConfig)
		}
	}
	if prod := read("configs/service/config.game.prod.example.yaml"); !strings.Contains(prod, "    advertise_addr: CHANGE_ME:7000\n") {
		t.Fatalf("config.game.prod.example.yaml has no production Gate address:\n%s", prod)
	}
	if game, gate := read("deploy/k8s/base/game.yaml"), read("deploy/k8s/base/gate.yaml"); strings.Contains(game, "player-tcp") || !strings.Contains(gate, "containerPort: 7000") {
		t.Fatalf("k8s player port: game has it = %v, gate has it = %v; want only the Gate", strings.Contains(game, "player-tcp"), strings.Contains(gate, "containerPort: 7000"))
	}
	for _, item := range checkPlayerGate(root, manifest) {
		if item.Status != StatusOK {
			t.Fatalf("doctor %s after roost add gate: %s %s", item.Name, item.Status, item.Detail)
		}
	}

	before := moduleProjectDigest(t, root)
	if out := roost("add", "gate"); !strings.Contains(out, "nothing changed") {
		t.Fatalf("second roost add gate printed:\n%s", out)
	}
	if out, err := try("add", "gate", "edge"); err == nil || !strings.Contains(err.Error(), `access.player.gate is already "gate"`) {
		t.Fatalf("roost add gate edge on a project with a Gate: err = %v\n%s", err, out)
	}
	if after := moduleProjectDigest(t, root); after != before {
		t.Fatal("repeating roost add gate changed the project")
	}
	roost("project", "sync", "--skip-deps")
	if after := moduleProjectDigest(t, root); after != before {
		t.Fatal("project sync after roost add gate changed the project")
	}
}

// 没有 account（没装登录模块）的玩家 TCP 工程：Gate 拿不到校验票据的 account，命令拒绝且不改工程。
func TestAddGateRefusesWithoutAccount(t *testing.T) {
	if testing.Short() {
		t.Skip("generates a project")
	}
	t.Parallel()
	root, roost, try := moduleProject(t, "gatenoacct")
	roost("add", "access", "player", "--service", "game")
	roost("add", "transport", "tcp")
	before := moduleProjectDigest(t, root)
	out, err := try("add", "gate")
	if err == nil || !strings.Contains(err.Error(), "roost module add login") {
		t.Fatalf("roost add gate without account: err = %v, want a refusal pointing at the login module\n%s", err, out)
	}
	if moduleProjectDigest(t, root) != before {
		t.Fatal("a refused roost add gate changed the project")
	}
}

// gateManifest 是 doctor 与校验用的最小 Gate 工程声明：game 承载玩家（tcp），gate 是 Gate，account 托管。
func gateManifest() Manifest {
	m := DefaultManifest("gt", "example.com/gt", []string{"game"}, []string{"configdata", "etcd", "redis", "mongo", "nats", "dataengine", "nest"}, nil)
	m.Services["account"] = ServiceSpec{Framework: "account"}
	game := m.Services["game"]
	game.Uses = []string{"account"}
	m.Services["game"] = game
	m.Services["gate"] = ServiceSpec{Mods: []string{"etcd", "nats"}, Uses: []string{"account"}}
	m.Access = map[string]AccessSpec{"player": {Service: "game", Transports: []string{"tcp"}, Gate: "gate"}}
	m.Modules = map[string]InstalledModule{"login": {Service: "game"}}
	return m
}

func TestPlayerGateManifestRules(t *testing.T) {
	if err := gateManifest().validatePlayerGate(); err != nil {
		t.Fatalf("valid Gate manifest: %v", err)
	}
	for _, tc := range []struct {
		name, want string
		edit       func(*Manifest)
	}{
		{"unknown service", `unknown service "gw"`, func(m *Manifest) { a := m.Access["player"]; a.Gate = "gw"; m.Access["player"] = a }},
		{"gate is the player service", "is the player service itself", func(m *Manifest) { a := m.Access["player"]; a.Gate = "game"; m.Access["player"] = a }},
		{"gate hosts a framework service", "hosts the framework service", func(m *Manifest) { a := m.Access["player"]; a.Gate = "account"; m.Access["player"] = a }},
		{"no tcp", "needs the tcp transport", func(m *Manifest) { a := m.Access["player"]; a.Transports = nil; m.Access["player"] = a }},
		{"gate without etcd", "needs the etcd mod", func(m *Manifest) { m.Services["gate"] = ServiceSpec{Mods: []string{"nats"}, Uses: []string{"account"}} }},
		{"gate runs nest", "must not run nest", func(m *Manifest) {
			m.Services["gate"] = ServiceSpec{Mods: []string{"etcd", "nats", "nest"}, Uses: []string{"account"}}
		}},
		{"gate without account", "must list account under uses", func(m *Manifest) { m.Services["gate"] = ServiceSpec{Mods: []string{"etcd", "nats"}} }},
		{"player without etcd", `"game" needs the etcd mod`, func(m *Manifest) {
			g := m.Services["game"]
			g.Mods = []string{"configdata", "redis", "mongo", "nats", "dataengine", "nest"}
			m.Services["game"] = g
		}},
		{"no account hosted", "needs a hosted account service", func(m *Manifest) {
			delete(m.Services, "account")
			g := m.Services["game"]
			g.Uses = nil
			m.Services["game"] = g
			m.Services["gate"] = ServiceSpec{Mods: []string{"etcd", "nats"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := gateManifest()
			tc.edit(&m)
			err := m.validatePlayerGate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// gateDoctorProject 写出 doctor gate:* 读的文件：两端开发配置、account 配置与生成的 bootstrap（形态与 roost add gate 的产物相同）。
func gateDoctorProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	common := "etcd:\n  endpoints: [127.0.0.1:2379]\nsingleton:\n  enabled: true\n"
	write("configs/service/config.game.yaml", "sid: 1000\n"+common+"player_access:\n  gate:\n    namespace: gt.access\n    advertise_addr: 127.0.0.1:7000\n  tcp:\n    enabled: false\nlogin:\n  presence:\n    addr: 127.0.0.1:7000\n")
	write("configs/service/config.gate.yaml", "sid: 1000\n"+common+"player_access:\n  gate:\n    namespace: gt.access\n    addr: 0.0.0.0:7000\n    advertise_addr: 127.0.0.1:7000\n")
	write("configs/service/config.account.yaml", "sid: 1000\nportal:\n  servers:\n    \"1000\": 127.0.0.1:7000\n")
	write("internal/bootstrap/generated.go", "func New() (*app.App, error) { return NewWithOptions(Options{GameIngress: gateIngressOptions()}) }\n\t\t"+gateModConstructor+")\n")
	return root
}

func gateDoctorStatus(t *testing.T, root string, m Manifest) map[string]CheckItem {
	t.Helper()
	items := map[string]CheckItem{}
	for _, item := range checkPlayerGate(root, m) {
		items[item.Name] = item
	}
	return items
}

// 每项 gate:* 检查先在符合要求的工程上通过（绿），再改坏它查的那一处，只有它变成 FAIL（红）。
func TestDoctorPlayerGateChecks(t *testing.T) {
	green := gateDoctorStatus(t, gateDoctorProject(t), gateManifest())
	for _, name := range []string{"gate:ingress", "gate:player-tcp", "gate:namespace", "gate:etcd-singleton", "gate:client-address"} {
		if green[name].Status != StatusOK {
			t.Fatalf("%s on a correct Gate project: %+v", name, green[name])
		}
	}
	replace := func(t *testing.T, root, rel, old, new string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), old) {
			t.Fatalf("%s has no %q", rel, old)
		}
		if err := os.WriteFile(path, []byte(strings.Replace(string(body), old, new, 1)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		check, rel, old, new, want string
	}{
		{"gate:ingress", "internal/bootstrap/generated.go", "GameIngress: gateIngressOptions()", "", "does not run game behind Game Ingress"},
		{"gate:player-tcp", "configs/service/config.game.yaml", "    enabled: false\n", "    enabled: true\n", "player_access.tcp.enabled is true"},
		{"gate:namespace", "configs/service/config.gate.yaml", "namespace: gt.access", "namespace: other.access", `gate has "other.access", game has "gt.access"`},
		{"gate:namespace", "configs/service/config.game.yaml", "    namespace: gt.access\n", "", `game has "roost.access"`},
		{"gate:etcd-singleton", "configs/service/config.game.yaml", "  enabled: true\n", "  enabled: false\n", "game: singleton.enabled: true"},
		{"gate:etcd-singleton", "configs/service/config.gate.yaml", "etcd:\n  endpoints: [127.0.0.1:2379]\n", "", "gate: etcd.endpoints"},
		{"gate:etcd-singleton", "configs/service/config.gate.yaml", "  endpoints: [127.0.0.1:2379]\n", "  endpoints: [127.0.0.1:2379]\n  service_prefix: /other/services/\n", "the same etcd.service_prefix"},
		{"gate:client-address", "configs/service/config.game.yaml", "presence:\n    addr: 127.0.0.1:7000\n", "presence:\n    addr: 127.0.0.1:7999\n", "login.presence.addr 127.0.0.1:7999"},
		{"gate:client-address", "configs/service/config.game.yaml", "    advertise_addr: 127.0.0.1:7000\n", "    advertise_addr: 127.0.0.1:7998\n", "player_access.gate.advertise_addr 127.0.0.1:7998"},
		{"gate:client-address", "configs/service/config.account.yaml", "127.0.0.1:7000", "127.0.0.1:7001", "portal.servers.1000 127.0.0.1:7001"},
		{"gate:client-address", "configs/service/config.gate.yaml", "    advertise_addr: 127.0.0.1:7000\n", "", "advertise_addr is empty"},
	} {
		t.Run(tc.check+"/"+tc.want, func(t *testing.T) {
			root := gateDoctorProject(t)
			replace(t, root, tc.rel, tc.old, tc.new)
			items := gateDoctorStatus(t, root, gateManifest())
			if item := items[tc.check]; item.Status != StatusFail || !strings.Contains(item.Detail, tc.want) {
				t.Fatalf("%s after the edit: %+v, want FAIL mentioning %q", tc.check, item, tc.want)
			}
			for name, item := range items {
				if name != tc.check && item.Status != StatusOK {
					t.Fatalf("editing %s also failed %s: %+v", tc.rel, name, item)
				}
			}
		})
	}
	// 没声明 Gate：没有 gate:* 项；但服务配置里留着 player_access.gate 时报 FAIL（没有进程读它）。
	plain := gateManifest()
	access := plain.Access["player"]
	access.Gate = ""
	plain.Access["player"] = access
	delete(plain.Services, "gate")
	root := gateDoctorProject(t)
	if item := gateDoctorStatus(t, root, plain)["gate:ingress"]; item.Status != StatusFail || !strings.Contains(item.Detail, "declares no Gate") {
		t.Fatalf("stray player_access.gate without a Gate: %+v", item)
	}
	replace(t, root, "configs/service/config.game.yaml", "  gate:\n    namespace: gt.access\n    advertise_addr: 127.0.0.1:7000\n", "")
	if items := gateDoctorStatus(t, root, plain); len(items) != 0 {
		t.Fatalf("a project without a Gate got gate checks: %+v", items)
	}
}
