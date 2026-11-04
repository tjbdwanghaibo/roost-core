package nest

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ownedProject 写一个最小工程：Guild 声明 service=alliance，Player 未声明归属；各有一个 handler 文件。
func ownedProject(t *testing.T, guildMarker string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.com/owned\n\ngo 1.27.0\n",
		"game/entities/guild/entity.go": `package guild

import "github.com/tjbdwanghaibo/roost-core/framework/entity"

const EntityKindGuild entity.EntityKind = 1

type IGuildEntity interface{ entity.IThreadSafeEntity }

` + guildMarker + `
type Guild struct {
	*entity.RemoteEntityBase
}
`,
		"game/entities/player/entity.go": `package player

import "github.com/tjbdwanghaibo/roost-core/framework/entity"

const EntityKindPlayer entity.EntityKind = 2

type IPlayerEntity interface{ entity.IThreadSafeEntity }

//roost:entity id=2 entityKind=EntityKindPlayer category=entity.EntityCategoryPlayer
type Player struct {
	*entity.EntityBase
}
`,
		"game/handler/join_guild.go": `package handler

import (
	guild "example.com/owned/game/entities/guild"
	player "example.com/owned/game/entities/player"
)

//roost:nest rollback=undo durability=strict
func handlerJoinGuild(target guild.IGuildEntity, member player.IPlayerEntity) error { return nil }
`,
		"game/handler/rename.go": `package handler

import player "example.com/owned/game/entities/player"

//roost:nest rollback=undo durability=strict
func handlerRename(target player.IPlayerEntity, name string) error { return nil }
`,
	}
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func readOwned(t *testing.T, root, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

// 声明了 service= 的 kind 的 handler 只在归属服务的引擎实例上注册：不进包级 RegisterNestHandlers，
// 改由 ownership_gen.go 的 Mod 在 alliance 进程的 Provide 里注册；未声明归属的 handler 照旧全局注册。
func TestOwnedKindHandlersRegisterOnlyOnTheOwningService(t *testing.T) {
	root := ownedProject(t, "//roost:entity id=1 entityKind=EntityKindGuild category=entity.EntityCategoryRemote remote=managed service=alliance")
	if err := Run([]string{"-dir", filepath.Join(root, "game")}, io.Discard); err != nil {
		t.Fatalf("nest: %v", err)
	}
	aggregate := readOwned(t, root, "internal/registry/nest_gen.go")
	if !strings.Contains(aggregate, "gamehandler.RegisterRenameNestHandlers()") {
		t.Fatalf("unowned handler must stay in the package-level aggregate:\n%s", aggregate)
	}
	if strings.Contains(aggregate, "RegisterJoinGuild") {
		t.Fatalf("handler of an owned kind must not be registered in every process:\n%s", aggregate)
	}
	ownership := readOwned(t, root, "internal/registry/ownership_gen.go")
	for _, want := range []string{
		`"alliance": {gamehandler.RegisterJoinGuildNestHandlersOn}`,
		"func NewOwnedNestHandlersMod(service string) app.Mod",
		"func ForeignKinds(service string) []entity.EntityKind",
		`guild.EntityKindGuild: "alliance"`,
	} {
		if !strings.Contains(ownership, want) {
			t.Fatalf("ownership_gen.go missing %q:\n%s", want, ownership)
		}
	}
	wrapper := readOwned(t, root, "game/handler/join_guild_nest_gen.go")
	if !strings.Contains(wrapper, "func RegisterJoinGuildNestHandlersOn(mgr *nest.NestMgr) error") ||
		strings.Contains(wrapper, "func RegisterJoinGuildNestHandlers()") {
		t.Fatalf("owned handler file must register on an engine instance only:\n%s", wrapper)
	}

	// 去掉 service= 之后回到原行为：全局注册，ownership_gen.go 被删除。
	entityFile := filepath.Join(root, "game", "entities", "guild", "entity.go")
	raw := strings.Replace(readOwned(t, root, "game/entities/guild/entity.go"), " service=alliance", "", 1)
	if err := os.WriteFile(entityFile, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"-dir", filepath.Join(root, "game")}, io.Discard); err != nil {
		t.Fatalf("nest after removing service=: %v", err)
	}
	if aggregate := readOwned(t, root, "internal/registry/nest_gen.go"); !strings.Contains(aggregate, "gamehandler.RegisterJoinGuildNestHandlers()") {
		t.Fatalf("undeclared kind must keep registering everywhere:\n%s", aggregate)
	}
	if _, err := os.Stat(filepath.Join(root, "internal", "registry", "ownership_gen.go")); !os.IsNotExist(err) {
		t.Fatalf("ownership_gen.go must be removed once no kind declares an owner (stat err %v)", err)
	}
}

// 一个 handler 锁住两个归属不同服务的 kind：没有任何进程能执行它，生成期拒绝。
func TestHandlerAcrossTwoOwningServicesIsRejected(t *testing.T) {
	root := ownedProject(t, "//roost:entity id=1 entityKind=EntityKindGuild category=entity.EntityCategoryRemote remote=managed service=alliance")
	playerFile := filepath.Join(root, "game", "entities", "player", "entity.go")
	raw := strings.Replace(readOwned(t, root, "game/entities/player/entity.go"), "category=entity.EntityCategoryPlayer", "category=entity.EntityCategoryPlayer service=game", 1)
	if err := os.WriteFile(playerFile, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Run([]string{"-dir", filepath.Join(root, "game")}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "handlerJoinGuild") || !strings.Contains(err.Error(), "alliance") || !strings.Contains(err.Error(), "game") {
		t.Fatalf("handler over kinds owned by two services must be refused with both names, got %v", err)
	}
}

// 本服务登记路由的 kind 与起服预加载的 kind（联盟服务方案 §2.7、§2.10 第 2 项）来自同一次归属扫描：RouteKinds 是
// 本服务拥有的 Remote 托管 kind，PreloadKinds 是其中声明了 preload=startup 的。没写 preload= 的 kind 不预加载。
func TestRouteAndPreloadKindsComeFromTheOwnershipScan(t *testing.T) {
	root := ownedProject(t, "//roost:entity id=1 entityKind=EntityKindGuild category=entity.EntityCategoryRemote remote=managed service=alliance preload=startup")
	if err := Run([]string{"-dir", filepath.Join(root, "game")}, io.Discard); err != nil {
		t.Fatalf("nest: %v", err)
	}
	ownership := readOwned(t, root, "internal/registry/ownership_gen.go")
	_, preload, found := strings.Cut(ownership, "var startupPreloadKinds = map[entity.EntityKind]bool{")
	preload, _, _ = strings.Cut(preload, "}")
	if !found || !strings.Contains(preload, "guild.EntityKindGuild: true") ||
		!strings.Contains(ownership, "func RouteKinds(service string) []entity.EntityKind") ||
		!strings.Contains(ownership, "func PreloadKinds(service string) []entity.EntityKind") {
		t.Fatalf("ownership_gen.go must list Guild as a startup preload kind and give RouteKinds / PreloadKinds:\n%s", ownership)
	}

	entityFile := filepath.Join(root, "game", "entities", "guild", "entity.go")
	raw := strings.Replace(readOwned(t, root, "game/entities/guild/entity.go"), " preload=startup", "", 1)
	if err := os.WriteFile(entityFile, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"-dir", filepath.Join(root, "game")}, io.Discard); err != nil {
		t.Fatalf("nest without preload=: %v", err)
	}
	ownership = readOwned(t, root, "internal/registry/ownership_gen.go")
	_, preload, _ = strings.Cut(ownership, "var startupPreloadKinds = map[entity.EntityKind]bool{")
	preload, _, _ = strings.Cut(preload, "}")
	if strings.Contains(preload, "EntityKindGuild") || !strings.Contains(ownership, `guild.EntityKindGuild: "alliance"`) {
		t.Fatalf("a kind without preload= must still route but not preload:\n%s", ownership)
	}
}

// preload=startup 只能写在 remote=managed 且声明了 service= 的实体上：预加载的范围是“持久所有权记录的 owner 是本 sid
// 的实体”，只有归属某个服务的 Remote 托管 kind 有。取值只有 startup。
func TestPreloadIsRefusedOutsideAnOwnedRemoteEntity(t *testing.T) {
	for name, tc := range map[string]struct{ guild, player, want string }{
		"local entity without service": {
			guild:  "//roost:entity id=1 entityKind=EntityKindGuild category=entity.EntityCategoryRemote remote=managed service=alliance",
			player: "category=entity.EntityCategoryPlayer preload=startup",
			want:   "preload=startup needs remote=managed and service=",
		},
		"local entity with service": {
			guild:  "//roost:entity id=1 entityKind=EntityKindGuild category=entity.EntityCategoryRemote remote=managed service=alliance",
			player: "category=entity.EntityCategoryPlayer service=game preload=startup",
			want:   "preload=startup needs remote=managed and service=",
		},
		"remote entity without service": {
			guild: "//roost:entity id=1 entityKind=EntityKindGuild category=entity.EntityCategoryRemote remote=managed preload=startup",
			want:  "preload=startup needs remote=managed and service=",
		},
		"unknown policy": {
			guild: "//roost:entity id=1 entityKind=EntityKindGuild category=entity.EntityCategoryRemote remote=managed service=alliance preload=lazy",
			want:  `preload="lazy" is not a preload policy`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := ownedProject(t, tc.guild)
			if tc.player != "" {
				playerFile := filepath.Join(root, "game", "entities", "player", "entity.go")
				raw := strings.Replace(readOwned(t, root, "game/entities/player/entity.go"), "category=entity.EntityCategoryPlayer", tc.player, 1)
				if err := os.WriteFile(playerFile, []byte(raw), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			err := Run([]string{"-dir", filepath.Join(root, "game")}, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}
