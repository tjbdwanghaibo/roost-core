package dao

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// game-demo 第十四批：一个工程必须能声明自己 DAO 的 schema 版本。
//
// 旧行为：生成的 `<Dao>SchemaVersion` 永远是 1。框架有一整套迁移机制
// （`migration.RegisterDAO` + 生成的 `Migrate` 调 `MigrateDAO(coll, raw,
// from, SchemaVersion)`），但 from 与 target 恒等，**没有任何生成的工程能
// 触发它** —— 一个承诺了却无人能履行的能力。
func TestDaoSchemaVersionComesFromTheMarker(t *testing.T) {
	dir := t.TempDir()
	source := `package def

//roost:dao coll=players db=game schema=3
type PlayerDao struct {
	Name string
}
`
	if err := os.WriteFile(filepath.Join(dir, "player.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := Run([]string{"-def", dir, "-out", out, "-pkg", "db"}, os.Stderr); err != nil {
		t.Fatalf("generate: %v", err)
	}
	generated, err := os.ReadFile(filepath.Join(out, "gen_player_dao.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(generated), "PlayerDaoSchemaVersion uint32 = 3") {
		t.Errorf("the declared schema version did not reach the generated code:\n%s", generated)
	}
}

// Omitting it keeps the version at 1, so no existing definition changes
// meaning.
func TestDaoSchemaVersionDefaultsToOne(t *testing.T) {
	dir := t.TempDir()
	source := `package def

//roost:dao coll=players db=game
type PlayerDao struct {
	Name string
}
`
	if err := os.WriteFile(filepath.Join(dir, "player.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := Run([]string{"-def", dir, "-out", out, "-pkg", "db"}, os.Stderr); err != nil {
		t.Fatalf("generate: %v", err)
	}
	generated, _ := os.ReadFile(filepath.Join(out, "gen_player_dao.go"))
	if !strings.Contains(string(generated), "PlayerDaoSchemaVersion uint32 = 1") {
		t.Errorf("a definition without schema= did not default to 1:\n%s", generated)
	}
}

// A version that cannot be a schema version is refused at parse time: zero
// would make every stored document look newer than the code, and a
// non-number is a typo that would otherwise be silently ignored.
func TestDaoSchemaVersionRefusesNonsense(t *testing.T) {
	for name, value := range map[string]string{"zero": "0", "negative": "-1", "text": "two"} {
		dir := t.TempDir()
		source := "package def\n\n//roost:dao coll=players db=game schema=" + value + "\ntype PlayerDao struct{ Name string }\n"
		if err := os.WriteFile(filepath.Join(dir, "player.go"), []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := Run([]string{"-def", dir, "-out", t.TempDir(), "-pkg", "db"}, os.Stderr); err == nil {
			t.Errorf("%s: schema=%s was accepted", name, value)
		}
	}
}
