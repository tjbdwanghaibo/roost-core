package dao

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// W-2026-09-18-09（维护者 2026-10-04 选 A）：一个全 `nopersist,sync` 的 DAO 是无持久化
// 实体（刷出来的怪）的位置权威，但它不该为了能生成而编造一个 Mongo 集合名，生成物也不该
// 带着一条永远不会写任何东西的持久化路径。`//roost:dao nocoll` 声明“没有集合”。
// 方案：docs/feature/DAO-NO-COLLECTION-2026-10-04.md。

const ghostDaoSource = `package def

// GhostDao lives only in memory.
//
//roost:dao nocoll
type GhostDao struct {
	PosX  int64            ` + "`bson:\"pos_x\" dao:\"nopersist,sync\"`" + `
	PosY  int64            ` + "`bson:\"pos_y\" dao:\"nopersist,sync\"`" + `
	Buffs map[int32]int64  ` + "`bson:\"buffs\" dao:\"nopersist,sync\"`" + `
	Scratch int64          ` + "`dao:\"nopersist,nosync\"`" + `
	Ignored int64          ` + "`dao:\"-\"`" + `
}
`

func generateNoCollectionSource(t *testing.T, source string) (string, error) {
	t.Helper()
	defDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(defDir, "ghost.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	if err := Run([]string{"-def", defDir, "-out", outDir, "-pkg", "db"}, io.Discard); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(filepath.Join(outDir, "gen_ghost_dao.go"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw), nil
}

// 生成物保留 sync 打包、组件读写与 Nest 内存回滚，不出现集合 / 库名常量和任何 Mongo
// 读写、迁移、加载路径。
func TestANoCollectionDaoKeepsSyncAndDropsEveryStoragePath(t *testing.T) {
	content, err := generateNoCollectionSource(t, ghostDaoSource)
	if err != nil {
		t.Fatalf("a nocoll DAO whose fields are all nopersist must generate: %v", err)
	}
	// gofmt aligns const blocks and runs of one-line methods; compare with
	// whitespace collapsed so the promise is about tokens, not columns.
	flat := strings.Join(strings.Fields(content), " ")
	for _, want := range []string{
		`GhostDaoRegistryKey = "GhostDao"`,
		"func (d *GhostDao) CollName() string { return GhostDaoRegistryKey }",
		`func (d *GhostDao) DbName() string { return "" }`,
		"var _ entity.DaoInterface = (*GhostDao)(nil)",
		"var _ nest.RollbackSnapshotter = (*GhostDao)(nil)",
		"func (d *GhostDao) SetPosX(v int64)",
		"func (d *GhostDao) SetBuffs(key int32, val int64)",
		"func (d *GhostDao) MarshalSync(mask uint64) []byte",
		"func (d *GhostDao) ApplySync(raw []byte) error",
		"func GhostDaoSyncFields() []dataengine.SyncFieldMeta",
		"func (d *GhostDao) CaptureRollbackState() ([]byte, error)",
		"func (d *GhostDao) RestoreRollbackState(raw []byte) error",
		"d.tracker.MarkSync(ghostDaoFieldPosX)",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("nocoll DAO output is missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"Collection",
		"DBName",
		"SchemaVersion",
		"DbScope",
		"Migrate",
		"migration",
		"MutationParticipant",
		"PrepareMutation",
		"AcceptMutation",
		"MarkPersist",
		"MarshalPersist",
		"marshalCommitState",
		"marshalPersistPatchBSON",
		"func (d *GhostDao) Marshal()",
		"func (d *GhostDao) Unmarshal(",
		"RestorePersisted",
		`"_schema"`,
		`"sort"`,
	} {
		if strings.Contains(content, forbidden) {
			t.Errorf("nocoll DAO output still contains storage path %q", forbidden)
		}
	}
}

// R3：有持久字段却没有集合是配置错误，点名全部持久字段——包括没写 dao tag、按默认
// persist,sync 处理的那个。
func TestANoCollectionDaoWithPersistentFieldsIsRefusedByName(t *testing.T) {
	_, err := generateNoCollectionSource(t, `package def

//roost:dao nocoll
type GhostDao struct {
	PosX  int64 `+"`dao:\"nopersist,sync\"`"+`
	HP    int64
	Level int32 `+"`dao:\"persist\"`"+`
	Skip  int64 `+"`dao:\"-\"`"+`
}
`)
	if err == nil {
		t.Fatal("a nocoll DAO with persistent fields generated")
	}
	for _, want := range []string{"GhostDao", "nocoll", "HP", "Level", "nopersist"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must mention %q; got %v", want, err)
		}
	}
	for _, notPersistent := range []string{"PosX", "Skip"} {
		if strings.Contains(err.Error(), notPersistent) {
			t.Errorf("error names %s, which does not persist; got %v", notPersistent, err)
		}
	}
}

// R1 / R2：nocoll 与描述存储的键冲突，带值的 nocoll 不是裸标志。R5：两样都没有时，
// 原有错误保留并提示 nocoll。
func TestTheNoCollectionMarkerRefusesContradictions(t *testing.T) {
	cases := []struct {
		name   string
		marker string
		want   []string
	}{
		{"with coll and db", "//roost:dao nocoll coll=ghosts db=game", []string{"nocoll", "coll=", "db="}},
		{"with dbscope", "//roost:dao nocoll dbscope=sid", []string{"nocoll", "dbscope="}},
		{"with schema", "//roost:dao nocoll schema=2", []string{"nocoll", "schema="}},
		{"with a value", "//roost:dao nocoll=true", []string{"nocoll", "bare flag"}},
		{"neither", "//roost:dao nocol", []string{`unknown marker option "nocol"`, "nocoll"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSource(t, "package def\n\n"+tc.marker+"\ntype GhostDao struct {\n\tPosX int64 `dao:\"nopersist,sync\"`\n}\n")
			if err == nil {
				t.Fatalf("%s parsed", tc.marker)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error must mention %q; got %v", want, err)
				}
			}
		})
	}
}

// 旧写法逐字不变：没有 nocoll 的定义解析出 NoCollection=false，仍有集合与库名。
func TestACollectionDaoIsUnchangedByTheNoCollectionFlag(t *testing.T) {
	defs, err := parseSource(t, "package def\n\n//roost:dao coll=heroes db=game\ntype HeroDao struct {\n\tName string\n}\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := defs.Daos[0]; got.NoCollection || got.Coll != "heroes" || got.Db != "game" {
		t.Fatalf("collection DAO parsed as %+v", got)
	}
}

// 一个 DAO 从有集合改为无集合：同一个生成文件按内容重写，旧的持久化路径随之消失，
// 不留下第二个文件；按现有退役规则，生成头之外的文件不受影响。
func TestSwitchingADaoToNoCollectionRetiresItsStoragePaths(t *testing.T) {
	defDir := t.TempDir()
	outDir := t.TempDir()
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(defDir, "ghost.go"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	generate := func() {
		t.Helper()
		if err := Run([]string{"-def", defDir, "-out", outDir, "-pkg", "db"}, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	write(strings.Replace(ghostDaoSource, "//roost:dao nocoll", "//roost:dao coll=ghosts db=game", 1))
	generate()
	before, err := os.ReadFile(filepath.Join(outDir, "gen_ghost_dao.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(before), "GhostDaoCollection") || !strings.Contains(string(before), `"ghosts"`) || !strings.Contains(string(before), "RestorePersisted") {
		t.Fatal("fixture: the collection form did not generate its storage path")
	}

	write(ghostDaoSource)
	generate()
	after, err := os.ReadFile(filepath.Join(outDir, "gen_ghost_dao.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, retired := range []string{"GhostDaoCollection", "ghosts", "RestorePersisted", "PrepareMutation"} {
		if strings.Contains(string(after), retired) {
			t.Errorf("switching to nocoll left %q in the generated DAO", retired)
		}
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "gen_ghost_dao.go" {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("output after the switch = %v, want only gen_ghost_dao.go", names)
	}
}
