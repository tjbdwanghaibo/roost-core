package roost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// B5 · roost add rpc：一个工程自己的跨进程服务，从接口到装配一条命令。拥有者进程装配 owner Mod，
// 调用者（roost.yaml uses_rpcs）装配生成的 ClientMod；生成的两半随 make generate 重生成；
// ErrRequestInvalid 的码来自清单的 errcode 号段。
func TestAddRPCScaffoldsOwnerAndWiresCaller(t *testing.T) {
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
