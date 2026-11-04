package roost

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ownedEntityProject 生成 game + alliance 两个带 Nest 的服务，加实体 Guild 与一个锁它的 handler。
func ownedEntityProject(t *testing.T) string {
	t.Helper()
	_, root, err := NewProject(NewOptions{
		Name: "planet", Module: "example.com/planet", Out: filepath.Join(t.TempDir(), "planet"),
		Services: []string{"game", "alliance"}, Mods: []string{"configdata", "nest"}, Features: []string{"entity", "dao", "nest"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, options := range []AddOptions{
		{Kind: "entity", Name: "Guild"},
		{Kind: "component", Name: "Roster", Entity: "Guild"},
		{Kind: "handler", Name: "JoinGuild", Entity: "Guild", Component: "Roster"},
	} {
		if _, err := Add(root, options); err != nil {
			t.Fatalf("add %s: %v", options.Kind, err)
		}
	}
	return root
}

// declareOwner 在 Guild 的实体标记上写 service=<service>。
func declareOwner(t *testing.T, root, service string) {
	t.Helper()
	path := filepath.Join(root, "game", "entities", "guild", "entity.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	marked := strings.Replace(string(raw), "category=entity.EntityCategoryOther", "category=entity.EntityCategoryOther service="+service, 1)
	if marked == string(raw) {
		t.Fatalf("entity marker shape changed; update this test:\n%s", raw)
	}
	if err := os.WriteFile(path, []byte(marked), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 归属服务名必须是 roost.yaml 声明过的服务：写错时生成期就报出实体与服务名，而不是运行期才没人能写。
func TestGenerateRejectsAnEntityOwnedByAnUndeclaredService(t *testing.T) {
	t.Parallel()
	root := ownedEntityProject(t)
	declareOwner(t, root, "aliance")
	err := Generate(root, GenerateOptions{Stdout: io.Discard})
	if err == nil {
		t.Fatal("generate accepted service=aliance, which roost.yaml does not declare")
	}
	for _, want := range []string{"Guild", "service=aliance", "alliance, game"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
}

// 声明归属后：只有归属服务的 bootstrap 装归属 handler 的 Mod；bootstrap 还没刷新时 generate 拒绝并给出命令；
// add lifecycle 缺省落到归属服务、拒绝别的服务；doctor 指出别的服务用了归属 kind 的 Sender。工程能编译。
func TestEntityOwnerScopesHandlersLifecycleAndDoctor(t *testing.T) {
	t.Parallel()
	root := ownedEntityProject(t)
	declareOwner(t, root, "alliance")

	err := Generate(root, GenerateOptions{Stdout: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "roost project sync") {
		t.Fatalf("generate with a stale bootstrap must ask for project sync, got %v", err)
	}
	var out bytes.Buffer
	if err := Run([]string{"project", "sync", "--root", root}, &out, &out); err != nil {
		t.Fatalf("project sync: %v\n%s", err, out.String())
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatalf("generate after sync: %v", err)
	}
	bootstrap := readProjectFile(t, root, "internal/bootstrap/generated.go")
	alliance, game, found := strings.Cut(bootstrap, `app.ServiceName("game")`)
	if !found {
		t.Fatalf("bootstrap has no game service:\n%s", bootstrap)
	}
	if !strings.Contains(alliance, `registry.NewOwnedNestHandlersMod("alliance")`) || strings.Contains(game, "NewOwnedNestHandlersMod") {
		t.Fatalf("only the owning service may assemble the owned handler Mod:\n%s", bootstrap)
	}
	if aggregate := readProjectFile(t, root, "internal/registry/nest_gen.go"); strings.Contains(aggregate, "JoinGuild") {
		t.Fatalf("an owned kind's handler must not be registered in every process:\n%s", aggregate)
	}

	if _, err := Add(root, AddOptions{Kind: "lifecycle", Name: "Guild", Service: "game"}); err == nil || !strings.Contains(err.Error(), "service=alliance") {
		t.Fatalf("lifecycle in a non-owning service must be refused, got %v", err)
	}
	if _, err := Add(root, AddOptions{Kind: "lifecycle", Name: "Guild"}); err != nil {
		t.Fatalf("lifecycle must default to the owning service: %v", err)
	}

	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if items := checkEntityOwnerUse(root, m); len(items) != 1 || items[0].Status != StatusOK {
		t.Fatalf("entity-owner before a cross-service sender use = %+v; want one OK", items)
	}
	caller := "package game\n\nimport syncsender \"example.com/planet/game/handler/syncsender\"\n\nvar _ = syncsender.NewJoinGuildSender\n"
	if err := os.WriteFile(filepath.Join(root, "internal", "service", "game", "guild_caller.go"), []byte(caller), 0o644); err != nil {
		t.Fatal(err)
	}
	items := checkEntityOwnerUse(root, m)
	if len(items) != 1 || items[0].Status != StatusFail || !strings.Contains(items[0].Detail, "internal/service/game/guild_caller.go") || !strings.Contains(items[0].Detail, "alliance") {
		t.Fatalf("entity-owner after game uses the Guild sender = %+v; want one FAIL naming the file and the owner", items)
	}
	if err := os.Remove(filepath.Join(root, "internal", "service", "game", "guild_caller.go")); err != nil {
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
			t.Fatalf("go %s with an owned entity: %v\n%s", action, err, output)
		}
	}
}

// remote=managed 的归属 kind（阶段 1 接线）：每个装 remote_entity 的服务把别的服务拥有的 Remote kind 交给
// RemoteEntityMod（WithForeignKinds），认领与写许可前拒绝；ForeignKinds 只列 Remote 托管 kind（RemoteEntityMod
// 的 Init 要求它们是已注册的 Remote 托管 kind）。生成工程能编译。
func TestOwnedRemoteKindIsForeignToEveryOtherRemoteEntityService(t *testing.T) {
	t.Parallel()
	_, root, err := NewProject(NewOptions{
		Name: "planet", Module: "example.com/planet", Out: filepath.Join(t.TempDir(), "planet"),
		Services: []string{"game", "alliance"}, Mods: []string{"configdata", "nest", "remote_entity"}, Features: []string{"entity", "dao", "nest"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, options := range []AddOptions{{Kind: "entity", Name: "Guild"}, {Kind: "entity", Name: "Shard"}} {
		if _, err := Add(root, options); err != nil {
			t.Fatalf("add %s: %v", options.Name, err)
		}
	}
	guild := filepath.Join(root, "game", "entities", "guild", "entity.go")
	raw, err := os.ReadFile(guild)
	if err != nil {
		t.Fatal(err)
	}
	remote := strings.Replace(string(raw), "category=entity.EntityCategoryOther", "category=entity.EntityCategoryRemote remote=managed service=alliance", 1)
	remote = strings.Replace(remote, "*entity.EntityBase", "*entity.RemoteEntityBase", 1)
	if err := os.WriteFile(guild, []byte(remote), 0o644); err != nil {
		t.Fatal(err)
	}
	// 本地 kind 也声明归属：只影响 handler 注册，不进 ForeignKinds。
	shard := filepath.Join(root, "game", "entities", "shard", "entity.go")
	if raw, err = os.ReadFile(shard); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shard, []byte(strings.Replace(string(raw), "category=entity.EntityCategoryOther", "category=entity.EntityCategoryOther service=alliance", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run([]string{"project", "sync", "--root", root}, &out, &out); err != nil {
		t.Fatalf("project sync: %v\n%s", err, out.String())
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	bootstrap := readProjectFile(t, root, "internal/bootstrap/generated.go")
	for _, service := range []string{"game", "alliance"} {
		if want := `kitremoteentity.WithForeignKinds(registry.ForeignKinds("` + service + `")...)`; !strings.Contains(bootstrap, want) {
			t.Errorf("bootstrap lacks %s:\n%s", want, bootstrap)
		}
	}
	ownership := readProjectFile(t, root, "internal/registry/ownership_gen.go")
	if !strings.Contains(ownership, "EntityKindGuild: \"alliance\"") || regexpLocalKindInRemoteOwners(ownership) {
		t.Errorf("ForeignKinds must list only the Remote kind:\n%s", ownership)
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
			t.Fatalf("go %s with an owned Remote kind: %v\n%s", action, err, output)
		}
	}
}

// regexpLocalKindInRemoteOwners 报告 remoteKindOwners 表里出现了本地 kind（Shard）。
func regexpLocalKindInRemoteOwners(source string) bool {
	_, table, found := strings.Cut(source, "remoteKindOwners = map[entity.EntityKind]string{")
	if !found {
		return false
	}
	table, _, _ = strings.Cut(table, "}")
	return strings.Contains(table, "EntityKindShard")
}
