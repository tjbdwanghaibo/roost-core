package errcode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractDefinitionsAndWriteCSV(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "game", "gameplay", "shop")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "errors.go"), []byte(`package shop

import "github.com/tjbdwanghaibo/roost-core/errcode"

var (
	ErrShopItemNotFound = errcode.Define(500101, "shop.item_not_found", "商店商品不存在")
	ErrShopLimitReached = errcode.Define(500102, "shop.limit_reached", "商店限购次数不足")
)
`), 0o644); err != nil {
		t.Fatal(err)
	}

	defs, err := extractDefinitions(root)
	if err != nil {
		t.Fatalf("extractDefinitions: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("len(defs) = %d, want 2", len(defs))
	}
	if defs[0].Code != 500101 || defs[0].Name != "shop.item_not_found" || defs[0].Message != "商店商品不存在" {
		t.Fatalf("first definition mismatch: %+v", defs[0])
	}

	out := filepath.Join(root, "errcode.csv")
	if err := writeCSV(out, defs); err != nil {
		t.Fatalf("writeCSV: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{"Code,Name,Message,File", "500101,shop.item_not_found,商店商品不存在", "500102,shop.limit_reached,商店限购次数不足"} {
		if !strings.Contains(text, want) {
			t.Fatalf("csv missing %q:\n%s", want, text)
		}
	}
}
