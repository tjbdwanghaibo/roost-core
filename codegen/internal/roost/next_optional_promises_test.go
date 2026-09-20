package roost

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// D16 · project next 的进阶引导：必做链完成后，列出框架有、工程还没用的能力（rpc / saga / attribute /
// skill / webroute / cfggen），每条一个命令一个理由；用了的不再提示。
func TestOptionalHintsNameWhatTheProjectHasNotUsedYet(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Mods: []string{"configdata"}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	joined := func(hints []optionalHint) string {
		var b strings.Builder
		for _, hint := range hints {
			b.WriteString(hint.Command + "\n")
		}
		return b.String()
	}
	all := joined(optionalNextHints(root, m))
	for _, want := range []string{"roost add rpc <Name> -service game", "roost add saga <name> -service game", "add attribute to roost.yaml features", "roost add skill <Name>", "add webroute to roost.yaml features", "configs/schema/cfg.yaml"} {
		if !strings.Contains(all, want) {
			t.Errorf("hints lack %q:\n%s", want, all)
		}
	}
	// Once used, a capability stops being suggested.
	game := m.Services["game"]
	game.Rpcs = []string{"guild"}
	m.Services["game"] = game
	m.Features = append(m.Features, "saga", "attribute", "webroute", "rpc")
	if err := os.MkdirAll(filepath.Join(root, "game", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "configs", "schema", "cfg.yaml"), []byte("package: cfg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rest := optionalNextHints(root, m); len(rest) != 0 {
		t.Fatalf("every capability is in use, yet hints remain:\n%s", joined(rest))
	}
}

// C14 · 换行不是内容：Windows 检出把生成文件存成 CRLF，generate --check 不能因此报 stale；
// 生成工程还带一份 .gitattributes 把生成的文本文件钉成 LF。
func TestGenerateCheckToleratesCRLFCheckouts(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Mods: []string{"configdata"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Check: true}); err != nil {
		t.Fatalf("fresh project is stale: %v", err)
	}
	path := filepath.Join(root, "internal", "bootstrap", "generated.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.ReplaceAll(raw, []byte("\n"), []byte("\r\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Check: true}); err != nil {
		t.Fatalf("a CRLF checkout of an unchanged generated file reads as stale: %v", err)
	}
	attributes, err := os.ReadFile(filepath.Join(root, ".gitattributes"))
	if err != nil {
		t.Fatalf(".gitattributes not generated: %v", err)
	}
	if !strings.Contains(string(attributes), "*.go text eol=lf") {
		t.Fatalf(".gitattributes does not pin LF for Go files:\n%s", attributes)
	}
}
