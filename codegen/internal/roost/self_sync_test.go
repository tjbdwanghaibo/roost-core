package roost

// access.player.self_sync：开关的校验、关闭时生成物不引用自身视图、打开时生成可见接线与可照做的报错，
// 以及一个按新手流程搭出的工程打开开关后，经生成的接线跑真实 TCP：登录 → handler 改金币 → 客户端收到增量，
// 断线重连 → 收到全量。

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func selfSyncManifest(t *testing.T, transports ...string) Manifest {
	t.Helper()
	m := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"configdata", "mongo", "nats", "dataengine", "nest"}, nil)
	m.Access = map[string]AccessSpec{"player": {Service: "game", Transports: transports, SelfSync: true}}
	return m
}

func TestSelfSyncManifestValidation(t *testing.T) {
	if err := selfSyncManifest(t, "tcp").Validate(); err != nil {
		t.Fatalf("self_sync with tcp refused: %v", err)
	}
	err := selfSyncManifest(t).Validate()
	if err == nil || !strings.Contains(err.Error(), "access.player.self_sync needs the tcp transport") || !strings.Contains(err.Error(), "roost add transport tcp") {
		t.Fatalf("self_sync without tcp: %v, want a refusal naming the fix", err)
	}
	m := selfSyncManifest(t, "tcp")
	m.Features = []string{"protocol", "nest", "dao"}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "needs the entity feature") {
		t.Fatalf("self_sync without the entity feature: %v", err)
	}
}

// 开关关闭（默认）时 bootstrap 与 player TCP 生成物照旧：不引用 wiring/selfsync，Nest 仍是 NewMod。
func TestSelfSyncOffKeepsTheFormerWiring(t *testing.T) {
	m := selfSyncManifest(t, "tcp")
	access := m.Access["player"]
	access.SelfSync = false
	m.Access["player"] = access
	files, err := renderProject(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"internal/bootstrap/generated.go", "internal/access/player/tcp/server_gen.go"} {
		body := string(files[path].Body)
		if strings.Contains(body, "selfsync") || strings.Contains(body, "SelfSync") {
			t.Errorf("%s mentions the self view with self_sync off", path)
		}
	}
	if bootstrap := string(files["internal/bootstrap/generated.go"].Body); !strings.Contains(bootstrap, "kitnest.NewMod(EntityAccess)") || strings.Contains(bootstrap, "NewModWithEntitySync") {
		t.Errorf("bootstrap Nest constructor changed with self_sync off:\n%s", bootstrap)
	}
	if _, exists := files[selfSyncProtocolPath]; exists {
		t.Errorf("%s planned with self_sync off", selfSyncProtocolPath)
	}
	if guide := string(files["docs/PLAYER_ACCESS_TCP.zh-CN.md"].Body); guide != renderPlayerTCPGuide() {
		t.Errorf("the player TCP guide changed with self_sync off")
	}
}

// 开关打开：接线全部在生成器所有的文件里，推送编号的协议声明归项目（只创建一次）。
func TestSelfSyncRendersVisibleWiring(t *testing.T) {
	files, err := renderProject(selfSyncManifest(t, "tcp"))
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := files["internal/bootstrap/generated.go"]
	if !bootstrap.Owned {
		t.Fatal("bootstrap is not generator-owned")
	}
	for _, want := range []string{
		`selfsync "github.com/tjbdwanghaibo/roost-core/wiring/selfsync"`,
		"var PlayerSelfSync = selfsync.MustNew(selfsync.Config{Entities: EntityAccess, Kind: player.EntityKindPlayer, MessageID: msgid.MsgSelfSync})",
		"kitnest.NewModWithEntitySync(EntityAccess, PlayerSelfSync.EntitySyncSetup())",
		"accessplayertcp.NewMod().WithSelfSync(PlayerSelfSync)",
	} {
		if !strings.Contains(string(bootstrap.Body), want) {
			t.Errorf("bootstrap lacks %q:\n%s", want, bootstrap.Body)
		}
	}
	server := files["internal/access/player/tcp/server_gen.go"]
	for _, want := range []string{
		"func (mod *Mod) WithSelfSync(self *selfsync.SelfSync) *Mod",
		"mod.selfSync.BindPlayers(mod.transportRuntime)",
		"server.ConnectHandshake(mod.selfSync.Handshake())",
		"func (mod *IngressMod) WithSelfSync(self *selfsync.SelfSync) *IngressMod",
		"return self.OnActive(binding, receiver)",
		"mod.selfSync.BindPlayers(ingress)",
	} {
		if !server.Owned || !strings.Contains(string(server.Body), want) {
			t.Errorf("server_gen.go lacks %q", want)
		}
	}
	if guide := string(files["docs/PLAYER_ACCESS_TCP.zh-CN.md"].Body); !strings.Contains(guide, "## 8. 玩家看自己的状态（self_sync）") || !strings.Contains(guide, "player_sync_packer_gen.go") || !strings.Contains(guide, "subjectPacker=") {
		t.Errorf("the player TCP guide does not explain self_sync and its default packer")
	}
	def, exists := files[selfSyncProtocolPath]
	if !exists || def.Owned || !strings.Contains(string(def.Body), "SelfSync() SelfSyncPush") || !strings.Contains(string(def.Body), "//roost:msg id=19999") {
		t.Fatalf("%s = %+v, want a business-owned push declaration at the top of the game group", selfSyncProtocolPath, def)
	}
}

// generate / doctor 的前置检查：开关就是“Player 要 Entity Sync”，标记缺省按 sync=true，没写 subjectPacker= 时
// 用生成的默认 packer；拒绝（没有实体、没有 DAO、显式 sync=false、缺推送编号）说出要改的文件与写法，
// DAO 没有 sync 字段只给警告（doctor 的 warn）。
func TestSelfSyncCheckNamesTheFix(t *testing.T) {
	root := t.TempDir()
	m := selfSyncManifest(t, "tcp")
	_, err := checkSelfSync(root, m)
	if err == nil || !strings.Contains(err.Error(), "roost add entity Player") || !strings.Contains(err.Error(), "roost project sync") {
		t.Fatalf("missing entity and def: %v", err)
	}
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
	entity := func(marker, fields string) {
		t.Helper()
		write("game/entities/player/entity.go", "package player\n\nimport db \"example.com/planet/db\"\n\nvar _ db.PlayerDao\n\n"+marker+"\ntype Player struct {\n"+fields+"}\n")
	}
	const oneDao = "\tdao *db.PlayerDao `dao:\"player\"`\n"
	write("go.mod", "module example.com/planet\n\ngo 1.24\n")
	write("db/player_dao_gen.go", "package db\n\ntype PlayerDao struct{}\n")
	write(selfSyncProtocolPath, renderSelfSyncProtocolDef(m))

	// 只建了实体、还没有 DAO：默认 packer 没东西可发。
	entity("//roost:entity id=1 entityKind=EntityKindPlayer category=entity.EntityCategoryOther", "")
	if _, err := checkSelfSync(root, m); err == nil || !strings.Contains(err.Error(), "roost add dao Player --entity Player") || !strings.Contains(err.Error(), selfSyncDocumentURL) {
		t.Fatalf("Player without a DAO: %v", err)
	}
	// 显式 sync=false 与开关矛盾。
	entity("//roost:entity id=1 entityKind=EntityKindPlayer sync=false", oneDao)
	if _, err := checkSelfSync(root, m); err == nil || !strings.Contains(err.Error(), "drop sync=false") {
		t.Fatalf("sync=false: %v", err)
	}
	// 标记不写 sync / subjectPacker、DAO 没有 sync 字段：就绪，默认 packer，带警告。
	entity("//roost:entity id=1 entityKind=EntityKindPlayer category=entity.EntityCategoryOther", oneDao)
	status, err := checkSelfSync(root, m)
	if err != nil {
		t.Fatalf("a bare marker with one DAO refused: %v", err)
	}
	if !strings.Contains(status.packer, "NewPlayerSyncPacker (generated in game/entities/player/player_sync_packer_gen.go)") {
		t.Errorf("packer = %q, want the generated default", status.packer)
	}
	for _, want := range []string{"db.PlayerDao has no field tagged sync", "receives nothing", `dao:"persist,sync"`, "roost generate"} {
		if !strings.Contains(status.warning, want) {
			t.Errorf("warning %q lacks %q", status.warning, want)
		}
	}
	// Nest 在 handler 提交时自动收集客户端同步变化；提示不能让业务手动标脏。
	if strings.Contains(status.warning, "TakeSyncDirty") || strings.Contains(status.warning, "MarkSyncDirty") {
		t.Fatalf("the hint tells the business to mark sync dirty by hand: %s", status.warning)
	}
	// DAO 有 sync 字段（生成的 DAO 带 SyncFields）：没有警告。
	write("db/player_dao_sync_gen.go", "package db\n\nfunc PlayerDaoSyncFields() []int { return nil }\n")
	if status, err := checkSelfSync(root, m); err != nil || status.warning != "" {
		t.Fatalf("with sync fields: status=%+v err=%v", status, err)
	}
	// 业务自己的 packer。
	entity(`//roost:entity id=1 entityKind=EntityKindPlayer sync=true syncNamespace="player" subjectPacker=NewPlayerSyncPacker`, oneDao+"\tbag *db.PlayerDao `dao:\"bag\"`\n")
	if status, err := checkSelfSync(root, m); err != nil || status.packer != "NewPlayerSyncPacker" || status.warning != "" {
		t.Fatalf("custom packer: status=%+v err=%v", status, err)
	}
	// 多个 DAO 而没写 packer：默认 packer 只发一个 DAO，说出怎么写自己的。
	entity("//roost:entity id=1 entityKind=EntityKindPlayer", oneDao+"\tbag *db.PlayerDao `dao:\"bag\"`\n")
	if _, err := checkSelfSync(root, m); err == nil || !strings.Contains(err.Error(), "subjectPacker=NewPlayerSyncPacker") {
		t.Fatalf("two DAOs without a packer: %v", err)
	}
	// 推送编号缺失。
	entity("//roost:entity id=1 entityKind=EntityKindPlayer", oneDao)
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(selfSyncProtocolPath))); err != nil {
		t.Fatal(err)
	}
	if _, err := checkSelfSync(root, m); err == nil || !strings.Contains(err.Error(), "roost project sync") {
		t.Fatalf("missing push id: %v", err)
	}
	access := m.Access["player"]
	access.SelfSync = false
	m.Access["player"] = access
	entity("//roost:entity id=1 entityKind=EntityKindPlayer sync=false", "")
	if _, err := checkSelfSync(root, m); err != nil {
		t.Fatalf("self_sync off still checked: %v", err)
	}
}

// 按新手流程（project new → add access / entity / component / dao / handler / protocol / endpoint / lifecycle /
// transport）搭一个工程，只打开 self_sync、给 DAO 字段标 sync（不改标记、不写 packer），然后在工程里跑一条
// 经生成接线的真实 TCP 用例；再换成业务自己的 packer 跑一遍，最后切回默认 packer。用例走的是：
// 生成的 bootstrap 变量、生成的 Nest Mod（NewModWithEntitySync）、生成的 player 接入与 TCP Mod（WithSelfSync），
// Nest 执行生成的 handler；只把 DataEngine 换成不落库的 committer 与“权威里没有”的 loader。
func TestSelfSyncGeneratedProjectOverRealTCP(t *testing.T) {
	if testing.Short() {
		t.Skip("generates a project and runs go build / vet / test in it")
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	root := filepath.Join(t.TempDir(), "planet")
	if _, _, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: root, Mods: []string{"configdata", "mongo", "nats", "dataengine", "nest"}}); err != nil {
		t.Fatal(err)
	}
	modPath := filepath.Join(root, "go.mod")
	mod, err := os.ReadFile(modPath)
	if err != nil {
		t.Fatal(err)
	}
	mod = regexp.MustCompile(`(?m)^replace github\.com/tjbdwanghaibo/roost-core\b.*$\n?`).ReplaceAll(mod, nil)
	mod = append(mod, []byte("\nreplace github.com/tjbdwanghaibo/roost-core => "+strconv.Quote(filepath.ToSlash(repo))+"\n")...)
	if err := os.WriteFile(modPath, mod, 0o644); err != nil {
		t.Fatal(err)
	}
	roost := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := Run(args, &out, &out); err != nil {
			t.Fatalf("roost %s: %v\n%s", strings.Join(args, " "), err, out.String())
		}
		return out.String()
	}
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
	edit := func(rel, old, replacement string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(raw, []byte(old)) {
			t.Fatalf("%s does not contain %q", rel, old)
		}
		if err := os.WriteFile(path, bytes.Replace(raw, []byte(old), []byte(replacement), 1), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"add", "access", "player", "--service", "game"},
		{"add", "entity", "Player"},
		{"add", "component", "Wallet", "--entity", "Player"},
		{"add", "dao", "Player", "--entity", "Player"},
		{"add", "handler", "AddGold", "--entity", "Player", "--component", "Wallet"},
		{"add", "protocol", "AddGold", "--group", "game", "--handler", "player"},
		{"add", "protocol", "EnterGame", "--group", "game", "--handler", "player"},
		{"add", "lifecycle", "Player", "--service", "game"},
		{"add", "transport", "tcp"},
	} {
		roost(append(args, "--root", root)...)
	}
	// 业务字段与方法（新手手册的编辑点）。
	write("db/def/player.go", "package dbdef\n\n//roost:dao coll=player db=main\ntype PlayerDao struct {\n\tGold int64 `bson:\"gold\" dao:\"persist,sync\"`\n}\n")
	// 组件只经生成的 setter 改 DAO，不手动标脏：客户端收到增量靠 Nest 提交时自动收集，本用例钉住这一点。
	edit("game/entities/player/wallet_component.go", "// Add business methods below.", `// AddGold 加金币；同步变化由 Nest 在提交时收集，不用手动标脏。
func (component *WalletComponent) AddGold(amount int64) error {
	if amount <= 0 {
		return fmt.Errorf("wallet: amount %d must be positive", amount)
	}
	dao := component.owner.Dao()
	dao.SetGold(dao.GetGold() + amount)
	return nil
}

// Add business methods below.`)
	write("game/handler/add_gold.go", "package handler\n\nimport player \"example.com/planet/game/entities/player\"\n\n//roost:nest rollback=undo durability=strict\nfunc handlerAddGold(target player.IWalletEntity, amount int64) error {\n\treturn target.WalletComp().AddGold(amount)\n}\n")
	edit("protocol/def/add_gold.go", "type AddGoldRequest struct{}", "type AddGoldRequest struct {\n\tAmount int64 `pb:\"1\"`\n}")
	// 协议生成先放了“未实现”的 controller 占位；add endpoint 接管它，换成 endpoint 生成的 Sender 映射。
	roost("add", "endpoint", "AddGold", "--handler", "player", "--root", root)
	// Controller 归业务：登录 endpoint 要从 Registry 取 lifecycle，这里给它留一份 Registry。
	write("game/controllers/player/controller.go", `package player

import (
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
)

type Controller struct {
	registry   *app.Registry
	nestClient corenest.Client
}

func FromRegistry(registry *app.Registry) (*Controller, error) {
	if registry == nil {
		return nil, fmt.Errorf("player controller: app registry is required")
	}
	client, ok := app.Lookup[corenest.Client](registry, app.ModName("nest"))
	if !ok || client == nil {
		return nil, fmt.Errorf("player controller: nest client is unavailable")
	}
	return &Controller{registry: registry, nestClient: client}, nil
}

func (controller *Controller) NestClient() corenest.Client { return controller.nestClient }

func (controller *Controller) Registry() *app.Registry { return controller.registry }
`)
	write("game/controllers/player/enter_game.go", `package player

import (
	"example.com/planet/game/lifecycle"
	"example.com/planet/game/playeraccess"
	"example.com/planet/protocol/pb"
)

// HandleEnterGame 首登创建、之后加载玩家实体。
func (controller *Controller) HandleEnterGame(ctx *playeraccess.Context, _ *pb.EnterGameRequest) (*pb.EnterGameResponse, error) {
	players, err := lifecycle.PlayerFromRegistry(controller.Registry())
	if err != nil {
		return nil, err
	}
	if _, _, err := players.GetOrCreate(ctx.Context(), ctx.PlayerID); err != nil {
		return &pb.EnterGameResponse{Code: 1, Reason: err.Error()}, nil
	}
	return &pb.EnterGameResponse{}, nil
}
`)
	write("internal/access/player/tcp/auth.go", selfSyncTestAuthenticator)

	// 打开开关：标记一个字都不改、不写 packer，project sync 直接通过，生成默认 packer。
	edit("roost.yaml", "        transports:\n            - tcp\n", "        transports:\n            - tcp\n        self_sync: true\n")
	entityBefore, err := os.ReadFile(filepath.Join(root, "game", "entities", "player", "entity.go"))
	if err != nil {
		t.Fatal(err)
	}
	roost("project", "sync", "--skip-deps", "--root", root)
	roost("generate", "--root", root)
	generatedPacker := filepath.Join(root, "game", "entities", "player", "player_sync_packer_gen.go")
	exists := func(path string) bool {
		_, err := os.Stat(path)
		return err == nil
	}
	if raw, err := os.ReadFile(generatedPacker); err != nil || !bytes.HasPrefix(raw, []byte("// Code generated by tool/entity. DO NOT EDIT.")) || !bytes.Contains(raw, []byte("func NewPlayerSyncPacker(")) {
		t.Fatalf("default packer = %q, %v", raw, err)
	}
	if after, _ := os.ReadFile(filepath.Join(root, "game", "entities", "player", "entity.go")); !bytes.Equal(after, entityBefore) {
		t.Fatalf("project sync rewrote the business entity.go")
	}
	for _, rel := range []string{"internal/bootstrap/generated.go", selfSyncProtocolPath} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/bootstrap/self_sync_e2e_test.go", selfSyncGeneratedE2E)
	runGenerated := func(label string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cancel()
		for _, args := range [][]string{{"build", "./..."}, {"vet", "./..."}, {"test", "-race", "-count=1", "-run", "^(TestSelfSyncEndToEnd|TestSelfSyncFrameLimitFollowsTheAccessLimit)$", "-v", "./internal/bootstrap"}} {
			command := exec.CommandContext(ctx, "go", args...)
			command.Dir = root
			out, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("%s: go %s: %v\n%s", label, strings.Join(args, " "), err, out)
			}
			if args[0] == "test" {
				for _, name := range []string{"TestSelfSyncEndToEnd", "TestSelfSyncFrameLimitFollowsTheAccessLimit"} {
					if !strings.Contains(string(out), "--- PASS: "+name) {
						t.Fatalf("%s: the generated %s did not run:\n%s", label, name, out)
					}
				}
				t.Logf("%s:\n%s", label, out)
			}
		}
	}
	runGenerated("default packer")

	// 业务改用自己的 packer：标记写 subjectPacker=，生成的那份被删掉（还在的话 NewPlayerSyncPacker 重复声明，
	// 编译不过），行为与以前照抄文档的工程相同。
	edit("game/entities/player/entity.go", "category=entity.EntityCategoryOther\n", "category=entity.EntityCategoryOther sync=true syncNamespace=\"player\" subjectPacker=NewPlayerSyncPacker\n")
	write("game/entities/player/sync_packer.go", selfSyncTestPacker)
	roost("project", "sync", "--skip-deps", "--root", root)
	if exists(generatedPacker) {
		t.Fatalf("the default packer survived subjectPacker=NewPlayerSyncPacker")
	}
	runGenerated("custom packer")

	// 再切回来：删掉标记里的 subjectPacker= 与业务 packer，生成的那份回来。
	edit("game/entities/player/entity.go", " sync=true syncNamespace=\"player\" subjectPacker=NewPlayerSyncPacker\n", "\n")
	if err := os.Remove(filepath.Join(root, "game", "entities", "player", "sync_packer.go")); err != nil {
		t.Fatal(err)
	}
	roost("project", "sync", "--skip-deps", "--root", root)
	if !exists(generatedPacker) {
		t.Fatalf("the default packer did not come back after subjectPacker= was removed")
	}
	command := exec.Command("go", "build", "./...")
	command.Dir = root
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("back to the default packer: go build: %v\n%s", err, out)
	}
}

const selfSyncTestAuthenticator = `package tcp

import (
	"context"
	"net"
	"strconv"
	"strings"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
)

// 测试工程专用：接受 "player:<id>"。不可用于生产。
type applicationAuthenticator struct{}

func newApplicationAuthenticator(_ *viper.Viper) (Authenticator, error) { return applicationAuthenticator{}, nil }

func (applicationAuthenticator) Authenticate(_ context.Context, token string, _ net.Addr) (gateway.Principal, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(token, "player:"), 10, 64)
	if err != nil || id <= 0 {
		return gateway.Principal{}, gateway.ErrUnauthenticated
	}
	return gateway.Principal{PlayerID: id, SessionID: token}, nil
}
`

const selfSyncTestPacker = `package player

import (
	"fmt"

	db "example.com/planet/db"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

const PlayerSyncCodec uint16 = 1

// "self" 是 access.player.self_sync 订阅的视图：玩家自己看全部同步字段。
var playerViews, playerViewsErr = dataengine.NewSyncViewSet(db.PlayerDaoSyncFields(),
	entity.NamedSyncView{Profile: entity.SyncProfile{Key: "self"}, Fields: []string{"*"}},
)

func NewPlayerSyncPacker(value entity.IThreadSafeEntity) entity.SubjectSyncPacker {
	owner, _ := value.(*Player)
	return playerSyncPacker{player: owner}
}

type playerSyncPacker struct{ player *Player }

func (packer playerSyncPacker) PackSubjectSnapshot(profile entity.SyncProfile) (entity.FrozenSyncPayload, error) {
	return packer.pack(profile, dataengine.AllFields)
}

func (packer playerSyncPacker) PackSubjectDelta(profile entity.SyncProfile, mask uint64) (entity.FrozenSyncPayload, error) {
	return packer.pack(profile, mask)
}

func (packer playerSyncPacker) pack(profile entity.SyncProfile, mask uint64) (entity.FrozenSyncPayload, error) {
	if packer.player == nil || playerViewsErr != nil {
		return entity.FrozenSyncPayload{}, fmt.Errorf("player sync packer: %v", playerViewsErr)
	}
	view, ok := playerViews.Lookup(profile)
	if !ok {
		return entity.FrozenSyncPayload{}, fmt.Errorf("%w: %+v", entity.ErrSyncViewUnknown, profile)
	}
	if mask &= view.Fields; mask == 0 {
		return entity.FrozenSyncPayload{}, nil
	}
	raw := packer.player.Dao().MarshalSync(mask)
	if len(raw) == 0 {
		return entity.FrozenSyncPayload{}, nil
	}
	return entity.TakeFrozenSyncPayload(PlayerSyncCodec, raw), nil
}
`

// selfSyncGeneratedE2E 放进生成工程的 internal/bootstrap：用生成的 PlayerSelfSync、Nest / 接入 / TCP 三个 Mod
// 走 Init → Provide → Start，客户端按正式 RS v2 帧登录、请求、解码自己的状态。
const selfSyncGeneratedE2E = `package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"example.com/planet/db"
	accessplayer "example.com/planet/internal/access/player"
	accessplayertcp "example.com/planet/internal/access/player/tcp"
	"example.com/planet/protocol/msgid"
	"example.com/planet/protocol/pb"
	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	gatewiring "github.com/tjbdwanghaibo/roost-core/wiring/gate"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	kitnest "github.com/tjbdwanghaibo/roost-core/wiring/nest"
	"github.com/tjbdwanghaibo/roost-core/wiring/selfsync"

	player "example.com/planet/game/entities/player"
	registry "example.com/planet/internal/registry"
)

type noDurableStore struct{}

func (noDurableStore) NestOptions() []corenest.NestOption {
	return []corenest.NestOption{corenest.NestOptionWithTransactionCommitter(noDurableStore{})}
}
func (noDurableStore) Commit(context.Context, corenest.CommitRecord) error { return nil }

type absentAuthority struct{}

func (absentAuthority) LoadEntity(context.Context, int64, entity.EntityKind) (entity.IThreadSafeEntity, error) {
	return nil, engine.ErrEntityAggregateNotFound
}

type selfClient struct {
	conn     net.Conn
	receiver *frame.Receiver
	state    *db.PlayerDao
	seq      uint32
	log      []string
}

func dialSelf(t *testing.T, addr string, player int64) *selfClient {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := wire.Write(conn, []*wire.Packet{{MsgID: 0, Seq: 1, Payload: []byte(fmt.Sprintf("player:%d", player))}}, wire.DefaultMaxPayload); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if ack, err := wire.Read(conn, wire.DefaultMaxPayload); err != nil || ack.MsgID != 0 || ack.Seq != 1 {
		t.Fatalf("login ack %+v %v", ack, err)
	}
	return &selfClient{conn: conn, receiver: frame.NewReceiver(frame.Limits{}), state: db.NewPlayerDao(), seq: 1, log: []string{"ack"}}
}

func (c *selfClient) next(t *testing.T) *wire.Packet {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	packet, err := wire.Read(c.conn, wire.DefaultMaxPayload)
	if err != nil {
		t.Fatalf("read: %v; so far %v", err, c.log)
	}
	if (wire.Header{Flags: packet.Flags}).Kind() != wire.PayloadSync {
		c.log = append(c.log, fmt.Sprintf("reply %d/%d", packet.MsgID, packet.Seq))
		return packet
	}
	if packet.MsgID != msgid.MsgSelfSync {
		t.Fatalf("sync push on message %d, want msgid.MsgSelfSync", packet.MsgID)
	}
	var kind frame.Kind
	applied, err := c.receiver.Receive(packet.Payload, func(decoded frame.Frame) error {
		kind = decoded.Kind
		for _, object := range decoded.Objects {
			for _, component := range object.Components {
				update, err := entitysync.DecodeSubjectUpdate(component.Data, 0)
				if err != nil {
					return err
				}
				if update.Profile.Key != "self" {
					return fmt.Errorf("profile %q, want self", update.Profile.Key)
				}
				if err := c.state.ApplySync(update.Payload.BytesCopy()); err != nil {
					return err
				}
			}
		}
		return nil
	})
	switch {
	case errors.Is(err, frame.ErrBaselineMismatch):
		c.log = append(c.log, "delta before full (skipped)")
	case err != nil:
		t.Fatalf("decode: %v", err)
	case applied:
		name := map[frame.Kind]string{frame.Full: "full", frame.Delta: "delta"}[kind]
		c.log = append(c.log, fmt.Sprintf("%s gold=%d", name, c.state.GetGold()))
	}
	return packet
}

func (c *selfClient) request(t *testing.T, id uint32, payload []byte) {
	t.Helper()
	c.seq++
	if err := wire.Write(c.conn, []*wire.Packet{{MsgID: id, Seq: c.seq, Payload: payload}}, wire.DefaultMaxPayload); err != nil {
		t.Fatal(err)
	}
	for {
		if packet := c.next(t); packet.Flags == 0 && packet.MsgID == id && packet.Seq == c.seq {
			return
		}
	}
}

func (c *selfClient) expect(t *testing.T, want string) {
	t.Helper()
	for range 64 {
		if c.log[len(c.log)-1] == want {
			return
		}
		for _, seen := range c.log {
			if seen == want {
				return
			}
		}
		c.next(t)
	}
	t.Fatalf("never saw %q: %v", want, c.log)
}

func TestSelfSyncEndToEnd(t *testing.T) {
	if err := registry.RegisterAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := EntityAccess.ConfigureLoader(absentAuthority{}); err != nil {
		t.Fatal(err)
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()
	cfg := viper.New()
	cfg.Set("player_access.tcp.enabled", true)
	cfg.Set("player_access.tcp.addr", addr)
	cfg.Set("sync.entity.interval", "10ms")
	reg := app.NewRegistry(cfg)
	if err := reg.Register(mods.ModDataEngine, noDurableStore{}); err != nil {
		t.Fatal(err)
	}
	// 与生成的 bootstrap 同样的三个构造。
	nestMod := kitnest.NewModWithEntitySync(EntityAccess, PlayerSelfSync.EntitySyncSetup())
	access := accessplayer.NewMod()
	tcp := accessplayertcp.NewMod().WithSelfSync(PlayerSelfSync)
	all := []app.Mod{nestMod, access, tcp}
	for _, mod := range all {
		if err := mod.Init(cfg); err != nil {
			t.Fatalf("%s init: %v", mod.Name(), err)
		}
	}
	for _, mod := range all {
		if provider, ok := mod.(interface{ Provide(*app.Registry) error }); ok {
			if err := provider.Provide(reg); err != nil {
				t.Fatalf("%s provide: %v", mod.Name(), err)
			}
		}
	}
	for _, mod := range all {
		if err := mod.Start(); err != nil {
			t.Fatalf("%s start: %v", mod.Name(), err)
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = tcp.StopWithContext(ctx)
		_ = nestMod.StopWithContext(ctx)
	})

	first := dialSelf(t, addr, 42)
	enter, _ := pb.MarshalEnterGameRequest(&pb.EnterGameRequest{})
	first.request(t, msgid.MsgEnterGame, enter)
	first.expect(t, "full gold=0")
	add, _ := pb.MarshalAddGoldRequest(&pb.AddGoldRequest{Amount: 5})
	first.request(t, msgid.MsgAddGold, add)
	first.expect(t, "delta gold=5")
	t.Logf("first connection: %v", first.log)

	_ = first.conn.Close()
	again := dialSelf(t, addr, 42)
	again.expect(t, "full gold=5")
	add, _ = pb.MarshalAddGoldRequest(&pb.AddGoldRequest{Amount: 2})
	again.request(t, msgid.MsgAddGold, add)
	again.expect(t, "delta gold=7")
	t.Logf("reconnected: %v", again.log)
}

// 部署把 player_access.tcp.max_payload_bytes 调小、别的不改：生成的 bootstrap 照常通过启动前的配置检查（Nest Mod
// 并入的 self_sync 声明与生成的 TCP Mod 声明逐字相同，--check-config 走同一处），Nest Mod Init 之后自身视图一帧的
// 上限就是这个值——超过接入层上限的帧不会推出去、连接不会因此被关而反复重连。独立 Gate 部署（生成的
// IngressMod.WithSelfSync）取 Game Ingress 的载荷上限。
func TestSelfSyncFrameLimitFollowsTheAccessLimit(t *testing.T) {
	cfg := viper.New()
	cfg.SetConfigFile("../../configs/service/config.game.yaml")
	if err := cfg.ReadInConfig(); err != nil {
		t.Fatal(err)
	}
	cfg.Set("server_type", "game") // 进程启动时由命令行给出
	cfg.Set("player_access.tcp.max_payload_bytes", 65536)
	generated, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if err := generated.CheckServiceConfig("game", cfg); err != nil {
		t.Fatalf("config check of the generated game service: %v", err)
	}
	embedded := selfsync.MustNew(selfsync.Config{Entities: EntityAccess, Kind: player.EntityKindPlayer, MessageID: msgid.MsgSelfSync})
	nestMod := kitnest.NewModWithEntitySync(EntityAccess, embedded.EntitySyncSetup())
	if err := nestMod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nestMod.EntitySync().Close(context.Background()) })
	if got := embedded.FrameLimit(); got != 65536 {
		t.Fatalf("embedded TCP: self_sync frame limit = %d, want player_access.tcp.max_payload_bytes 65536", got)
	}

	gate := selfsync.MustNew(selfsync.Config{Entities: EntityAccess, Kind: player.EntityKindPlayer, MessageID: msgid.MsgSelfSync})
	options := gatewiring.DefaultOptions()
	ingress := accessplayertcp.NewIngressMod(options).WithSelfSync(gate)
	cfg.Set("player_access.tcp.max_payload_bytes", 1<<20)
	cfg.Set("player_access.tcp.enabled", false)
	gateNest := kitnest.NewModWithEntitySync(EntityAccess, gate.EntitySyncSetup())
	if err := gateNest.Init(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gateNest.EntitySync().Close(context.Background()) })
	if got := gate.FrameLimit(); got != options.Config.MaxPayloadBytes {
		t.Fatalf("independent Gate: self_sync frame limit = %d, want the Game Ingress payload limit %d", got, options.Config.MaxPayloadBytes)
	}
	if err := ingress.Init(cfg); err != nil {
		t.Fatalf("ingress init: %v", err)
	}
}
`
