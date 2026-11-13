package dao

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parseSource writes one definition file into a temp dir and parses it.
func parseSource(t *testing.T, source string) (*Definitions, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "def.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	return parseDefDir(dir)
}

func TestParseRejectsOrphanMarker(t *testing.T) {
	// Regression: a marker separated from its struct used to be dropped
	// silently — a DAO that silently does not persist.
	_, err := parseSource(t, `package def

//roost:dao coll=heroes db=game

var unrelated = 1

type HeroDao struct {
	Name string
}
`)
	if err == nil || !strings.Contains(err.Error(), "not attached to a struct") {
		t.Fatalf("err=%v, want orphan marker error", err)
	}
}

func TestParseRejectsMarkerBindingMultipleStructs(t *testing.T) {
	// Regression: one marker above a grouped type declaration used to bind
	// silently to every struct in the group — several types writing the same
	// collection with zero warning.
	_, err := parseSource(t, `package def

//roost:dao coll=heroes db=game
type (
	AlphaDao struct{ Name string `+"`"+`store:"name"`+"`"+` }
	BetaDao  struct{ Name string `+"`"+`store:"name"`+"`"+` }
)
`)
	if err == nil || !strings.Contains(err.Error(), "binds to multiple structs") {
		t.Fatalf("err=%v, want multi-bind error", err)
	}
}

func TestRunSweepsOrphansWhenAllDefinitionsRemoved(t *testing.T) {
	// Regression: deleting the last definition used to return early before
	// the orphan sweep, leaving the final generated files behind forever —
	// and generate --check could not see them either.
	defDir := t.TempDir()
	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(defDir, "def.go"), []byte(`package def

//roost:dao coll=heroes db=game
type HeroDao struct {
	Name string `+"`"+`store:"name"`+"`"+`
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"-def", defDir, "-out", outDir, "-pkg", "out"}, os.Stdout); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defDir, "def.go"), []byte("package def\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"-def", defDir, "-out", outDir, "-pkg", "out"}, os.Stdout); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "gen_hero_dao.go")); !os.IsNotExist(err) {
		t.Fatalf("last generated file must be swept when all definitions are gone: %v", err)
	}
}

func TestParseAcceptsMarkerAboveDocComment(t *testing.T) {
	// Doc comments between marker and struct are ordinary Go practice and
	// must not unbind the marker.
	defs, err := parseSource(t, `package def

//roost:dao coll=heroes db=game
// HeroDao is the persistent hero aggregate.
// It spans several doc lines to prove the distance rule
// no longer breaks on documentation.
type HeroDao struct {
	Name string `+"`"+`store:"name"`+"`"+`
}
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs.Daos) != 1 || defs.Daos[0].Name != "HeroDao" {
		t.Fatalf("daos=%+v", defs.Daos)
	}
}

func TestParseRejectsMarkerWithoutParameters(t *testing.T) {
	_, err := parseSource(t, `package def

//roost:dao
type HeroDao struct {
	Name string `+"`"+`store:"name"`+"`"+`
}
`)
	if err == nil || !strings.Contains(err.Error(), "missing parameters") {
		t.Fatalf("err=%v, want missing parameters error", err)
	}
}

func TestParseRejectsMarkerWithoutRequiredParams(t *testing.T) {
	_, err := parseSource(t, `package def

//roost:dao coll=heroes
type HeroDao struct {
	Name string `+"`"+`store:"name"`+"`"+`
}
`)
	if err == nil || !strings.Contains(err.Error(), "requires coll= and db=") {
		t.Fatalf("err=%v, want required-params error", err)
	}
}

func TestParseRejectsDaoTagTraps(t *testing.T) {
	// 拼写错误和矛盾开关不能退回默认值；空 tag / 只有 map 选项遵循默认双开。
	cases := []struct {
		tag  string
		want string
	}{
		{"`dao:\"persits,sync\"`", "unknown option"},
		{"`dao:\"persist,sync,map=huge\"`", "unknown map mode"},
		{"`dao:\"persist,nopersist\"`", "both persist and nopersist"},
		{"`dao:\"sync,nosync\"`", "both sync and nosync"},
	}
	for _, tc := range cases {
		_, err := parseSource(t, `package def

//roost:dao coll=heroes db=game
type HeroDao struct {
	Items map[int64]int32 `+tc.tag+`
}
`)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("tag %s: err=%v, want %q", tc.tag, err, tc.want)
		}
	}
}

// 嵌套类型的字段没有自己的持久 / 同步开关：持久与同步按 DAO 顶层字段整体决定，嵌套类型的全部字段随之落库、随之下发。
// 嵌套字段上写 nosync / nopersist 曾被接受却不生效（标了 nosync 的内部账本照样进同步补丁发给客户端），所以在解析期拒绝，
// 报错点名类型与字段并给出改法；不改开关的 map= 与整字段排除的 "-" 照常可用。
func TestParseRejectsPersistAndSyncOptionsOnNestedFields(t *testing.T) {
	for _, tag := range []string{`nosync`, `persist,nosync`, `nopersist,nosync`, `nopersist`, `persist`, `sync`, `persist,sync,map=fast`} {
		_, err := parseSource(t, `package def

//roost:dao coll=heroes db=game
type HeroDao struct {
	Wallet *Wallet `+"`"+`store:"wallet"`+"`"+`
}

type Wallet struct {
	Coins  map[int64]int64 `+"`"+`store:"coins"`+"`"+`
	Ledger map[int64]int64 `+"`"+`dao:"`+tag+`" store:"ledger"`+"`"+`
}
`)
		if err == nil {
			t.Fatalf("dao:%q on a nested field was accepted; persistence and sync are per top-level field, so it would not take effect", tag)
		}
		for _, want := range []string{"nested struct Wallet field Ledger", `dao:"` + tag + `"`, "top-level"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("dao:%q: err=%v, want it to contain %q", tag, err, want)
			}
		}
	}
	defs, err := parseSource(t, `package def

//roost:dao coll=heroes db=game
type HeroDao struct {
	Wallet *Wallet `+"`"+`store:"wallet"`+"`"+`
}

type Wallet struct {
	Coins   map[int64]int64 `+"`"+`dao:"map=fast" store:"coins"`+"`"+`
	Scratch int64           `+"`"+`dao:"-"`+"`"+`
}
`)
	if err != nil {
		t.Fatalf("map= and - on nested fields must stay accepted: %v", err)
	}
	if got := defs.Nested[0].Fields; len(got) != 1 || got[0].Tag.Map != "fast" {
		t.Fatalf("nested fields=%+v, want only Coins with map=fast", got)
	}
}

func TestParseAcceptsExplicitTagIntents(t *testing.T) {
	defs, err := parseSource(t, `package def

//roost:dao coll=heroes db=game
type HeroDao struct {
	A int64            `+"`"+`dao:"nosync" store:"a"`+"`"+`
	B int64            `+"`"+`dao:"nopersist,sync" store:"b"`+"`"+`
	C map[int64]int32  `+"`"+`dao:"persist,sync,map=fast" store:"c"`+"`"+`
	D int64            `+"`"+`dao:"-"`+"`"+`
}
`)
	if err != nil {
		t.Fatal(err)
	}
	fields := defs.Daos[0].Fields
	if len(fields) != 3 {
		t.Fatalf("fields=%+v", fields)
	}
	if !fields[0].Tag.Persist || fields[0].Tag.Sync {
		t.Fatalf("A tag=%+v, want persist only", fields[0].Tag)
	}
	if fields[1].Tag.Persist || !fields[1].Tag.Sync {
		t.Fatalf("B tag=%+v, want sync only", fields[1].Tag)
	}
	if !fields[2].Tag.Persist || !fields[2].Tag.Sync || fields[2].Tag.Map != "fast" {
		t.Fatalf("C tag=%+v", fields[2].Tag)
	}
}

func TestRunRemovesOrphansAndCollapsesDaoSuffix(t *testing.T) {
	defDir := t.TempDir()
	outDir := t.TempDir()
	write := func(dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(defDir, "def.go", `package def

//roost:dao coll=heroes db=game
type HeroDao struct {
	Name string `+"`"+`store:"name"`+"`"+`
}
`)
	// A stale generated file from a renamed definition, and a hand-written
	// file that happens to match the pattern but lacks the header.
	write(outDir, "gen_player_dao_dao.go", generatedHeader+"\npackage out\n")
	write(outDir, "gen_manual_dao.go", "package out\n\n// hand-written, must survive\n")

	if err := Run([]string{"-def", defDir, "-out", outDir, "-pkg", "out"}, os.Stdout); err != nil {
		t.Fatal(err)
	}

	// Regression: HeroDao used to produce gen_hero_dao_dao.go.
	if _, err := os.Stat(filepath.Join(outDir, "gen_hero_dao.go")); err != nil {
		t.Fatalf("expected gen_hero_dao.go: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "gen_player_dao_dao.go")); !os.IsNotExist(err) {
		t.Fatalf("orphan generated file was not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "gen_manual_dao.go")); err != nil {
		t.Fatalf("hand-written file must survive orphan cleanup: %v", err)
	}

	// Rename the DAO: the old generated file becomes an orphan and is
	// replaced in the same run.
	write(defDir, "def.go", `package def

//roost:dao coll=heroes db=game
type ChampionDao struct {
	Name string `+"`"+`store:"name"`+"`"+`
}
`)
	if err := Run([]string{"-def", defDir, "-out", outDir, "-pkg", "out"}, os.Stdout); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "gen_champion_dao.go")); err != nil {
		t.Fatalf("expected gen_champion_dao.go: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "gen_hero_dao.go")); !os.IsNotExist(err) {
		t.Fatalf("renamed definition left an orphan behind: %v", err)
	}
}

func TestParseRejectsUnsupportedFieldTypes(t *testing.T) {
	// Regression: these shapes used to fall through as KindBasic and emit
	// code that only failed when the downstream project compiled it.
	cases := []struct {
		field string
		want  string
	}{
		{"Arr [3]int32", "fixed-size arrays"},
		{"Ptr *map[int64]int32", "pointers to named local types"},
		{"Ch chan int", "unsupported type"},
	}
	for _, tc := range cases {
		_, err := parseSource(t, `package def

//roost:dao coll=heroes db=game
type HeroDao struct {
	`+tc.field+`
}
`)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("field %q: err=%v, want %q", tc.field, err, tc.want)
		}
	}
}

// redisdao 标记无参数 / 缺 key 与 prefix / 绑到多个结构体 / 孤立无结构体各报其错；dao 标签同时写 sync 与 nosync 拒绝。
func TestParseRefusesEachMalformedRedisMarkerAndConflictingDaoTag(t *testing.T) {
	cases := []struct{ name, source, want string }{
		{"redisdao without parameters", "package def\n\n//roost:redisdao\ntype HeroCache struct {\n\tName string\n}\n", "//roost:redisdao marker is missing parameters"},
		{"redisdao missing key and prefix", "package def\n\n//roost:redisdao mode=ref-hmap\ntype HeroCache struct {\n\tName string\n}\n", "requires key= and prefix="},
		{"redisdao binding a group", "package def\n\n//roost:redisdao mode=ref-hmap key=id prefix=hero\ntype (\n\tAlphaCache struct{ Name string }\n\tBetaCache  struct{ Name string }\n)\n", "binds to multiple structs"},
		{"orphan redisdao", "package def\n\n//roost:redisdao mode=ref-hmap key=id prefix=hero\n\nvar unrelated = 1\n\ntype HeroCache struct {\n\tName string\n}\n", "not attached to a struct"},
		{"dao tag sync and nosync", `package def

//roost:dao coll=heroes db=game
type HeroDao struct {
	Name string ` + "`" + `dao:"sync,nosync" store:"name"` + "`" + `
}
`, "states both sync and nosync"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSource(t, tc.source)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parse = %v, want %q", err, tc.want)
			}
		})
	}
}

// The generator-level guards behind the parser had no tests of their own: a
// redis DAO with a mode nobody implements, without a key, or without a key
// type, and a Mongo DAO with an unknown dbscope. Each is pinned by message.
func TestGenerateRedisDaoRefusesEachIncompleteDefinition(t *testing.T) {
	defs, err := parseDefDir(mustAbs(t, "./testdata/def"))
	if err != nil {
		t.Fatal(err)
	}
	if len(defs.RedisDaos) == 0 {
		t.Fatal("fixture has no redis daos")
	}
	base := defs.RedisDaos[0]
	cases := []struct {
		name   string
		mutate func(*RedisDaoDef)
		want   string
	}{
		{"unsupported mode", func(d *RedisDaoDef) { d.Mode = "ref-list" }, `unsupported mode "ref-list"`},
		{"missing key", func(d *RedisDaoDef) { d.Key = "" }, "missing key"},
		{"missing key type", func(d *RedisDaoDef) { d.KeyType = "" }, "missing key type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dao := base
			tc.mutate(&dao)
			outFile := filepath.Join(t.TempDir(), "gen_redis_dao.go")
			if _, err := generateRedisDao(dao, "testdata", outFile, true); err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), dao.Name) {
				t.Fatalf("error must mention %q and the dao name; got %v", tc.want, err)
			}
		})
	}
}

func TestGenerateDaoRefusesUnknownDatabaseScope(t *testing.T) {
	defs, err := parseDefDir(mustAbs(t, "./testdata/def"))
	if err != nil {
		t.Fatal(err)
	}
	if len(defs.Daos) == 0 {
		t.Fatal("fixture has no daos")
	}
	dao := defs.Daos[0]
	dao.DbScope = "region"
	outFile := filepath.Join(t.TempDir(), "gen_dao.go")
	if _, err := generateDao(dao, defs, "testdata", outFile, true); err == nil || !strings.Contains(err.Error(), `unsupported dbscope "region"`) {
		t.Fatalf("error = %v", err)
	}
	for _, ok := range []string{"", "global", "sid"} {
		dao.DbScope = ok
		if _, err := generateDao(dao, defs, "testdata", filepath.Join(t.TempDir(), "gen_dao.go"), true); err != nil {
			t.Fatalf("dbscope %q rejected: %v", ok, err)
		}
	}
}

func mustAbs(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// 生成的取值方法不复制引用（只有 []byte 的 GetXAll 与 map 的 []<基本类型> 值收发副本），拿到的值与 DAO 内部共用内存，
// 改它就绕过 setter：没有脏标记、不进 WAL 与同步、回滚覆盖不到。维护者 2026-10-30 决定这些形状在生成期拒绝（方案 A），
// 报错点名类型、字段与建议写法。走完整的 Run：修前这些定义都能生成成功。
func TestGenerationRejectsReferenceShapesTheAccessorsWouldShare(t *testing.T) {
	cases := []struct {
		name   string
		field  string // HeroDao 里的一行
		nested string // Holder 里的一行（非空时 HeroDao 带 Holder *Holder）
		want   string
	}{
		{name: "map of maps", field: "Deep map[int32]map[int32]int64", want: "dao HeroDao field Deep (map[int32]map[int32]int64): the map's values are maps"},
		{name: "map of basic pointers", field: "Best map[int32]*int64", want: "dao HeroDao field Best (map[int32]*int64): the map's values are pointers to int64, which is not a nested struct"},
		{name: "map of pointers to a non-struct type", field: "Ranks map[int32]*Rank", want: "dao HeroDao field Ranks (map[int32]*Rank): the map's values are pointers to Rank, which is not a nested struct"},
		{name: "map of slices of slices", field: "Blobs map[int32][][]byte", want: "dao HeroDao field Blobs (map[int32][][]byte): the map's values are slices whose elements are []byte"},
		{name: "map of slices of nested pointers", field: "Packs map[int32][]*Holder", want: "dao HeroDao field Packs (map[int32][]*Holder): the map's values are slices whose elements are *Holder"},
		{name: "slice of slices", field: "Chunks [][]byte", want: "dao HeroDao field Chunks ([][]byte): the slice's elements are []byte"},
		{name: "slice of maps", field: "Pages []map[int32]int64", want: "dao HeroDao field Pages ([]map[int32]int64): the slice's elements are map[int32]int64"},
		{name: "slice of basic pointers", field: "Scores []*int64", want: "dao HeroDao field Scores ([]*int64): the slice's elements are pointers to int64, which is not a nested struct"},
		{name: "basic pointer field", field: "Gold *int64", want: "dao HeroDao field Gold (*int64): a pointer to int64"},
		{name: "pointer to a non-struct type", field: "Top *Rank", want: "dao HeroDao field Top (*Rank): a pointer to Rank, which is not a nested struct"},
		{name: "shape inside a nested struct", nested: "Chunks [][]byte", want: "nested struct Holder field Chunks ([][]byte): the slice's elements are []byte"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			daoField, holderField := tc.field, "Level int32"
			if tc.nested != "" {
				daoField, holderField = "", tc.nested
			}
			source := `package def

type Rank int64

//roost:dao coll=heroes db=game
type HeroDao struct {
	Name   string
	Holder *Holder
	` + daoField + `
}

type Holder struct {
	` + holderField + `
}
`
			defDir := t.TempDir()
			writeDef(t, defDir, source)
			err := Run([]string{"-def", defDir, "-out", t.TempDir(), "-pkg", "out"}, io.Discard)
			if err == nil {
				t.Fatalf("generation accepted a reference shape whose accessors share the DAO's memory; want an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error does not name the type, field and shape:\n got: %v\nwant: %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "use []byte, a slice of a basic type, a basic type, or a pointer to a nested struct") {
				t.Fatalf("error does not suggest the supported shapes: %v", err)
			}
		})
	}
}

// 允许的形状照常生成：基本类型、[]byte、[]<基本类型>、map[K]<基本类型>、map[K][]<基本类型>（收发副本），以及嵌套类型
// 指针（*T、[]*T、map[K]*T，改动经嵌套类型自己的 setter 标脏），在 DAO 与嵌套类型里都一样。
func TestGenerationAcceptsTheShapesTheAccessorsCopyOrOwn(t *testing.T) {
	out := t.TempDir()
	generateDefs(t, out, `package def

//roost:dao coll=heroes db=game
type HeroDao struct {
	Name     string `+"`"+`store:"name"`+"`"+`
	Blob     []byte `+"`"+`store:"blob"`+"`"+`
	Ids      []int64 `+"`"+`store:"ids"`+"`"+`
	Counts   map[int32]int64 `+"`"+`store:"counts"`+"`"+`
	Receipts map[string][]byte `+"`"+`store:"receipts"`+"`"+`
	Lists    map[int32][]int64 `+"`"+`store:"lists"`+"`"+`
	Main     *Gear `+"`"+`store:"main"`+"`"+`
	Bag      []*Gear `+"`"+`store:"bag"`+"`"+`
	Slots    map[int32]*Gear `+"`"+`store:"slots"`+"`"+`
}

type Gear struct {
	Level  int32 `+"`"+`store:"level"`+"`"+`
	Raw    []byte `+"`"+`store:"raw"`+"`"+`
	Tags   []string `+"`"+`store:"tags"`+"`"+`
	Marks  map[int32][]byte `+"`"+`store:"marks"`+"`"+`
	Socket *Gem `+"`"+`store:"socket"`+"`"+`
	Gems   []*Gem `+"`"+`store:"gems"`+"`"+`
	ByID   map[int64]*Gem `+"`"+`store:"by_id"`+"`"+`
}

type Gem struct {
	Kind int32 `+"`"+`store:"kind"`+"`"+`
}
`)
	for _, name := range []string{"gen_hero_dao.go", "gen_gear_nested.go", "gen_gem_nested.go"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Fatalf("allowed shapes did not generate %s: %v", name, err)
		}
	}
}
