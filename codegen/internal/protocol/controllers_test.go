package protocol

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestControllerScaffoldsPreserveBusinessAndFollowMethods(t *testing.T) {
	root := t.TempDir()
	defs := &Definitions{ModulePath: "example.com/game", Messages: []MsgDef{{Name: "Equip", Req: "EquipRequest", Resp: "EquipResponse", Handler: "player"}}}
	if err := generateControllers(root, defs, io.Discard); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "player", "equip.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "HandleEquip(ctx *player_agent.Context, req *pb.EquipRequest) (*pb.EquipResponse, error)") {
		t.Fatalf("wrong signature: %s", raw)
	}
	business := strings.ReplaceAll(string(raw), `fmt.Errorf("HandleEquip: not implemented")`, `fmt.Errorf("business implementation")`)
	if err := os.WriteFile(path, []byte(business), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, filepath.Join(root, "player", "business.go")); err != nil {
		t.Fatal(err)
	}
	defs.Messages = append(defs.Messages, MsgDef{Name: "Move", Req: "MoveRequest", Handler: "player"})
	if err := generateControllers(root, defs, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("moved method recreated: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(root, "player", "business.go"))
	if string(after) != business {
		t.Fatal("business overwritten")
	}
	raw, err = os.ReadFile(filepath.Join(root, "player", "move.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "HandleMove(ctx *player_agent.Context, req *pb.MoveRequest) error") {
		t.Fatalf("wrong one-way signature: %s", raw)
	}
	if err := generateControllers(root, defs, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestControllerScaffoldRefusesFilenameConflict(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "player")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "equip.go")
	original := []byte("package player\n// existing business\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	defs := &Definitions{ModulePath: "example.com/game", Messages: []MsgDef{{Name: "Equip", Req: "EquipRequest", Handler: "player"}}}
	if err := generateControllers(root, defs, io.Discard); err == nil {
		t.Fatal("expected conflict")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(original) {
		t.Fatal("overwritten")
	}
	if _, err := os.Stat(filepath.Join(dir, "controller.go")); !os.IsNotExist(err) {
		t.Fatal("partial scaffold written")
	}
}
