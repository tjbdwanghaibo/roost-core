package entity

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// W-2026-09-18-09（选 A）· `//roost:dao nocoll`：无集合 DAO 没有 `<Dao>Collection` 常量，
// 只有 DaoManager 登记键 `<Dao>RegistryKey`。Entity 生成器读 DAO 包的生成源码认出它：
// noPersist 实体的接线改用登记键；持久实体或 remote=managed 实体使用它是配置错误，生成期
// 拒绝并点名，不落 wire 文件（docs/feature/DAO-NO-COLLECTION-2026-10-04.md R4）。

const noCollectionDaoSource = "package db\n\ntype GhostDao struct{}\n\nconst GhostDaoRegistryKey = \"GhostDao\"\n"
const collectionDaoSource = "package db\n\ntype GhostDao struct{}\n\nconst (\n\tGhostDaoDBName     = \"game\"\n\tGhostDaoCollection = \"ghosts\"\n)\n"

func ghostEntitySource(markerTail string, base string) string {
	return "package ghost\n\nimport (\n\t\"example.com/game/db\"\n\t\"github.com/tjbdwanghaibo/roost-core/framework/entity\"\n)\n\n" +
		"const EntityKindGhost entity.EntityKind = 6\n\n" +
		"//roost:entity entityKind=EntityKindGhost " + markerTail + "\n" +
		"type Ghost struct {\n\t" + base + "\n\tentity.DaoManager\n\tdao *db.GhostDao `dao:\"ghost\"`\n}\n"
}

func generateGhost(t *testing.T, daoSource, entitySource string) (string, error) {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod":                       "module example.com/game\n\ngo 1.27.0\n",
		"db/gen_ghost_dao.go":          daoSource,
		"game/entities/ghost/ghost.go": entitySource,
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	err := Run([]string{"-dir", filepath.Join(root, "game")}, io.Discard)
	wire, readErr := os.ReadFile(filepath.Join(root, "game", "entities", "ghost", "ghost_gen_wire.go"))
	if err == nil && readErr != nil {
		t.Fatalf("generation succeeded without a wire file: %v", readErr)
	}
	if err != nil && readErr == nil {
		t.Fatal("a refused generation still wrote the wire file")
	}
	return string(wire), err
}

func TestANoPersistEntityWiresANoCollectionDaoByItsRegistryKey(t *testing.T) {
	wire, err := generateGhost(t, noCollectionDaoSource, ghostEntitySource("noPersist=true lifetime=ephemeral sync=true", "*entity.EntityBase"))
	if err != nil {
		t.Fatalf("noPersist entity with a nocoll DAO rejected: %v", err)
	}
	for _, want := range []string{
		"param.Dao[db.GhostDaoRegistryKey]",
		"e.DaoManager.Set(db.GhostDaoRegistryKey, e.dao)",
		"entity.MapDAOSyncChanges(e, db.GhostDaoRegistryKey,",
	} {
		if !strings.Contains(wire, want) {
			t.Errorf("wire is missing %q", want)
		}
	}
	if strings.Contains(wire, "GhostDaoCollection") {
		t.Error("wire still references the collection constant a nocoll DAO does not have")
	}
}

// 有集合 DAO 的接线不变。
func TestACollectionDaoIsStillWiredByItsCollection(t *testing.T) {
	wire, err := generateGhost(t, collectionDaoSource, ghostEntitySource("noPersist=true lifetime=ephemeral", "*entity.EntityBase"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wire, "param.Dao[db.GhostDaoCollection]") || strings.Contains(wire, "RegistryKey") {
		t.Fatalf("collection DAO wiring changed:\n%s", wire)
	}
}

func TestAStoredEntityCannotUseANoCollectionDao(t *testing.T) {
	cases := []struct {
		name   string
		marker string
		base   string
		want   []string
	}{
		{"persistent", "", "*entity.EntityBase", []string{"Ghost", "dao", "GhostDao", "nocoll", "noPersist=true"}},
		{"remote managed", "noPersist=true remote=managed", "*entity.RemoteEntityBase", []string{"Ghost", "GhostDao", "nocoll", "remote=managed"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := generateGhost(t, noCollectionDaoSource, ghostEntitySource(tc.marker, tc.base))
			if err == nil {
				t.Fatal("a stored entity with a nocoll DAO was generated")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}
