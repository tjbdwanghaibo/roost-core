package roost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
