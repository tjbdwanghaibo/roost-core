package roost

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAddEntityScaffoldsTheOtherCategoryInsteadOfMintingOne(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{
		Name: "planet", Module: "example.com/planet", Out: target,
		Mods: []string{"configdata"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "entity", Name: "Player"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "entity", Name: "Guild"}); err != nil {
		t.Fatal(err)
	}
	// v1.47.0 起 lifecycle 不再由 add lifecycle 写脚手架，而是 generate 生成 lifecycle_gen.go（只用 EntityKind 常量），
	// 原来这里查“脚手架引用已删除的分类常量”的一段随之删除。

	for _, name := range []string{"player", "guild"} {
		raw, err := os.ReadFile(filepath.Join(root, "game", "entities", name, "entity.go"))
		if err != nil {
			t.Fatal(err)
		}
		body := string(raw)
		if !strings.Contains(body, "category=entity.EntityCategoryOther") {
			t.Errorf("%s does not declare its category on the marker:\n%s", name, body)
		}
		if strings.Contains(body, "MustRegisterEntityKindCategory") {
			t.Errorf("%s still hand-registers its category, which reintroduces the ordering trap:\n%s", name, body)
		}
		if strings.Contains(body, "entity.EntityCategory = 1") {
			t.Errorf("%s still mints its own category constant, which claims the remote lock rank:\n%s", name, body)
		}
		if strings.Contains(body, "EntityCategory"+strings.ToUpper(name[:1])+name[1:]) {
			t.Errorf("%s still declares a per-entity category constant:\n%s", name, body)
		}
	}
}

func TestAddModAppendsItsConfigSectionToExistingServiceConfigs(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "saga")
	read := func(rel string) string {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	if cfg := read("configs/service/config.game.yaml"); strings.Contains(cfg, "\nredis:\n") || strings.Contains(cfg, "\nsaga:\n") {
		t.Fatalf("precondition: a configdata-only project already has redis/saga config:\n%s", cfg)
	}
	if _, err := Add(root, AddOptions{Kind: "mod", Name: "redis", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "saga", Name: "AllianceRally", Service: "game", Steps: []string{"Reserve", "March"}}); err != nil {
		t.Fatal(err)
	}
	dev := read("configs/service/config.game.yaml")
	for _, want := range []string{"\nredis:\n", "\nsaga:\n", "stream_max_bytes: 8589934592", "\nmongo:\n", "\nnats:\n"} {
		if !strings.Contains(dev, want) {
			t.Errorf("dev config lacks %q after add mod/saga:\n%s", want, dev)
		}
	}
	if strings.Count(dev, "\nsaga:\n") != 1 || strings.Count(dev, "\nmongo:\n") != 1 {
		t.Errorf("a section was appended twice:\n%s", dev)
	}
	prod := read("configs/service/config.game.prod.example.yaml")
	if !strings.Contains(prod, "\nsaga:\n") || !strings.Contains(prod, "\nredis:\n") {
		t.Errorf("production example lacks the added sections:\n%s", prod)
	}
	if strings.Contains(prod, "127.0.0.1") || !strings.Contains(prod, "CHANGE_ME") {
		t.Errorf("production example appended sections keep loopback addresses instead of CHANGE_ME:\n%s", prod)
	}
	// Adding the same mod again changes nothing.
	before := dev
	if _, err := Add(root, AddOptions{Kind: "mod", Name: "redis", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	if after := read("configs/service/config.game.yaml"); after != before {
		t.Errorf("re-adding a mod rewrote the config:\n%s", after)
	}
}

// add access 给服务补上 nest，nest 又带入 dataengine / mongo / nats；add rpc 让服务挂上总线（nats）。
// 这些依赖带进来的 Mod 与 add mod 显式加的一样要在服务配置里有配置段，否则进程按看不见的默认地址连接
// （新用户试用第三轮：连到本机任意一个 27017）。
func TestModsBroughtInByAddAccessAndAddRPCGetTheirConfigSections(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "configdata")
	read := func(rel string) string {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	if cfg := read("configs/service/config.game.yaml"); strings.Contains(cfg, "\nmongo:\n") || strings.Contains(cfg, "\nnest:\n") {
		t.Fatalf("precondition: a configdata-only project already has mongo/nest config:\n%s", cfg)
	}
	paths, err := Add(root, AddOptions{Kind: "access", Name: "player", Service: "game"})
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"configs/service/config.game.yaml", "configs/service/config.game.prod.example.yaml"} {
		cfg := read(rel)
		// 带 DataEngine 的服务每个 sid 一个写者：与 project new 直接选 nest 一样打开单实例锁，并补锁用的 redis: 段。
		for _, want := range []string{"\nnest:\n", "\ndataengine:\n", "\nmongo:\n", "\nnats:\n", "\nredis:\n"} {
			if strings.Count(cfg, want) != 1 {
				t.Errorf("%s has %d %q blocks after add access, want 1:\n%s", rel, strings.Count(cfg, want), want, cfg)
			}
		}
		singletonOn := false
		for _, block := range topLevelConfigBlocks(cfg) {
			if block.key == "singleton" {
				singletonOn = strings.Contains(block.text, "\n  enabled: true\n")
			}
		}
		if !singletonOn {
			t.Errorf("%s: the Data Engine service's singleton lock was not turned on:\n%s", rel, cfg)
		}
		if !slices.Contains(paths, rel) {
			t.Errorf("add access did not report %s among the files it changed: %v", rel, paths)
		}
	}
	if prod := read("configs/service/config.game.prod.example.yaml"); strings.Contains(prod, "127.0.0.1") {
		t.Errorf("production example appended sections keep loopback addresses:\n%s", prod)
	}

	rpcRoot := copyOfNewProject(t, "configdata")
	if _, err := Add(rpcRoot, AddOptions{Kind: "rpc", Name: "Guild", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(rpcRoot, "configs", "service", "config.game.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "\nnats:\n") != 1 {
		t.Errorf("the rpc owner runs on the bus but its config has no nats: block:\n%s", raw)
	}
}

func TestAddRPCScaffoldsOwnerAndWiresCaller(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{
		Name: "planet", Module: "example.com/planet", Out: target,
		Services: []string{"game", "gate"}, Mods: []string{"configdata"}, Features: []string{"protocol", "errcode"},
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := Add(root, AddOptions{Kind: "rpc", Name: "Guild", Service: "game"})
	if err != nil {
		t.Fatalf("add rpc: %v", err)
	}
	read := func(rel string) string {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		return string(body)
	}
	for _, rel := range []string{"internal/rpc/guild/guild.go", "internal/rpc/guild/service.go", "internal/rpc/guild/mod.go",
		"internal/rpc/guild/server_run.go", "internal/rpc/guild/guild_rpc_gen.go", "internal/rpc/guild/guild_rpc_assembly_gen.go"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("add rpc did not write %s (created: %v)", rel, created)
		}
	}
	iface := read("internal/rpc/guild/guild.go")
	if !strings.Contains(iface, "//roost:rpc service_type=guild capability=rpc.guild") || !strings.Contains(iface, "errcode.Define(100000,") {
		t.Errorf("interface file lacks the marker or an errcode from the manifest space:\n%s", iface)
	}
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(m.Services["game"].Rpcs, "guild") || !contains(m.Features, "rpc") {
		t.Fatalf("manifest not updated: rpcs=%v features=%v", m.Services["game"].Rpcs, m.Features)
	}
	bootstrap := read("internal/bootstrap/generated.go")
	if !strings.Contains(bootstrap, "rpcGuild.NewMod(rpcGuild.New())") {
		t.Errorf("bootstrap does not assemble the owner Mod into game:\n%s", bootstrap)
	}
	// The owner registers handlers on the bus, so the nats mod rides along
	// (an effective dependency, like a framework ClientMod's).
	if !strings.Contains(bootstrap, "kitnats.NewNatsMod(") {
		t.Errorf("bootstrap does not assemble the nats mod for the rpc owner:\n%s", bootstrap)
	}
	if !strings.Contains(read("internal/rpc/guild/guild.go"), "func Error(err error) (int32, string)") {
		t.Error("the interface file lacks the Error mapper the generated handlers call")
	}
	// A second owner is refused; so is a hosted framework service owning one.
	if _, err := Add(root, AddOptions{Kind: "rpc", Name: "Guild", Service: "gate"}); err == nil {
		t.Fatal("the same rpc was accepted under a second owner")
	}

	// The caller side is a manifest edit, committed through sync.
	gate := m.Services["gate"]
	gate.UsesRpcs = []string{"guild"}
	m.Services["gate"] = gate
	raw, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ManifestName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncProject(root); err != nil {
		t.Fatalf("sync with uses_rpcs: %v", err)
	}
	bootstrap = read("internal/bootstrap/generated.go")
	if !strings.Contains(bootstrap, "rpcGuild.NewClientMod()") {
		t.Errorf("bootstrap does not assemble the ClientMod into gate:\n%s", bootstrap)
	}
	// uses_rpcs of an rpc nobody owns, or of one's own rpc, is refused.
	bad := m
	bad.Services["gate"] = ServiceSpec{Mods: gate.Mods, UsesRpcs: []string{"ledger"}}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "no service owns") {
		t.Fatalf("dangling uses_rpcs accepted: %v", err)
	}
	self := m
	self.Services["game"] = ServiceSpec{Mods: m.Services["game"].Mods, Rpcs: []string{"guild"}, UsesRpcs: []string{"guild"}}
	if err := self.Validate(); err == nil || !strings.Contains(err.Error(), "both owns and uses") {
		t.Fatalf("self-use accepted: %v", err)
	}
	// generate --check sees the two halves as current.
	if err := Generate(root, GenerateOptions{Check: true}); err != nil {
		t.Fatalf("generate --check after add rpc: %v", err)
	}
}

// Add 按种类的每条参数检查都单独生效，被拒的 Add 让 roost.yaml 逐字节不变。
func TestAddRefusesEachInvalidKindParameter(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{
		Name: "planet", Module: "example.com/planet", Out: target,
		Services: []string{"game", "gate"}, Mods: []string{"configdata"}, Features: []string{"protocol"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "access", Name: "player", Service: "game"}); err != nil {
		t.Fatalf("seed access player: %v", err)
	}
	if _, err := Add(root, AddOptions{Kind: "transport", Name: "tcp", Service: "game"}); err != nil {
		t.Fatalf("seed transport tcp: %v", err)
	}
	manifestPath := filepath.Join(root, ManifestName)
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		options AddOptions
		want    string
	}{
		{"invalid name", AddOptions{Kind: "service", Name: "9 bad!"}, `invalid service name "9 bad!"`},
		{"service exists", AddOptions{Kind: "service", Name: "game"}, "service game already exists"},
		{"mod unknown service", AddOptions{Kind: "mod", Name: "nest", Service: "lobby"}, `unknown service "lobby"`},
		{"mod unknown wiring mod", AddOptions{Kind: "mod", Name: "teleport", Service: "game"}, `unknown wiring mod "teleport"`},
		{"access unsupported", AddOptions{Kind: "access", Name: "admin", Service: "game"}, `unsupported access layer "admin"`},
		{"access ambiguous service", AddOptions{Kind: "access", Name: "player"}, "requires --service when the project has multiple services"},
		{"access unknown service", AddOptions{Kind: "access", Name: "player", Service: "lobby"}, `unknown service "lobby"`},
		{"access exists", AddOptions{Kind: "access", Name: "player", Service: "gate"}, "access layer player already exists"},
		{"transport unsupported", AddOptions{Kind: "transport", Name: "udp"}, `unsupported player transport "udp"`},
		{"transport wrong service", AddOptions{Kind: "transport", Name: "tcp", Service: "gate"}, `player access belongs to service "game", not "gate"`},
		{"transport exists", AddOptions{Kind: "transport", Name: "tcp", Service: "game"}, "player transport tcp already exists"},
		{"saga ambiguous service", AddOptions{Kind: "saga", Name: "trade", Steps: []string{"reserve", "commit"}}, "saga requires -service when the project has multiple services"},
		{"saga unknown service", AddOptions{Kind: "saga", Name: "trade", Service: "lobby", Steps: []string{"reserve", "commit"}}, `unknown service "lobby"`},
		{"protocol invalid handler", AddOptions{Kind: "protocol", Name: "PlayerLogin", Group: "game", Handler: "9 bad!"}, `invalid protocol handler "9 bad!"`},
		{"protocol invalid group", AddOptions{Kind: "protocol", Name: "PlayerLogin", Group: "9 bad!"}, `invalid protocol group`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paths, err := Add(root, tc.options)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Add(%+v) = %v, %v; want error containing %q", tc.options, paths, err, tc.want)
			}
			after, readErr := os.ReadFile(manifestPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("refused Add changed %s:\nbefore:\n%s\nafter:\n%s", ManifestName, before, after)
			}
		})
	}
}

// The handler kind is refused when the project never enabled the nest feature.
func TestAddHandlerRequiresNestFeature(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{
		Name: "planet", Module: "example.com/planet", Out: target,
		Mods: []string{"configdata"}, Features: []string{"protocol", "entity"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Add(root, AddOptions{Kind: "handler", Name: "Login", Entity: "Player", Protocol: "PlayerLogin"})
	if err == nil || !strings.Contains(err.Error(), "handler requires the nest feature") {
		t.Fatalf("handler without nest feature error = %v", err)
	}
}

func TestControllerScaffoldTransactionalGeneration(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a generated player-access project")
	}
	t.Parallel()
	root := copyOfNewProject(t, "configdata")
	for _, step := range []AddOptions{{Kind: "access", Name: "player", Service: "game"}, {Kind: "protocol", Name: "Hello", Group: "game", Handler: "player"}} {
		if _, err := Add(root, step); err != nil {
			t.Fatal(err)
		}
	}
	coreRoot, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %v: %v\n%s", args, err, out)
		}
	}
	run("mod", "edit", "-replace", generatedCoreModule+"="+coreRoot)
	generate := func() {
		t.Helper()
		if err := GenerateTransactional(ctx, root, GenerateOptions{Stdout: io.Discard}, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	generate()
	path := filepath.Join(root, "game/controllers/player/hello.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "HandleHello(ctx *playeraccess.Context, req *pb.HelloRequest)") {
		t.Fatalf("missing typed controller: %s", raw)
	}
	// 模拟实际业务响应，真实编译生成的注册链与 Controller 类型。
	business := strings.ReplaceAll(string(raw), `"fmt"`, "")
	business = strings.ReplaceAll(business, `return nil, fmt.Errorf("HandleHello: not implemented")`, `return &pb.HelloResponse{}, nil`)
	if err := os.WriteFile(path, []byte(business), 0600); err != nil {
		t.Fatal(err)
	}
	generate()
	after, _ := os.ReadFile(path)
	if string(after) != business {
		t.Fatal("business overwritten")
	}
	run("build", "./...")
	// 手动回包不再编码一次的断言随 playeraccess 搬进了 core（infra/network/playeraccess 的单元测试）。
	if err := Generate(root, GenerateOptions{Check: true}); err != nil {
		t.Fatal(err)
	}
	// 注册生成物仍存在时，缺失业务骨架也必须被 --check 识别。
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Check: true}); err == nil || !strings.Contains(err.Error(), "hello.go") {
		t.Fatalf("missing scaffold not detected: %v", err)
	}
	generate()
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

// add rpc -entity-service（联盟服务方案 §2.3 / §2.10 第 4 项）：写方法带 RequestID，Result 按 RequestID 查回执，
// 接收端有自己的处理预算，答复分成成功 / 业务拒绝 / 未执行可重试 / 结果未知；owner Mod 依赖 nest，
// Nest（与它之前的 DataEngine 恢复）启动之后才在总线上接请求。拥有者没有 Nest 时拒绝。生成工程能编译。
func TestAddRPCEntityServiceScaffoldsTheWriteContract(t *testing.T) {
	t.Parallel()
	_, root, err := NewProject(NewOptions{
		Name: "planet", Module: "example.com/planet", Out: filepath.Join(t.TempDir(), "planet"),
		Services: []string{"game", "alliance"}, Mods: []string{"configdata"}, Features: []string{"entity", "dao", "nest", "protocol", "errcode"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "rpc", Name: "Alliance", Service: "alliance", EntityService: true}); err == nil || !strings.Contains(err.Error(), "roost add mod nest -service alliance") {
		t.Fatalf("an entity-service rpc owned by a service without Nest must be refused with the fix, got %v", err)
	}
	for _, mod := range []string{"nest", "remote_entity"} {
		if _, err := Add(root, AddOptions{Kind: "mod", Name: mod, Service: "alliance"}); err != nil {
			t.Fatal(err)
		}
	}
	// 骨架的 Apply / Result 标了 route：拥有者还没托管 Remote 实体 kind 时拒绝并写明怎么声明。
	if _, err := Add(root, AddOptions{Kind: "rpc", Name: "Alliance", Service: "alliance", EntityService: true}); err == nil || !strings.Contains(err.Error(), "remote=managed service=alliance") {
		t.Fatalf("an entity-service rpc whose owner has no Remote managed kind must be refused with the declaration to write, got %v", err)
	}
	declareRemoteGuild(t, root, "service=alliance")
	// 经命令行走一遍：-entity-service 要在 add rpc 的参数白名单里。
	var cliOut bytes.Buffer
	if err := Run([]string{"add", "rpc", "Alliance", "-service", "alliance", "-entity-service", "--root", root}, &cliOut, &cliOut); err != nil {
		t.Fatalf("roost add rpc -entity-service: %v\n%s", err, cliOut.String())
	}
	iface := readProjectFile(t, root, "internal/rpc/alliance/alliance.go")
	for _, want := range []string{
		"requestID string, callerID int64, callerSID int32",
		"//roost:rpc route\n\tApply(ctx context.Context",
		"//roost:rpc route\n\tResult(ctx context.Context, target int64, requestID string) (found bool, outcome int32, version uint64, err error)",
		"errcode.CodeRetryable", "errcode.CodeRedirect", "target is the full entity id",
	} {
		if !strings.Contains(iface, want) {
			t.Errorf("interface lacks %q:\n%s", want, iface)
		}
	}
	service := readProjectFile(t, root, "internal/rpc/alliance/service.go")
	for _, want := range []string{"HandleBudget", "context.WithTimeout(ctx, HandleBudget)", "kitremoteentity.RPCWriteAnswer(ctx, s.owners, s.localSID, target, err)",
		"kitremoteentity.RPCRouteCheck(ctx, s.owners, s.localSID, target)", "_ RouteChecker = (*Service)(nil)"} {
		if !strings.Contains(service, want) {
			t.Errorf("service skeleton lacks %q:\n%s", want, service)
		}
	}
	mod := readProjectFile(t, root, "internal/rpc/alliance/mod.go")
	for _, want := range []string{"[]app.ModName{mods.ModNats, mods.ModNest, mods.ModRemoteEntity}", "[]app.ModName{mods.ModRemoteEntityPreload}", "m.service.SetOwnership(owners, identity.Sid)"} {
		if !strings.Contains(mod, want) {
			t.Errorf("owner Mod lacks %q:\n%s", want, mod)
		}
	}
	if transport := readProjectFile(t, root, "internal/rpc/alliance/alliance_rpc_gen.go"); !strings.Contains(transport, "c.callRouted(ctx, target, MethodApply") ||
		!strings.Contains(transport, "routes.CheckRoute(ctx.Context(), wire.Target)") {
		t.Errorf("the generated transport must route Apply to the owner and check the route first:\n%s", transport)
	}
	if testing.Short() {
		return
	}
	useLocalRoostCore(t, root)
	for _, action := range []string{"build", "vet"} {
		check := exec.Command("go", action, "./...")
		check.Dir = root
		check.Env = append(os.Environ(), "GOWORK=off")
		if output, err := check.CombinedOutput(); err != nil {
			t.Fatalf("go %s after add rpc -entity-service: %v\n%s", action, err, output)
		}
	}
}

// declareRemoteGuild 加实体 Guild（若还没有），把它改成 Remote 托管并写上 extra（例如 service=alliance preload=startup），
// 然后 project sync。
func declareRemoteGuild(t *testing.T, root, extra string) {
	t.Helper()
	path := filepath.Join(root, "game", "entities", "guild", "entity.go")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if _, err := Add(root, AddOptions{Kind: "entity", Name: "Guild"}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	remote := strings.Replace(string(raw), "category=entity.EntityCategoryOther", "category=entity.EntityCategoryRemote remote=managed "+extra, 1)
	remote = strings.Replace(remote, "*entity.EntityBase", "*entity.RemoteEntityBase", 1)
	if remote == string(raw) {
		t.Fatalf("entity marker shape changed; update this helper:\n%s", raw)
	}
	if err := os.WriteFile(path, []byte(remote), 0o644); err != nil {
		t.Fatal(err)
	}
	// 生成的 lifecycle_gen.go 用 core 的 wiring/entitylifecycle（v1.47.0 新增）：sync 的依赖整理指向本仓，
	// 不去取已发布的 core。
	useLocalRoostCore(t, root)
	var out bytes.Buffer
	if err := Run([]string{"project", "sync", "--root", root}, &out, &out); err != nil {
		t.Fatalf("project sync: %v\n%s", err, out.String())
	}
}

// 路由是方法级策略：只查标了 route 的方法。标了 route 的方法要求它所在 rpc 的拥有者托管 Remote 实体 kind——路由客户端
// 按实体的 Redis 路由登记与持久所有权记录找 owner，只有 //roost:entity remote=managed service=<owner> 的 kind 有这两样；
// 拥有者没有这类 kind 时每次调用都只会得到“实体不存在”，所以 roost generate 在跑任何生成器之前拒绝并写明原因。
// 同一个 rpc 不标 route 时照常生成。
func TestARouteRPCNeedsAnOwnerThatOwnsARemoteKind(t *testing.T) {
	t.Parallel()
	_, root, err := NewProject(NewOptions{
		Name: "planet", Module: "example.com/planet", Out: filepath.Join(t.TempDir(), "planet"),
		Services: []string{"game", "alliance"}, Mods: []string{"configdata", "nest"}, Features: []string{"protocol", "errcode"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "rpc", Name: "Alliance", Service: "alliance"}); err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatalf("an rpc without route methods must generate whatever its owner hosts: %v", err)
	}
	iface := filepath.Join(root, "internal", "rpc", "alliance", "alliance.go")
	source := readProjectFile(t, root, "internal/rpc/alliance/alliance.go")
	routed := strings.Replace(source, "\tPing(ctx", "\t//roost:rpc route=callerID\n\tPing(ctx", 1)
	if routed == source {
		t.Fatalf("rpc skeleton shape changed; update this test:\n%s", source)
	}
	if err := os.WriteFile(iface, []byte(routed), 0o644); err != nil {
		t.Fatal(err)
	}
	err = Generate(root, GenerateOptions{Stdout: io.Discard})
	if err == nil {
		t.Fatal("generate accepted a route method on an rpc whose owner owns no Remote managed kind")
	}
	for _, want := range []string{"internal/rpc/alliance", "Alliance.Ping", "owns no Remote managed entity kind", "remote=managed service=alliance"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
}

// 路由 rpc 的工程接线（联盟服务方案 §2.7、§2.10 第 2 项）：拥有者的 remote_entity 带 WithRouteKinds / WithPreloadKinds、
// 有预加载 kind 时装 RemotePreloadMod，owner Mod 排在预加载之后；调用方要有发布路由查找的 Mod（remote_mirror）；
// generate 前核对 bootstrap 与这些规则。生成工程能编译、vet。
func TestRouteRPCWiringInTheGeneratedProject(t *testing.T) {
	t.Parallel()
	_, root, err := NewProject(NewOptions{
		Name: "planet", Module: "example.com/planet", Out: filepath.Join(t.TempDir(), "planet"),
		Services: []string{"game", "alliance"}, Mods: []string{"configdata", "nest"}, Features: []string{"entity", "dao", "nest", "protocol", "errcode"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "mod", Name: "remote_entity", Service: "alliance"}); err != nil {
		t.Fatal(err)
	}
	declareRemoteGuild(t, root, "service=alliance preload=startup")
	if _, err := Add(root, AddOptions{Kind: "rpc", Name: "Alliance", Service: "alliance", EntityService: true}); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	game := m.Services["game"]
	game.UsesRpcs = []string{"alliance"}
	m.Services["game"] = game
	raw, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ManifestName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// game 调用路由方法却没有路由查找：sync 之后的生成被拒并给出命令。
	if _, err := SyncProject(root); err == nil || !strings.Contains(err.Error(), "roost add mod remote_mirror -service game") {
		t.Fatalf("a caller of route methods without remote_mirror / remote_entity must be refused with the fix, got %v", err)
	}
	if _, err := Add(root, AddOptions{Kind: "mod", Name: "remote_mirror", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	// owner Mod 没排在预加载之后：拒绝。
	modPath := filepath.Join(root, "internal", "rpc", "alliance", "mod.go")
	mod := readProjectFile(t, root, "internal/rpc/alliance/mod.go")
	unordered := strings.Replace(mod, "[]app.ModName{mods.ModRemoteEntityPreload}", "nil", 1)
	if err := os.WriteFile(modPath, []byte(unordered), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err == nil || !strings.Contains(err.Error(), "ModRemoteEntityPreload") {
		t.Fatalf("an owner Mod not ordered after the preload must be refused, got %v", err)
	}
	if err := os.WriteFile(modPath, []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	bootstrap := readProjectFile(t, root, "internal/bootstrap/generated.go")
	alliance, gameSection, found := strings.Cut(bootstrap, `app.ServiceName("game")`)
	if !found {
		t.Fatalf("bootstrap has no game service:\n%s", bootstrap)
	}
	for _, want := range []string{`kitremoteentity.WithRouteKinds(registry.RouteKinds("alliance")...)`, `kitremoteentity.WithPreloadKinds(registry.PreloadKinds("alliance")...)`, "kitremoteentity.NewRemotePreloadMod()"} {
		if !strings.Contains(alliance, want) {
			t.Errorf("alliance bootstrap lacks %s:\n%s", want, bootstrap)
		}
	}
	if strings.Contains(gameSection, "RouteKinds") || strings.Contains(gameSection, "NewRemotePreloadMod") || !strings.Contains(gameSection, "rpcAlliance.NewClientMod()") {
		t.Errorf("game only calls the rpc: a ClientMod, no route registration, no preload:\n%s", bootstrap)
	}
	// 去掉 preload=startup 而不 sync：bootstrap 还装着预加载，generate 要求先 sync。
	guild := filepath.Join(root, "game", "entities", "guild", "entity.go")
	entitySource := readProjectFile(t, root, "game/entities/guild/entity.go")
	if err := os.WriteFile(guild, []byte(strings.Replace(entitySource, " preload=startup", "", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err == nil || !strings.Contains(err.Error(), "roost project sync") {
		t.Fatalf("a bootstrap that still preloads after preload= was removed must ask for project sync, got %v", err)
	}
	if err := os.WriteFile(guild, []byte(entitySource), 0o644); err != nil {
		t.Fatal(err)
	}
	if testing.Short() {
		return
	}
	useLocalRoostCore(t, root)
	for _, action := range []string{"build", "vet"} {
		check := exec.Command("go", action, "./...")
		check.Dir = root
		check.Env = append(os.Environ(), "GOWORK=off")
		if output, err := check.CombinedOutput(); err != nil {
			t.Fatalf("go %s with a route rpc: %v\n%s", action, err, output)
		}
	}
}

// roost generate 写进 rpc 生成文件头的重跑命令必须与接口文件的 //go:generate 相同：否则 go generate ./... 改写文件头，
// 生成工程 porcelain 不干净、generate --check 报过期。
func TestRoostGenerateAndGoGenerateWriteTheSameRPCHeader(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "configdata")
	if _, err := Add(root, AddOptions{Kind: "rpc", Name: "Guild", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	iface := readProjectFile(t, root, "internal/rpc/guild/guild.go")
	_, command, found := strings.Cut(iface, "//go:generate ")
	if !found {
		t.Fatalf("interface has no //go:generate:\n%s", iface)
	}
	command, _, _ = strings.Cut(command, "\n")
	for _, rel := range []string{"internal/rpc/guild/guild_rpc_gen.go", "internal/rpc/guild/guild_rpc_assembly_gen.go"} {
		if header := readProjectFile(t, root, rel); !strings.Contains(header, "//\t"+command+"\n") {
			t.Errorf("%s does not cite the //go:generate command %q:\n%s", rel, command, header[:min(len(header), 600)])
		}
	}
}

// 停机先撤 handler（联盟服务冒烟的观察 ②，App 生命周期方案）：App 停机第一步经 nats Mod 的 CloseIngress 撤下总线上的
// 全部 handler，之后才停 Nest / Remote 与 owner Mod 自己；没有这一步时 handler 一直挂到 nats Mod 停止，停机窗口里的
// 请求进了已停的 Nest，调用方拿到 internal（结果未知）。生成工程里按 App 的停法（先关总线输入、再停 owner Mod）停，
// 之后经真实 Bus 发来的请求必须是“可重试”（no responders 或 CodeRetryable），-entity-service 与普通 rpc 都一样；
// 生成的 owner Mod 自己不再撤（Stop 是空操作）。传输是测试里的内存 NATS。
func TestGeneratedRPCOwnerHandlersAreWithdrawnWhenTheAppStops(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and tests a generated project")
	}
	t.Parallel()
	_, root, err := NewProject(NewOptions{
		Name: "planet", Module: "example.com/planet", Out: filepath.Join(t.TempDir(), "planet"),
		Services: []string{"game", "alliance"}, Mods: []string{"configdata"}, Features: []string{"entity", "dao", "nest", "protocol", "errcode"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, mod := range []string{"nest", "remote_entity"} {
		if _, err := Add(root, AddOptions{Kind: "mod", Name: mod, Service: "alliance"}); err != nil {
			t.Fatal(err)
		}
	}
	declareRemoteGuild(t, root, "service=alliance")
	if _, err := Add(root, AddOptions{Kind: "rpc", Name: "Alliance", Service: "alliance", EntityService: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "rpc", Name: "Notice", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	for rel, source := range map[string]string{
		"internal/rpc/alliance/memnats_test.go": strings.Replace(generatedMemNATSTest, "package rpcpkg", "package alliance", 1),
		"internal/rpc/alliance/stop_test.go":    generatedEntityOwnerStopTest,
		"internal/rpc/notice/memnats_test.go":   strings.Replace(generatedMemNATSTest, "package rpcpkg", "package notice", 1),
		"internal/rpc/notice/stop_test.go":      generatedPlainOwnerStopTest,
	} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	useLocalRoostCore(t, root)
	test := exec.Command("go", "test", "-count=1", "-run", "TestOwnerModStop", "./internal/rpc/alliance/", "./internal/rpc/notice/")
	test.Dir = root
	test.Env = append(os.Environ(), "GOWORK=off")
	if output, err := test.CombinedOutput(); err != nil {
		t.Fatalf("the generated owner Mods' stop tests failed: %v\n%s", err, output)
	}
}

// generatedMemNATSTest 是生成工程测试用的内存 NATS（fnats.IClient + fnats.IRpc）：订阅按精确主题登记，队列组只交给一个
// 订阅者，没有订阅者的请求得到 fnats.ErrNoResponders，与真实 NATS 对“撤掉订阅”的答复相同。
const generatedMemNATSTest = `package rpcpkg

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

type memNATS struct {
	mu      sync.Mutex
	subs    map[*memSub]struct{}
	waiting map[string]chan []byte
	seq     int
}

func newMemNATS() *memNATS {
	return &memNATS{subs: map[*memSub]struct{}{}, waiting: map[string]chan []byte{}}
}

type memSub struct {
	n       *memNATS
	subject string
	handler fnats.MsgHandler
}

func (s *memSub) Unsubscribe() error {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	delete(s.n.subs, s)
	return nil
}

func (s *memSub) IsValid() bool {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	_, ok := s.n.subs[s]
	return ok
}

func (n *memNATS) subscribe(subject string, handler fnats.MsgHandler) (fnats.ISubscription, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	sub := &memSub{n: n, subject: subject, handler: handler}
	n.subs[sub] = struct{}{}
	return sub, nil
}

func (n *memNATS) Subscribe(subject string, handler fnats.MsgHandler) (fnats.ISubscription, error) {
	return n.subscribe(subject, handler)
}

func (n *memNATS) QueueSubscribe(subject, _ string, handler fnats.MsgHandler) (fnats.ISubscription, error) {
	return n.subscribe(subject, handler)
}

func (n *memNATS) Publish(subject string, data []byte) error {
	n.mu.Lock()
	var handlers []fnats.MsgHandler
	for sub := range n.subs {
		if sub.subject == subject {
			handlers = append(handlers, sub.handler)
		}
	}
	n.mu.Unlock()
	for _, handler := range handlers {
		handler(&fnats.Msg{Subject: subject, Data: data})
	}
	return nil
}

func (n *memNATS) Request(string, []byte, time.Duration) ([]byte, error) {
	return nil, errors.New("memnats: use IRpc")
}
func (n *memNATS) Drain() error { return nil }
func (n *memNATS) Close()       {}

func (n *memNATS) Call(ctx context.Context, subject string, req []byte) ([]byte, error) {
	n.mu.Lock()
	var target *memSub
	for sub := range n.subs {
		if sub.subject == subject {
			target = sub
			break
		}
	}
	if target == nil {
		n.mu.Unlock()
		return nil, fnats.ErrNoResponders
	}
	n.seq++
	inbox := fmt.Sprintf("_INBOX.%d", n.seq)
	reply := make(chan []byte, 1)
	n.waiting[inbox] = reply
	n.mu.Unlock()
	target.handler(&fnats.Msg{Subject: subject, Reply: inbox, Data: append([]byte(nil), req...)})
	select {
	case data := <-reply:
		return data, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (n *memNATS) CallWithTimeout(subject string, req []byte, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return n.Call(ctx, subject, req)
}

func (n *memNATS) CallAsync(subject string, req []byte, cb fnats.RpcCallback) {
	go func() { cb(n.CallWithTimeout(subject, req, time.Second)) }()
}

func (n *memNATS) Reply(inbox string, resp []byte) error {
	n.mu.Lock()
	reply := n.waiting[inbox]
	delete(n.waiting, inbox)
	n.mu.Unlock()
	if reply != nil {
		reply <- resp
	}
	return nil
}

var (
	_ fnats.IClient = (*memNATS)(nil)
	_ fnats.IRpc    = (*memNATS)(nil)
)
`

// generatedOwnerStopHelpers 是两份 stop 测试共用的部分：起服务端与调用方两个 Bus，按 App 的停法停：先关总线的输入（App
// 停机第一步，nats Mod 的 CloseIngress），再停 owner Mod。
const generatedOwnerStopHelpers = `
func startBus(t *testing.T, n *memNATS, svc string, sid int32) *bus.Bus {
	t.Helper()
	b := bus.New(n, n, nil, bus.Config{Sid: sid, SvcType: svc, WorkerNum: 2})
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.StopWithContext(context.Background()) })
	return b
}

// stopLikeTheApp 与 App 的停法相同：停机第一步关总线的输入，然后停 Mod（实现了 StopWithContext 就带预算调它，否则调 Stop）。
func stopLikeTheApp(t *testing.T, server *bus.Bus, mod app.Mod) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := server.CloseIngress(ctx); err != nil {
		t.Fatalf("closing the bus inputs: %v", err)
	}
	if stopper, ok := mod.(app.ModStopperWithContext); ok {
		if err := stopper.StopWithContext(ctx); err != nil {
			t.Fatalf("owner Mod StopWithContext: %v", err)
		}
		return
	}
	mod.Stop()
}
`

const generatedEntityOwnerStopTest = `package alliance

import (
	"context"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	"github.com/tjbdwanghaibo/roost-core/infra/network/servicerpc"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// localOwners 让路由前置检查认定本实例（sid 3000）是每个实体的 owner。
type localOwners struct{ entity.IRemoteEntityManager }

func (localOwners) GetRemoteOwnership(context.Context, int64) (entity.RemoteEntityMarkerLease, bool, error) {
	return entity.RemoteEntityMarkerLease{OwnerSid: 3000}, true, nil
}

type ownerAt int32

func (o ownerAt) LookupRoute(context.Context, int64) (int32, error) { return int32(o), nil }

// App 停机开始之后（随后停 Nest 与 Remote），发来的路由请求必须是“可重试，未执行”，不能再进 handler。
func TestOwnerModStopWithdrawsTheHandlersBeforeNestStops(t *testing.T) {
	n := newMemNATS()
	server, caller := startBus(t, n, ServiceType, 3000), startBus(t, n, "game", 9100)
	cfg := viper.New()
	cfg.Set("sid", 3000)
	cfg.Set("server_type", ServiceType)
	registry := app.NewRegistry(cfg)
	if err := mods.RegisterAll(registry,
		mods.Capability{Name: mods.ModBus, Value: bus.IBus(server)},
		mods.Capability{Name: mods.ModRemoteEntity, Value: entity.IRemoteEntityManager(localOwners{})},
	); err != nil {
		t.Fatal(err)
	}
	mod := NewMod(New())
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	client, err := NewBusClient(caller, ownerAt(3000), "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// 停机之前请求进 handler（骨架的 Apply 还没实现，答复是它自己的错误，不是“可重试”）。
	if _, err := client.Apply(ctx, "req-1", 7, 9100, 1); servicerpc.Classify(err) == servicerpc.OutcomeRetryable {
		t.Fatalf("before the stop the request must reach the handler, got %v", err)
	}
	stopLikeTheApp(t, server, mod)
	_, err = client.Apply(ctx, "req-2", 7, 9100, 1)
	if outcome := servicerpc.Classify(err); outcome != servicerpc.OutcomeRetryable {
		t.Fatalf("a request after the owner Mod stopped was answered %v (outcome %s); want no responders or CodeRetryable: "+
			"the App must withdraw the handler before Nest stops", err, outcome)
	}
}
` + generatedOwnerStopHelpers

const generatedPlainOwnerStopTest = `package notice

import (
	"context"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	"github.com/tjbdwanghaibo/roost-core/infra/network/servicerpc"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// 普通 rpc 同一规则：App 停机开始之后的请求不再进 handler（它用的依赖随后就停）。
func TestOwnerModStopWithdrawsTheHandlers(t *testing.T) {
	n := newMemNATS()
	server, caller := startBus(t, n, ServiceType, 1), startBus(t, n, "gate", 9100)
	cfg := viper.New()
	registry := app.NewRegistry(cfg)
	if err := mods.RegisterAll(registry, mods.Capability{Name: mods.ModBus, Value: bus.IBus(server)}); err != nil {
		t.Fatal(err)
	}
	mod := NewMod(New())
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	client, err := NewBusClient(caller, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if reply, err := client.Ping(ctx, 7, "hello"); err != nil {
		t.Fatalf("before the stop: reply %q err %v", reply, err)
	}
	stopLikeTheApp(t, server, mod)
	reply, err := client.Ping(ctx, 7, "hello")
	if outcome := servicerpc.Classify(err); outcome != servicerpc.OutcomeRetryable {
		t.Fatalf("a request after the owner Mod stopped was answered %q, %v (outcome %s); want no responders or CodeRetryable: "+
			"the App must withdraw the handler when it stops", reply, err, outcome)
	}
}
` + generatedOwnerStopHelpers
