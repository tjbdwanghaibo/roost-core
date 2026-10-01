package roost

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// RR-20261001-03：schema 已写、configs/table 还没有 CSV 的工程是标准脚手架状态，
// `roost generate` 必须像 v1.17.2 一样跳过 config-data 而不是失败。脚手架总会写出
// configs/data/_manifest.json，RR-20260930-09 之后 roost 只看 manifest 是否存在就运行
// tablegen，而 tablegen 对每个 meta 无条件读 CSV，于是报 open configs/table/monster.csv。
func TestGenerateSkipsConfigDataWhenSchemaHasNoCSVYet(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Features: []string{"config"}})
	if err != nil {
		t.Fatal(err)
	}
	schema := filepath.Join(root, "configs", "schema", "monster.go")
	source := "package schema\n//roost:table name=monster file=monster.csv json=monster.json key=ID\ntype Monster struct { ID int32 `csv:\"id\" json:\"id\"` }\n"
	if err := os.WriteFile(schema, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "configs", "table"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatalf("generate with schema but no CSV: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "configs", "data", "monster.json")); !os.IsNotExist(err) {
		t.Fatalf("config-data ran without any CSV: %v", err)
	}
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkGenerated(root, manifest, io.Discard); err != nil {
		t.Fatalf("generate --check with schema but no CSV: %v", err)
	}
}
