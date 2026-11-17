package roost

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
)

// newLobbyProject 是 roost project new <name> -template lobby 的工程（只有托管的 account、lobby、relay）。
func newLobbyProject(t *testing.T) (string, Manifest) {
	t.Helper()
	_, root, err := NewProject(NewOptions{Name: "party", Module: "example.com/party", Out: filepath.Join(t.TempDir(), "party"), Template: lobbyTemplateName})
	if err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, m
}

func lobbyRelayItem(t *testing.T, root string, m Manifest, name string) CheckItem {
	t.Helper()
	for _, item := range checkLobbyRelay(root, m) {
		if item.Name == name {
			return item
		}
	}
	t.Fatalf("doctor has no %s item", name)
	return CheckItem{}
}

// 不带 game 的工程：-template lobby 只托管 account、lobby、relay；bootstrap 按 wiring 的接线装它们（lobby 带 account 的
// ClientMod，relay 没有协作者文件），配置能起服——lobby.games 有一个游戏、account.games 有同一个游戏、account.services
// 列出 lobby 与 relay、两边 token_key_env 是同一个变量，run.sh 给开发密钥。
func TestLobbyTemplateHostsAccountLobbyAndRelayWithoutGame(t *testing.T) {
	root, m := newLobbyProject(t)
	if got := sortedServiceNames(m); strings.Join(got, ",") != "account,lobby,relay" {
		t.Fatalf("services = %v, want account, lobby, relay only", got)
	}
	for _, name := range sortedServiceNames(m) {
		if !m.isFrameworkService(name) {
			t.Fatalf("%s is a business service; the lobby template has none", name)
		}
	}
	bootstrap := readProjectFile(t, root, "internal/bootstrap/generated.go")
	for _, want := range []string{
		"a.RegisterServer(svclobby.ServiceName, svclobby.NewServer(),",
		"svcaccount.NewClientMod(),\n\t\tsvclobby.NewMod(serviceLobby.Tokens()))",
		"a.RegisterServer(svcrelay.ServiceName, svcrelay.NewServer(),\n\t\tkitnats.NewNatsMod(nil),\n\t\tkitetcd.NewEtcdMod(),\n\t\tsvcrelay.NewMod())",
	} {
		if !strings.Contains(bootstrap, want) {
			t.Errorf("bootstrap lacks %q:\n%s", want, bootstrap)
		}
	}
	if strings.Contains(bootstrap, "domainlobby") || strings.Contains(bootstrap, "domainrelay") || strings.Contains(bootstrap, "serviceRelay") {
		t.Errorf("bootstrap imports a package lobby / relay do not have (service ServiceType or the relay collaborators):\n%s", bootstrap)
	}
	if _, err := os.Stat(filepath.Join(root, "services", "relay", "collaborators.go")); !os.IsNotExist(err) {
		t.Errorf("relay takes no collaborators, but services/relay/collaborators.go exists (%v)", err)
	}
	if !strings.Contains(readProjectFile(t, root, "services/lobby/collaborators.go"), "func Tokens() lobby.GameTokenValidator { return nil }") {
		t.Error("lobby collaborators do not default Tokens() to the account service")
	}

	lobby := readProjectFile(t, root, "configs/service/config.lobby.yaml")
	relay := readProjectFile(t, root, "configs/service/config.relay.yaml")
	account := readProjectFile(t, root, "configs/service/config.account.yaml")
	for _, tc := range []struct{ file, body, want string }{
		{"lobby", lobby, "  listen: 0.0.0.0:" + strconv.Itoa(portsOf(m, "lobby").Web) + "\n"},
		{"lobby", lobby, "  games:\n    party:\n"},
		{"lobby", lobby, "      max_members: 8\n"},
		{"lobby", lobby, "  token_key_env: " + relayTokenKeyEnv + "\n"},
		{"lobby", lobby, "  subject: roost.party.lobby\n"},
		{"lobby", lobby, "  relay_registry_prefix: /party/relays/\n"},
		{"relay", relay, "  listen: 0.0.0.0:" + strconv.Itoa(7100+portsOf(m, "relay").Block) + "\n"},
		{"relay", relay, "  advertise_addr: 127.0.0.1:" + strconv.Itoa(7100+portsOf(m, "relay").Block) + "\n"},
		{"relay", relay, "  token_key_env: " + relayTokenKeyEnv + "\n"},
		{"relay", relay, "  subject: roost.party.lobby\n"},
		{"relay", relay, "  registry_prefix: /party/relays/\n"},
		{"account", account, "  games:\n    party:\n      token_ttl: 2h\n"},
		{"account", account, "  services:\n    lobby:\n      kind: lobby\n    relay:\n      kind: relay\n"},
	} {
		if !strings.Contains(tc.body, tc.want) {
			t.Errorf("config.%s.yaml lacks %q:\n%s", tc.file, tc.want, tc.body)
		}
	}
	if strings.Contains(relay, "instance:") {
		t.Error("the generated relay config fixes relay.instance; two sids sharing it would advertise the same instance")
	}
	if prod := readProjectFile(t, root, "configs/service/config.relay.prod.example.yaml"); !strings.Contains(prod, "  advertise_addr: CHANGE_ME:") {
		t.Errorf("the relay production example keeps a loopback advertise_addr:\n%s", prod)
	}
	if run := readProjectFile(t, root, "deploy/dev/run.sh"); !strings.Contains(run, relayTokenKeyEnv+"=\"${"+relayTokenKeyEnv+":-") || !strings.Contains(run, "export "+relayTokenKeyEnv) {
		t.Error("deploy/dev/run.sh does not give lobby and relay a development token key")
	}

	// 全部配置（开发、生产示例、k8s Secret 示例，含 roost 改过的 account）按各进程注册的 Mod 的声明检查。
	all := allFrameworkConfigSchema()
	for _, service := range sortedServiceNames(m) {
		schema, err := serviceConfigSchema(m, service)
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range serviceConfigFiles(service) {
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file.rel)))
			if err != nil {
				t.Fatal(err)
			}
			bad, unread, err := checkServiceConfigText(raw, file.secret, file.production, schema, configschema.Schema{}, all)
			if err != nil {
				t.Fatalf("%s: %v", file.rel, err)
			}
			for _, problem := range append(bad, unread...) {
				t.Errorf("%s: %s", file.rel, problem)
			}
		}
	}
	for _, item := range checkLobbyRelay(root, m) {
		if item.Status != StatusOK {
			t.Errorf("doctor %s = %s: %s", item.Name, item.Status, item.Detail)
		}
	}
}

// roost add lobby / relay 加到 game 工程上：已托管的 account 复用，account.services 列出玩家服务（kind game）与
// lobby、relay；再加一次同名的什么都不改；名字被业务服务占了时拒绝。
func TestAddLobbyAndRelayToAGameProject(t *testing.T) {
	_, root, err := NewProject(NewOptions{Name: "arena", Module: "example.com/arena", Out: filepath.Join(t.TempDir(), "arena"), Template: "game", Frameworks: []string{"account"}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	m.Access = map[string]AccessSpec{"player": {Service: "game"}}
	raw, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ManifestName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"lobby", "relay"} {
		if _, err := Add(root, AddOptions{Kind: kind, Warnings: io.Discard}); err != nil {
			t.Fatalf("add %s: %v", kind, err)
		}
	}
	m, err = LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if hostedServiceOf(m, "account") != "account" || len(servicesHosting(m, "account")) != 1 {
		t.Fatalf("add lobby declared a second account: %v", sortedServiceNames(m))
	}
	if !contains(m.Services["game"].Uses, "account") || len(m.Services["game"].Uses) != 1 {
		t.Errorf("game uses %v; lobby and relay are not bus services", m.Services["game"].Uses)
	}
	account := readProjectFile(t, root, "configs/service/config.account.yaml")
	for _, want := range []string{"    game:\n      kind: game\n", "    lobby:\n      kind: lobby\n", "    relay:\n      kind: relay\n", "  games:\n    arena:\n      token_ttl: 2h\n"} {
		if !strings.Contains(account, want) {
			t.Errorf("config.account.yaml lacks %q:\n%s", want, account)
		}
	}
	for _, item := range checkLobbyRelay(root, m) {
		if item.Status != StatusOK {
			t.Errorf("doctor %s = %s: %s", item.Name, item.Status, item.Detail)
		}
	}
	if paths, err := Add(root, AddOptions{Kind: "lobby", Warnings: io.Discard}); err != nil || len(paths) != 0 {
		t.Errorf("adding the same lobby again = %v, %v; want nothing changed", paths, err)
	}
	if _, err := Add(root, AddOptions{Kind: "relay", Name: "game", Warnings: io.Discard}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("add relay over the business service game = %v; want a refusal", err)
	}
}

// lobby / relay 不在总线上：业务服务不能 uses 它们，-framework 与模块安装器也不认；没托管 account 的 lobby 被拒。
func TestLobbyAndRelayAreNotBusServices(t *testing.T) {
	if names := strings.Join(frameworkServiceNames(), ","); strings.Contains(names, "lobby") || strings.Contains(names, "relay") {
		t.Fatalf("frameworkServiceNames (-framework all, game-demo) = %s; lobby and relay are not bus services", names)
	}
	for _, raw := range []string{"lobby", "relay", "rank,relay"} {
		if _, err := parseFrameworkSelection(raw); err == nil {
			t.Errorf("-framework %s accepted", raw)
		}
	}
	m := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"configdata"}, nil)
	m.Services["account"] = ServiceSpec{Framework: "account"}
	m.Services["lobby"] = ServiceSpec{Framework: "lobby"}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	game := m.Services["game"]
	game.Uses = []string{"lobby"}
	m.Services["game"] = game
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "no bus client") {
		t.Errorf("uses: [lobby] = %v; want a refusal", err)
	}
	game.Uses = nil
	m.Services["game"] = game
	delete(m.Services, "account")
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "calls the account service") {
		t.Errorf("lobby without a hosted account = %v; want a refusal", err)
	}
	if _, err := declareFrameworkService(&m, "game", "relay"); err == nil {
		t.Error("declareFrameworkService (module installer) accepted relay")
	}
	if err := validateModuleRuntime(&ModuleRuntime{Package: "services/lobby/x", Start: "Start", Framework: "lobby"}); err == nil {
		t.Error("a module runtime on the lobby was accepted; the lobby has no domain CapabilityName to host it on")
	}
}

// doctor 的 lobby-relay:account-services：漏了 relay、kind 写错都报 FAIL；生成的配置是 OK（上面两个用例）。
func TestDoctorLobbyRelayAccountServices(t *testing.T) {
	root, m := newLobbyProject(t)
	const rel = "configs/service/config.account.yaml"
	original := readProjectFile(t, root, rel)
	for _, tc := range []struct{ name, from, to, want string }{
		{"missing relay", "    relay:\n      kind: relay\n", "", "account.services has no relay"},
		{"wrong kind", "    lobby:\n      kind: lobby\n", "    lobby:\n      kind: game\n", `account.services.lobby.kind is "game", want lobby`},
	} {
		edited := strings.Replace(original, tc.from, tc.to, 1)
		if edited == original {
			t.Fatalf("%s: edit did not apply:\n%s", tc.name, original)
		}
		writeProjectFile(t, root, rel, edited)
		item := lobbyRelayItem(t, root, m, "lobby-relay:account-services")
		if item.Status != StatusFail || !strings.Contains(item.Detail, tc.want) {
			t.Errorf("%s: %s %s; want FAIL naming %q", tc.name, item.Status, item.Detail, tc.want)
		}
	}
	writeProjectFile(t, root, rel, original)
	if item := lobbyRelayItem(t, root, m, "lobby-relay:account-services"); item.Status != StatusOK {
		t.Errorf("restored config: %s %s", item.Status, item.Detail)
	}
}

// doctor 的 lobby-relay:token-key-env：relay 指向别的变量、lobby 的变量为空都报 FAIL。
func TestDoctorLobbyRelayTokenKeyEnv(t *testing.T) {
	root, m := newLobbyProject(t)
	for _, tc := range []struct{ name, rel, to, want string }{
		{"different variable", "configs/service/config.relay.prod.example.yaml", "  token_key_env: OTHER_KEY\n", "name different variables"},
		{"empty", "configs/service/config.lobby.yaml", "  token_key_env: \"\"\n", "lobby.token_key_env is empty"},
	} {
		original := readProjectFile(t, root, tc.rel)
		edited := strings.Replace(original, "  token_key_env: "+relayTokenKeyEnv+"\n", tc.to, 1)
		if edited == original {
			t.Fatalf("%s: edit did not apply", tc.name)
		}
		writeProjectFile(t, root, tc.rel, edited)
		item := lobbyRelayItem(t, root, m, "lobby-relay:token-key-env")
		if item.Status != StatusFail || !strings.Contains(item.Detail, tc.want) {
			t.Errorf("%s: %s %s; want FAIL naming %q", tc.name, item.Status, item.Detail, tc.want)
		}
		writeProjectFile(t, root, tc.rel, original)
	}
	if item := lobbyRelayItem(t, root, m, "lobby-relay:token-key-env"); item.Status != StatusOK {
		t.Errorf("restored configs: %s %s", item.Status, item.Detail)
	}
}

// doctor 的 lobby-relay:advertise-addr：生产示例写回环地址（127.x、localhost、::1）报 FAIL；开发配置写回环不管。
func TestDoctorLobbyRelayAdvertiseAddr(t *testing.T) {
	root, m := newLobbyProject(t)
	const rel = "configs/service/config.relay.prod.example.yaml"
	original := readProjectFile(t, root, rel)
	port := strconv.Itoa(7100 + portsOf(m, "relay").Block)
	for _, loopback := range []string{"127.0.0.1:" + port, "127.0.0.2:" + port, "localhost:" + port, "\"[::1]:" + port + "\""} {
		edited := strings.Replace(original, "  advertise_addr: CHANGE_ME:"+port+"\n", "  advertise_addr: "+loopback+"\n", 1)
		if edited == original {
			t.Fatalf("%s: edit did not apply:\n%s", loopback, original)
		}
		writeProjectFile(t, root, rel, edited)
		item := lobbyRelayItem(t, root, m, "lobby-relay:advertise-addr")
		if item.Status != StatusFail || !strings.Contains(item.Detail, "loopback") {
			t.Errorf("%s: %s %s; want FAIL", loopback, item.Status, item.Detail)
		}
	}
	writeProjectFile(t, root, rel, original)
	if item := lobbyRelayItem(t, root, m, "lobby-relay:advertise-addr"); item.Status != StatusOK {
		t.Errorf("generated production example: %s %s", item.Status, item.Detail)
	}
	if !strings.Contains(readProjectFile(t, root, "configs/service/config.relay.yaml"), "  advertise_addr: 127.0.0.1:") {
		t.Fatal("the dev relay config no longer advertises loopback; the check below proves nothing")
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:7100": true, "127.9.9.9:1": true, "localhost:7100": true, "LOCALHOST": true, "[::1]:7100": true,
		"10.0.0.5:7100": false, "CHANGE_ME:7100": false, "relay.example.com:7100": false, ":7100": false, "0.0.0.0:7100": false,
	} {
		if got := isLoopbackHost(addr); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", addr, got, want)
		}
	}
}

// 没有 lobby / relay 的工程：doctor 不出 lobby-relay:* 项，account.services 不要求（account 按单一 game 服务处理）。
func TestProjectsWithoutLobbyOrRelayHaveNoLobbyRelayChecks(t *testing.T) {
	m := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"configdata"}, nil)
	m.Services["account"] = ServiceSpec{Framework: "account"}
	m.Access = map[string]AccessSpec{"player": {Service: "game"}}
	if items := checkLobbyRelay(t.TempDir(), m); len(items) != 0 {
		t.Errorf("checkLobbyRelay = %v; want none", items)
	}
	if required := requiredAccountServices(m); len(required) != 0 {
		t.Errorf("requiredAccountServices = %v; want none", required)
	}
	if devRunRelayTokenKey(m) != "" || directServicesGuide(m) != "" {
		t.Error("a project without lobby or relay gets the relay token key / guide text")
	}
}

// doctor 的 lobby-relay:account-games：lobby.games 的游戏不在 account.games 里（开发配置、生产示例各自对照）报 FAIL；
// 生成的配置是 OK。
func TestDoctorLobbyRelayAccountGames(t *testing.T) {
	root, m := newLobbyProject(t)
	if item := lobbyRelayItem(t, root, m, "lobby-relay:account-games"); item.Status != StatusOK {
		t.Fatalf("generated configs: %s %s", item.Status, item.Detail)
	}
	for _, tc := range []struct{ name, rel, from, to, want string }{
		{"account dev config lost the game", "configs/service/config.account.yaml", "  games:\n    party:\n      token_ttl: 2h\n", "  games:\n    other:\n      token_ttl: 2h\n",
			"configs/service/config.lobby.yaml lobby.games has party, which configs/service/config.account.yaml account.games does not list"},
		{"account production example lost the game", "configs/service/config.account.prod.example.yaml", "  games:\n    party:\n      token_ttl: 2h\n", "  games:\n    other:\n      token_ttl: 2h\n",
			"configs/service/config.lobby.prod.example.yaml lobby.games has party, which configs/service/config.account.prod.example.yaml account.games does not list"},
		{"lobby serves a second game", "configs/service/config.lobby.yaml", "  games:\n    party:\n", "  games:\n    brawl:\n      max_members: 4\n    party:\n",
			"lobby.games has brawl, which configs/service/config.account.yaml account.games does not list"},
	} {
		original := readProjectFile(t, root, tc.rel)
		edited := strings.Replace(original, tc.from, tc.to, 1)
		if edited == original {
			t.Fatalf("%s: edit did not apply:\n%s", tc.name, original)
		}
		writeProjectFile(t, root, tc.rel, edited)
		item := lobbyRelayItem(t, root, m, "lobby-relay:account-games")
		if item.Status != StatusFail || !strings.Contains(item.Detail, tc.want) {
			t.Errorf("%s: %s %s; want FAIL naming %q", tc.name, item.Status, item.Detail, tc.want)
		}
		writeProjectFile(t, root, tc.rel, original)
	}
	if item := lobbyRelayItem(t, root, m, "lobby-relay:account-games"); item.Status != StatusOK {
		t.Errorf("restored configs: %s %s", item.Status, item.Detail)
	}
}

// roost add 补 account.services 时新条目按名字排进已有条目之间，不插到最前面：先加 relay 再加 lobby，
// 结果仍是 game、lobby、relay。
func TestAddLobbyRelayKeepsAccountServicesInNameOrder(t *testing.T) {
	_, root, err := NewProject(NewOptions{Name: "arena", Module: "example.com/arena", Out: filepath.Join(t.TempDir(), "arena"), Template: "game", Frameworks: []string{"account"}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	m.Access = map[string]AccessSpec{"player": {Service: "game"}}
	raw, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ManifestName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"relay", "lobby"} {
		if _, err := Add(root, AddOptions{Kind: kind, Warnings: io.Discard}); err != nil {
			t.Fatalf("add %s: %v", kind, err)
		}
	}
	const want = "  services:\n    game:\n      kind: game\n    lobby:\n      kind: lobby\n    relay:\n      kind: relay\n"
	for _, rel := range serviceConfigRels("account") {
		if body := readProjectFile(t, root, rel); !strings.Contains(body, want) {
			t.Errorf("%s: account.services is not game, lobby, relay in that order:\n%s", rel, body)
		}
	}
}

// ensureYAMLMapEntries：新条目落在名字顺序的位置（最前、中间带头注释的条目之前、最后），已有条目与注释原样。
func TestEnsureYAMLMapEntriesInsertsInNameOrder(t *testing.T) {
	text := "account:\n  services:\n    bravo:\n      kind: game\n    # delta 的说明\n    delta:\n      kind: lobby\n  key_prefix: x\n"
	got, err := ensureYAMLMapEntries(text, "account", "services", "# c", []yamlMapEntry{
		{Key: "alpha", Lines: []string{"kind: relay"}}, {Key: "charlie", Lines: []string{"kind: relay"}}, {Key: "echo", Lines: []string{"kind: relay"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "account:\n  services:\n    alpha:\n      kind: relay\n    bravo:\n      kind: game\n    charlie:\n      kind: relay\n    # delta 的说明\n    delta:\n      kind: lobby\n    echo:\n      kind: relay\n  key_prefix: x\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// 部署模板：托管 lobby / relay 的工程发布它们的客户端端口（k8s 容器与 Service 端口、NetworkPolicy、生产 compose），
// 把 relay 令牌密钥注入 lobby 与 relay（k8s 引用 Secret、compose 与 shell 用环境变量），变量名取配置里 token_key_env 的
// 实际值（改了之后 sync 跟着改）。没有 lobby / relay 的服务不受影响。
func TestLobbyRelayDeploymentPublishesClientPortsAndTheTokenKey(t *testing.T) {
	root, m := newLobbyProject(t)
	lobbyPort := portsOf(m, "lobby").Web
	relayTCP, relayWS := relayPorts(m, "relay")
	check := func(env string) {
		t.Helper()
		lobby := readProjectFile(t, root, "deploy/k8s/base/lobby.yaml")
		relay := readProjectFile(t, root, "deploy/k8s/base/relay.yaml")
		account := readProjectFile(t, root, "deploy/k8s/base/account.yaml")
		secretRef := "            - name: " + env + "\n              valueFrom:\n                secretKeyRef: {name: party-relay-token, key: " + env + "}\n"
		for _, tc := range []struct{ file, body, want string }{
			{"lobby.yaml", lobby, secretRef},
			{"lobby.yaml", lobby, "            - name: lobby-http\n              containerPort: " + strconv.Itoa(lobbyPort) + "\n              protocol: TCP\n"},
			{"lobby.yaml", lobby, "    - {name: lobby-http, port: " + strconv.Itoa(lobbyPort) + ", targetPort: lobby-http, protocol: TCP}\n"},
			{"relay.yaml", relay, secretRef},
			{"relay.yaml", relay, "            - name: relay-tcp\n              containerPort: " + strconv.Itoa(relayTCP) + "\n"},
			{"relay.yaml", relay, "            - name: relay-ws\n              containerPort: " + strconv.Itoa(relayWS) + "\n"},
			{"relay.yaml", relay, "    - {name: relay-ws, port: " + strconv.Itoa(relayWS) + ", targetPort: relay-ws, protocol: TCP}\n"},
		} {
			if !strings.Contains(tc.body, tc.want) {
				t.Errorf("%s lacks %q:\n%s", tc.file, tc.want, tc.body)
			}
		}
		if strings.Contains(account, "secretKeyRef") || strings.Contains(account, "lobby-http") {
			t.Errorf("account.yaml got the lobby / relay settings:\n%s", account)
		}
		policy := readProjectFile(t, root, "deploy/k8s/base/network-policy.yaml")
		for _, port := range []int{lobbyPort, relayTCP, relayWS} {
			if !strings.Contains(policy, "roost.tjbdwanghaibo.io/player-access: \"true\"") || !strings.Contains(policy, "        - {protocol: TCP, port: "+strconv.Itoa(port)+"}\n") {
				t.Errorf("network-policy.yaml does not open %d to the player-access namespaces:\n%s", port, policy)
			}
		}
		// Secret 示例只在创建时写一次（同其他 Secret 示例，归应用），改名之后不再重写。
		if secret := readProjectFile(t, root, "deploy/k8s/base/secret.relay-token.example.yaml"); !strings.Contains(secret, "  name: party-relay-token\n") || !strings.Contains(secret, "  "+relayTokenKeyEnv+": CHANGE_ME\n") {
			t.Errorf("secret.relay-token.example.yaml:\n%s", secret)
		}
		compose := readProjectFile(t, root, "deploy/docker/docker-compose.prod.yaml")
		for _, want := range []string{
			"  lobby:\n", "    environment:\n      " + env + ": ${" + env + ":?",
			"    ports:\n      - \"" + strconv.Itoa(lobbyPort) + ":" + strconv.Itoa(lobbyPort) + "\"\n",
			"    ports:\n      - \"" + strconv.Itoa(relayTCP) + ":" + strconv.Itoa(relayTCP) + "\"\n      - \"" + strconv.Itoa(relayWS) + ":" + strconv.Itoa(relayWS) + "\"\n",
		} {
			if !strings.Contains(compose, want) {
				t.Errorf("docker-compose.prod.yaml lacks %q:\n%s", want, compose)
			}
		}
		if strings.Count(compose, "    environment:\n") != 2 {
			t.Errorf("want the token key in the lobby and the relay only:\n%s", compose)
		}
		if example := readProjectFile(t, root, "deploy/docker/env.example"); !strings.Contains(example, "\n"+env+"=CHANGE_ME\n") {
			t.Errorf("env.example:\n%s", example)
		}
		install := readProjectFile(t, root, "deploy/shell/install.sh")
		for _, want := range []string{
			"case \"$SERVICE\" in\n  lobby|relay)\n    RELAY_TOKEN_KEY=${" + env + ":-}\n",
			"printf '%s=%s\\n' " + env + " \"$RELAY_TOKEN_KEY\" > \"$RELEASE_ROOT/relay-token.env\"",
			"WorkingDirectory=$APP_ROOT/current\nEnvironmentFile=-$APP_ROOT/current/relay-token.env\nExecStart=",
		} {
			if !strings.Contains(install, want) {
				t.Errorf("install.sh lacks %q", want)
			}
		}
		if run := readProjectFile(t, root, "deploy/dev/run.sh"); !strings.Contains(run, env+"=\"${"+env+":-") || !strings.Contains(run, "relay "+strconv.Itoa(relayWS)+" relay.ws_listen (WebSocket)") {
			t.Errorf("deploy/dev/run.sh does not name %s / the client ports", env)
		}
	}
	check(relayTokenKeyEnv)

	// 改了 token_key_env（全部 lobby、relay 配置），sync 之后部署模板跟着用新的变量名。
	for _, service := range []string{"lobby", "relay"} {
		for _, rel := range serviceConfigRels(service) {
			body := readProjectFile(t, root, rel)
			edited := strings.Replace(body, "  token_key_env: "+relayTokenKeyEnv+"\n", "  token_key_env: PARTY_RELAY_KEY\n", 1)
			if edited == body {
				t.Fatalf("%s: edit did not apply", rel)
			}
			writeProjectFile(t, root, rel, edited)
		}
	}
	syncProject(t, root)
	compose := readProjectFile(t, root, "deploy/docker/docker-compose.prod.yaml")
	if strings.Contains(compose, relayTokenKeyEnv) {
		t.Fatalf("after renaming token_key_env the compose still injects %s:\n%s", relayTokenKeyEnv, compose)
	}
	check("PARTY_RELAY_KEY")
}
