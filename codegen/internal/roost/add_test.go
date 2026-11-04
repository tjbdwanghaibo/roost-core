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
	// The lifecycle scaffold used to reference the per-entity category
	// constant the entity scaffold minted. That constant is gone, so anything
	// still naming it would leave the project unbuildable.
	if _, err := Add(root, AddOptions{Kind: "access", Name: "player", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	lifecyclePaths, err := Add(root, AddOptions{Kind: "lifecycle", Name: "Player", Entity: "Player", Service: "game"})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range lifecyclePaths {
		raw, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			continue
		}
		if strings.Contains(string(raw), "EntityCategoryPlayer") {
			t.Errorf("%s still names the removed per-entity category constant:\n%s", path, raw)
		}
	}

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
	if err := os.WriteFile(filepath.Join(root, "game/playeraccess/manual_reply_test.go"), []byte(manualReplyTest), 0600); err != nil {
		t.Fatal(err)
	}
	run("test", "./game/playeraccess", "-run", "^TestManualReplyDoesNotEncodeAgain$", "-count=1")
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

const manualReplyTest = `package playeraccess
import (
 "context"
 "testing"
 "github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
)
type responseSession struct { replies int }
func (*responseSession) Principal() gateway.Principal { return gateway.Principal{PlayerID:1,SessionID:"test"} }
func (s *responseSession) Reply(context.Context,any) error {s.replies++;return nil}
func (*responseSession) Close(error) error {return nil}
func TestManualReplyDoesNotEncodeAgain(t *testing.T) {
 registry:=NewProtocolRegistry()
 type reply struct{}
 encoded:=0
 if err:=RegisterProtocol[int,*reply](registry,1,2,func([]byte)(int,error){return 0,nil},func(*reply)([]byte,error){encoded++;return nil,nil},func(ctx *Context,_ int)(*reply,error){return nil,ctx.Session.Reply(ctx.Context(),&Response{MessageID:2,Sequence:ctx.Seq})});err!=nil {t.Fatal(err)}
 session:=&responseSession{}
 response,err:=registry.Dispatch(context.Background(),session,1,7,nil)
 if err!=nil || response!=nil || encoded!=0 || session.replies!=1 {t.Fatalf("response=%v err=%v encoded=%d replies=%d",response,err,encoded,session.replies)}
}
`

// add rpc -entity-service（联盟服务方案 §2.3 / §2.10 第 4 项）：写方法带 RequestID，Result 按 RequestID 查回执，
// 接收端有自己的处理预算，答复分成成功 / 业务拒绝 / 未执行可重试 / 结果未知；owner Mod 依赖 nest，
// Nest（与它之前的 DataEngine 恢复）启动之后才在总线上接请求。拥有者没有 Nest 时拒绝。生成工程能编译。
func TestAddRPCEntityServiceScaffoldsTheWriteContract(t *testing.T) {
	t.Parallel()
	_, root, err := NewProject(NewOptions{
		Name: "planet", Module: "example.com/planet", Out: filepath.Join(t.TempDir(), "planet"),
		Services: []string{"game", "alliance"}, Mods: []string{"configdata"}, Features: []string{"protocol", "errcode"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "rpc", Name: "Alliance", Service: "alliance", EntityService: true}); err == nil || !strings.Contains(err.Error(), "roost add mod nest -service alliance") {
		t.Fatalf("an entity-service rpc owned by a service without Nest must be refused with the fix, got %v", err)
	}
	if _, err := Add(root, AddOptions{Kind: "mod", Name: "nest", Service: "alliance"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "rpc", Name: "Alliance", Service: "alliance", EntityService: true}); err != nil {
		t.Fatalf("add rpc -entity-service: %v", err)
	}
	iface := readProjectFile(t, root, "internal/rpc/alliance/alliance.go")
	for _, want := range []string{
		"requestID string, callerID int64, callerSID int32",
		"Result(ctx context.Context, target int64, requestID string) (found bool, code int32, version uint64, err error)",
		"errcode.CodeRetryable", "errcode.CodeRedirect",
	} {
		if !strings.Contains(iface, want) {
			t.Errorf("interface lacks %q:\n%s", want, iface)
		}
	}
	service := readProjectFile(t, root, "internal/rpc/alliance/service.go")
	for _, want := range []string{"HandleBudget", "context.WithTimeout(ctx, HandleBudget)", "kitremoteentity.RPCWriteAnswer(ctx, s.owners, s.localSID, target, err)"} {
		if !strings.Contains(service, want) {
			t.Errorf("service skeleton lacks %q:\n%s", want, service)
		}
	}
	if mod := readProjectFile(t, root, "internal/rpc/alliance/mod.go"); !strings.Contains(mod, "[]app.ModName{mods.ModNats, mods.ModNest}") {
		t.Errorf("owner Mod must start after Nest:\n%s", mod)
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

// //roost:rpc shard= 的槽位表（阶段 1 的 servicerpc.SlotTableConfig，键 <service_type>.slots / .shards）：调用方
// 必须配置，所有写了它的服务取值必须相同（slots 是整数，用不了 shared tag），doctor 的 rpc-slots 项核对。
func TestDoctorChecksTheSlotTableOfAShardedRPC(t *testing.T) {
	t.Parallel()
	_, root, err := NewProject(NewOptions{
		Name: "planet", Module: "example.com/planet", Out: filepath.Join(t.TempDir(), "planet"),
		Services: []string{"game", "alliance"}, Mods: []string{"configdata", "nest"}, Features: []string{"protocol", "errcode"},
	})
	if err != nil {
		t.Fatal(err)
	}
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
	if _, err := SyncProject(root); err != nil {
		t.Fatal(err)
	}
	if m, err = LoadManifest(root); err != nil {
		t.Fatal(err)
	}
	if items := checkSlotTables(root, m); len(items) != 0 {
		t.Fatalf("an rpc without shard= methods must not be checked: %+v", items)
	}
	iface := filepath.Join(root, "internal", "rpc", "alliance", "alliance.go")
	source := readProjectFile(t, root, "internal/rpc/alliance/alliance.go")
	sharded := strings.Replace(source, "\tApply(ctx", "\t//roost:rpc shard=target\n\tApply(ctx", 1)
	if err := os.WriteFile(iface, []byte(sharded), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatalf("generate with shard=: %v", err)
	}
	items := checkSlotTables(root, m)
	if len(items) != 1 || items[0].Status != StatusFail || !strings.Contains(items[0].Detail, "configs/service/config.game.yaml") || !strings.Contains(items[0].Detail, "alliance.slots") {
		t.Fatalf("a caller without the slot table = %+v; want one FAIL naming the game config and alliance.slots", items)
	}
	appendYAML := func(file, body string) {
		path := filepath.Join(root, "configs", "service", "config."+file+".yaml")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(raw, []byte(body)...), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	appendYAML("game", "\nalliance:\n  slots: 256\n  shards: [\"3000:0-255\"]\n")
	appendYAML("game.prod.example", "\nalliance:\n  slots: 256\n  shards: [\"3000:0-255\"]\n")
	if items := checkSlotTables(root, m); len(items) != 1 || items[0].Status != StatusOK {
		t.Fatalf("after the caller configures the table = %+v; want OK", items)
	}
	appendYAML("alliance", "\nalliance:\n  slots: 256\n  shards: [\"3000:0-127\", \"3001:128-255\"]\n")
	if items := checkSlotTables(root, m); len(items) != 1 || items[0].Status != StatusFail || !strings.Contains(items[0].Detail, "alliance.shards") {
		t.Fatalf("owner and caller disagree on the table = %+v; want FAIL naming alliance.shards", items)
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
			t.Fatalf("go %s with a sharded rpc: %v\n%s", action, err, output)
		}
	}
}
