package roost

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 可再生成的部分跟随 handler；手写入口移动文件、加授权逻辑后仍保持原样。
func TestEndpointRegenerationPreservesApplication(t *testing.T) {
	root := t.TempDir()
	manifest := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"configdata"}, nil)
	manifest.Access = map[string]AccessSpec{"player": {Service: "game"}}
	writeProjectFile(t, root, "protocol/def/grant.go", "package def\ntype GrantRequest struct { Amount int64; Count int64 }; type GrantResponse struct { Code int32; Reason string }\n")
	handler := `package handler
import player "example.com/planet/game/entities/player"
//roost:nest rollback=undo
func handlerGrant(target player.IPlayerEntity, amount int64) error { return nil }
`
	writeProjectFile(t, root, "game/handler/grant.go", handler)
	if _, err := addProtocolEndpoint(root, manifest, AddOptions{Name: "Grant", Handler: "player"}); err != nil {
		t.Fatal(err)
	}
	rel := "game/controllers/player/grant.go"
	wrapper := readProjectFile(t, root, rel)
	if !strings.Contains(wrapper, "//roost:endpoint nest=Grant") {
		t.Fatal(wrapper)
	}
	wrapper = strings.Replace(wrapper, "return controller.dispatchGrant", "// Custom authorization stays here.\n return controller.dispatchGrant", 1)
	writeProjectFile(t, root, "game/controllers/player/grants.go", wrapper)
	if err := os.Remove(filepath.Join(root, rel)); err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, root, "game/handler/grant.go", strings.Replace(handler, "amount int64", "count int64", 1))
	if err := generateEndpoints(root, manifest, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := readProjectFile(t, root, "game/controllers/player/grants.go"); got != wrapper {
		t.Fatal("application wrapper changed")
	}
	generated := "game/controllers/player/grant_endpoint_gen.go"
	body := readProjectFile(t, root, generated)
	if !strings.Contains(body, "entityID, request.Count") || strings.Contains(body, "request.Amount") {
		t.Fatal(body)
	}
	if err := generateEndpoints(root, manifest, io.Discard); err != nil {
		t.Fatal(err)
	}
	if readProjectFile(t, root, generated) != body {
		t.Fatal("generation drift")
	}
	// 不认领同名的业务文件。
	writeProjectFile(t, root, generated, "package player\n// application-owned\n")
	if err := generateEndpoints(root, manifest, io.Discard); err == nil || !strings.Contains(err.Error(), "overwrite application") {
		t.Fatalf("collision: %v", err)
	}
	writeProjectFile(t, root, generated, body)
	// 声明撤销时只删自动部分；手写文件由业务自行决定下一步。
	writeProjectFile(t, root, "game/controllers/player/grants.go", strings.Replace(wrapper, "//roost:endpoint nest=Grant\n", "", 1))
	if err := generateEndpoints(root, manifest, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, generated)); !os.IsNotExist(err) {
		t.Fatalf("orphan remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "game/controllers/player/grants.go")); err != nil {
		t.Fatal(err)
	}
}

func TestEndpointMarkerRefusesInvalidDeclarations(t *testing.T) {
	for _, tc := range []struct{ name, source, want string }{
		{"missing", "//roost:endpoint\nfunc (c *Controller) HandleGrant() {}", "requires nest="},
		{"orphan", "//roost:endpoint nest=Grant\nvar unrelated int", "not attached"},
		{"function", "//roost:endpoint nest=Grant\nfunc HandleGrant() {}", "Controller Handle"},
		{"receiver", "//roost:endpoint nest=Grant\nfunc (c *Other) HandleGrant() {}", "*Controller"},
		{"typo", "//roost:endpoint nesst=Grant\nfunc (c *Controller) HandleGrant() {}", "unknown marker option"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeProjectFile(t, root, "game/controllers/player/route.go", "package player\n"+tc.source+"\n")
			err := generateEndpoints(root, Manifest{}, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
}
