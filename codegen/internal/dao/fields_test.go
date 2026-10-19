package dao

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"text/template"
)

// 字段名首字母小写之后可能是 Go 关键字（`Type`、`Range`、`Map`、`Func` 都是 DAO 定义里自然的字段名）：生成物的私有名
// 必须避开关键字，包括嵌套结构体里的字段，否则整个包编译不过，错误还指向生成的临时文件。

func generateKeywordDao(t *testing.T, source string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "thing.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := Run([]string{"-def", dir, "-out", out, "-pkg", "db"}, os.Stderr); err != nil {
		t.Fatalf("generate: %v", err)
	}
	return out
}

func TestAFieldWhoseLowercaseNameIsAKeywordGenerates(t *testing.T) {
	out := generateKeywordDao(t, `package def

//roost:dao coll=things db=game
type ThingDao struct {
	Type  int32  `+"`bson:\"type\"`"+`
	Range int64  `+"`bson:\"range\"`"+`
	Map   string `+"`bson:\"map\"`"+`
	Name  string `+"`bson:\"name\"`"+`
}
`)
	generated, err := os.ReadFile(filepath.Join(out, "gen_thing_dao.go"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(generated)
	// The accessors keep the field's own name; only the private storage is
	// renamed, so a definition does not change meaning to its callers.
	for _, want := range []string{"func (d *ThingDao) GetType() int32", "func (d *ThingDao) SetType(", "func (d *ThingDao) GetRange() int64"} {
		if !strings.Contains(body, want) {
			t.Errorf("the generated DAO lost %q:\n%s", want, body)
		}
	}
}

// The same name inside a nested struct, which is where the demo hit it: the
// nested generator has its own template and its own copy of the problem.
func TestANestedFieldWhoseLowercaseNameIsAKeywordGenerates(t *testing.T) {
	out := generateKeywordDao(t, `package def

//roost:dao coll=things db=game
type ThingDao struct {
	Node Node `+"`bson:\"node\"`"+`
}

type Node struct {
	Type   int32  `+"`bson:\"type\"`"+`
	Select string `+"`bson:\"select\"`"+`
}
`)
	generated, err := os.ReadFile(filepath.Join(out, "gen_node_nested.go"))
	if err != nil {
		t.Fatal(err)
	}
	if body := string(generated); !strings.Contains(body, "func (s *Node) GetType() int32") {
		t.Errorf("the generated nested struct lost its accessor:\n%s", body)
	}
}

// 生成的嵌套 struct 能被 BSON 持久化，且 DirtyHook 不入文档：嵌套 struct 的字段全部未导出（变更只能走生成的 setter），
// 所以内嵌 DirtyHook 打 bson:"-" json:"-"，每个嵌套类型生成 MarshalBSON（值接收者）/ UnmarshalBSON（指针接收者），
// 按 snake_case 键写全部字段；否则落库的只是 {"pos": {"dirtyhook": {}}}。
func TestNestedStructsPersistTheirFieldsNotTheDirtyHook(t *testing.T) {
	defs, err := parseDefDir(mustAbs(t, "./testdata/def"))
	if err != nil {
		t.Fatal(err)
	}
	var position *NestedDef
	for i := range defs.Nested {
		if defs.Nested[i].Name == "Position" {
			position = &defs.Nested[i]
		}
	}
	if position == nil {
		t.Fatal("fixture has no Position nested struct")
	}
	tmpl := template.Must(template.New("nested").Funcs(nestedFuncMap(defs)).Parse(nestedTemplate))
	var out bytes.Buffer
	if err := tmpl.Execute(&out, map[string]any{"Package": "testdata", "Nested": *position}); err != nil {
		t.Fatal(err)
	}
	source := out.String()
	for _, want := range []string{
		"dataengine.DirtyHook `bson:\"-\" json:\"-\"`",
		"func (s Position) MarshalBSON() ([]byte, error)",
		"func (s *Position) UnmarshalBSON(raw []byte) error",
		"func (s Position) bsonDoc() positionBSONDoc",
		"func positionPtrBSONDoc(v *Position) *positionBSONDoc",
		"`bson:\"x\"`",
		"`bson:\"y\"`",
	} {
		if !strings.Contains(source, want) {
			t.Errorf("generated nested struct lacks %q; its fields would not reach Mongo and DirtyHook would\n%s", want, source)
		}
	}
}

// The follow-up to the fix above: the parent DAO must not encode nested values
// through bson.Marshaler (one fresh []byte plus an interface box per value —
// about three times the reflection floor for a document with a few of them).
// It places the wire form into its own document instead, so reflection
// encodes the nested value inline.
func TestTheDaoDocumentCarriesNestedWireFormsNotMarshalers(t *testing.T) {
	defs, err := parseDefDir(mustAbs(t, "./testdata/def"))
	if err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(t.TempDir(), "gen_hero_dao.go")
	if _, err := generateDao(defs.Daos[0], defs, "testdata", outFile, true); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	source := squeezeSpaces(string(content))
	for _, want := range []string{
		`Pos: d.pos.bsonDoc(),`,
		`Equips: daoMapDocs(d.heroDaoEquipsRawMap(), equipInfoPtrBSONDoc),`,
		"Pos positionBSONDoc `bson:\"pos\"`",
		"Equips map[int64]*equipInfoBSONDoc `bson:\"equips\"`",
		"d.pos = positionFromBSONDoc(doc.Pos)",
		"d.setEquipsRawMap(daoMapDocs(doc.Equips, equipInfoPtrFromBSONDoc))",
	} {
		if !strings.Contains(source, want) {
			t.Errorf("the DAO document does not use the nested wire form: missing %q", want)
		}
	}
	for _, stale := range []string{`Pos: d.pos,`, "Pos Position `bson:\"pos\"`"} {
		if strings.Contains(source, stale) {
			t.Errorf("the DAO document still hands the nested value itself to the codec: %q", stale)
		}
	}
}

// A1（维护者 2026-10-05：回滚统一走 DAO，docs/feature/REFACTOR-2026-10-05-dao-unified-rollback.md）：事务内会改、回滚时要恢复、
// 却不该落库也不该同步的状态放进 `dao:"nopersist,nosync"` 字段。承诺：这种字段和其他字段一样生成 mutator（undo 策略下登记逆操作），
// map 字段同样有 Get / Range / Len；它出现在回滚快照里，不出现在提交记录、存储读写与同步里。旧生成器只给 persist 或 sync 的字段
// 生成 mutator，nopersist,nosync 的标量只有 getter、map 连 getter 都没有，业务无从改它。
func TestTransientFieldsHaveMutatorsAndStayOutOfStorageAndSync(t *testing.T) {
	defs, err := parseDefDir(mustAbs(t, "./testdata/def"))
	if err != nil {
		t.Fatal(err)
	}
	var variety *DaoDef
	for i := range defs.Daos {
		if defs.Daos[i].Name == "VarietyDao" {
			variety = &defs.Daos[i]
		}
	}
	if variety == nil {
		t.Fatal("fixture has no VarietyDao")
	}
	outFile := filepath.Join(t.TempDir(), "gen_variety_dao.go")
	if _, err := generateDao(*variety, defs, "testdata", outFile, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	for _, want := range []string{
		"func (d *VarietyDao) SetNeither(v int64)",
		"func (d *VarietyDao) SetPending(key int32, val int64)",
		"func (d *VarietyDao) DelPending(key int32)",
		"func (d *VarietyDao) GetPending(key int32) (int64, bool)",
		"func (d *VarietyDao) RangePending(",
		"func (d *VarietyDao) PendingLen() int",
	} {
		if !strings.Contains(source, want) {
			t.Errorf("the generated DAO has no %q: a nopersist,nosync field cannot be changed inside a transaction", want)
		}
	}
	bodies := generatedFunctions(source)
	rollbackDoc := generatedType(source, "varietyDaoRollbackDoc")
	if !strings.Contains(rollbackDoc, `bson:"pending"`) || !strings.Contains(rollbackDoc, `bson:"neither"`) || !strings.Contains(rollbackDoc, `bson:"_persisted"`) {
		t.Errorf("the rollback snapshot does not cover the transient fields:\n%s", rollbackDoc)
	}
	if persistDoc := generatedType(source, "varietyDaoPersistDoc"); strings.Contains(persistDoc, "pending") || strings.Contains(persistDoc, "neither") || !strings.Contains(persistDoc, `bson:"persist_only"`) {
		t.Errorf("the persisted document is not exactly the persist fields:\n%s", persistDoc)
	}
	for _, name := range []string{"persistDoc", "setPersistDoc", "marshalCommitState", "marshalPersistData", "marshalPersistPatchBSON", "Unmarshal", "MarshalSync", "ApplySync", "markPendingKeyDirty", "markNeitherDirty"} {
		body, ok := bodies[name]
		if !ok {
			t.Fatalf("generated DAO has no %s", name)
		}
		if strings.Contains(body, "pending") || strings.Contains(body, "Pending") || strings.Contains(body, "neither") || strings.Contains(body, "Neither") {
			t.Errorf("%s touches a transient field; it must not reach storage, the commit record or sync:\n%s", name, body)
		}
	}
}

// generatedType returns the declaration of a top-level struct type in generated source.
func generatedType(source, name string) string {
	_, rest, ok := strings.Cut(source, "\ntype "+name+" struct {")
	if !ok {
		return ""
	}
	body, _, _ := strings.Cut(rest, "\n}\n")
	return body
}

// generatedFunctions splits generated source into top-level function bodies,
// keyed by function name (methods by method name).
func generatedFunctions(source string) map[string]string {
	out := make(map[string]string)
	for _, chunk := range strings.Split(source, "\nfunc ")[1:] {
		header, _, _ := strings.Cut(chunk, "(")
		name := header
		if strings.HasPrefix(chunk, "(") {
			// method: "(d *X) Name(" — the name follows the receiver.
			_, rest, _ := strings.Cut(chunk, ") ")
			name, _, _ = strings.Cut(rest, "(")
		}
		body := chunk
		if end := strings.Index(chunk, "\n}\n"); end >= 0 {
			body = chunk[:end]
		}
		// The mark functions' own names contain the field name; check the body only.
		if _, after, ok := strings.Cut(body, "{"); ok {
			body = after
		}
		out[strings.TrimSpace(name)] = body
	}
	return out
}

// 持久键规则（DESIGN-TODO D13，维护者 2026-10-19 决定）：DAO 定义里字段写了 bson:"x" 时 x 就是持久键，没写时是
// Go 字段名的 snake_case；生成的持久文档结构体（<dao>PersistDoc / 嵌套类型的 <type>BSONDoc）把每个字段存成的键写成
// 显式 bson 标签，读写、回滚、补丁都用它。新用户第七轮：把 Gold 改名 Coins、保留 bson:"gold" 想保住库里的键，修前
// 生成物仍按 coins 存取，金币读成 0。
func TestTheDeclaredBSONNameIsTheStoredKey(t *testing.T) {
	out := generateDefs(t, t.TempDir(), `package def

//roost:dao coll=wallets db=game
type WalletDao struct {
	Coins int64  `+"`bson:\"gold\" dao:\"persist,sync\"`"+`
	Name  string `+"`dao:\"persist,sync\"`"+`
	Note  string `+"`bson:\"memo,omitempty\" dao:\"persist\"`"+`
	Bag   *Bag   `+"`bson:\"bag\" dao:\"persist,sync\"`"+`
}

type Bag struct {
	Slots int32 `+"`bson:\"slot_count\"`"+`
}
`)
	dao := readGenerated(t, out, "gen_wallet_dao.go")
	for _, want := range []string{
		"type walletDaoPersistDoc struct",
		"Coins int64 `bson:\"gold\"`",
		"Name string `bson:\"name\"`",
		"Note string `bson:\"memo,omitempty\"`",
		`set["gold"]`,
		`WireName: "gold"`,
	} {
		if !strings.Contains(squeezeSpaces(dao), want) {
			t.Errorf("generated DAO lacks %q", want)
		}
	}
	if strings.Contains(dao, `"coins"`) {
		t.Errorf("the Go field name still decides a key:\n%s", dao)
	}
	nested := readGenerated(t, out, "gen_bag_nested.go")
	if !strings.Contains(squeezeSpaces(nested), "Slots int32 `bson:\"slot_count\"`") {
		t.Errorf("nested wire doc ignores the declared name:\n%s", nested)
	}
}

// 显式名的规则在生成期强制一次：键要能当 Mongo 字段路径用（字母开头，只含字母、数字、下划线），不能以 _ 开头
// （_id、_schema、_version 等是框架元数据），同一个类型里不能重复；bson:"-" 与 omitempty 以外的选项没有含义，
// 写了直接报错而不是悄悄忽略。
func TestInvalidDeclaredStoredKeysFailGeneration(t *testing.T) {
	for _, tc := range []struct {
		name, fields, want string
	}{
		{"reserved", "A int64 `bson:\"_schema\"`", `reserved`},
		{"path", "A int64 `bson:\"a.b\"`", `not a valid stored key`},
		{"dash", "A int64 `bson:\"-\"`", `dao:"nopersist`},
		{"option", "A int64 `bson:\"a,inline\"`", `option "inline"`},
		{"duplicate", "A int64 `bson:\"gold\"`\n\tGold int64", `both store as "gold"`},
		{"duplicate-transient", "A int64 `bson:\"x\" dao:\"nopersist,sync\"`\n\tX int64 `dao:\"persist\"`", `both store as "x"`},
		{"reserved-go-name", "SchemaVersion int64", `SchemaVersion`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeDef(t, dir, "package def\n\n//roost:dao coll=things db=game\ntype ThingDao struct {\n\t"+tc.fields+"\n}\n")
			err := Run([]string{"-def", dir, "-out", t.TempDir(), "-pkg", "db"}, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// 改字段告警（生成期）：生成器把输出目录里上一次的生成物当基线，比较持久键。键消失（改名 / 删字段 / 改 nopersist）、
// 新增、类型变化都打印 WARN，写明后果、迁移手册和 schema 是否提高；只提示，不阻断生成。同一份定义再生成一次不重复
// 告警；改 Go 名但保留原 bson 名，键没变，不告警。修前生成器对改名一声不吭。
func TestGenerateWarnsWhenAStoredKeyChangesAgainstThePreviousOutput(t *testing.T) {
	out := t.TempDir()
	generateDefs(t, out, `package def

//roost:dao coll=wallets db=game
type WalletDao struct {
	Gold  int64
	Level int32
	Bag   Bag
}

type Bag struct {
	Slots int32
}
`)
	var output bytes.Buffer
	generateDefsTo(t, out, &output, `package def

//roost:dao coll=wallets db=game
type WalletDao struct {
	Coins int64
	Level string
	Bag   Bag
}

type Bag struct {
	Capacity int32
}
`)
	text := output.String()
	for _, want := range []string{
		`WARN: WalletDao: stored key "gold" (Gold int64) is no longer generated`,
		`Documents already stored keep "gold" but it will not be read`,
		`the next full write of each document deletes it`,
		`if this is a rename the new field starts at the zero value`,
		"migrate the stored value offline before raising schema=",
		`bson:"gold"`,
		"docs/framework/SCHEMA-MIGRATION.md",
		`WARN: WalletDao: new stored key "coins" (Coins int64)`,
		`WARN: WalletDao: stored key "level" changes type int32 -> string`,
		"schema= is still 1",
		`WARN: Bag: stored key "slots" (Slots int32) is no longer generated`,
		`WARN: Bag: new stored key "capacity" (Capacity int32)`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("generate output lacks %q:\n%s", want, text)
		}
	}

	output.Reset()
	generateDefsTo(t, out, &output, `package def

//roost:dao coll=wallets db=game
type WalletDao struct {
	Coins int64
	Level string
	Bag   Bag
}

type Bag struct {
	Capacity int32
}
`)
	if strings.Contains(output.String(), "WARN") {
		t.Errorf("regenerating the same definition warned again:\n%s", output.String())
	}

	output.Reset()
	generateDefsTo(t, out, &output, `package def

//roost:dao coll=wallets db=game schema=2
type WalletDao struct {
	Money int64 `+"`bson:\"coins\"`"+`
	Level string
	Bag   Bag
}

type Bag struct {
	Capacity int32
}
`)
	if strings.Contains(output.String(), "WARN") {
		t.Errorf("a rename that keeps the stored key warned:\n%s", output.String())
	}

	output.Reset()
	generateDefsTo(t, out, &output, `package def

//roost:dao coll=wallets db=game schema=3
type WalletDao struct {
	Level string
	Bag   Bag
}

type Bag struct {
	Capacity int32
}
`)
	if text := output.String(); !strings.Contains(text, `stored key "coins" (Money int64) is no longer generated`) || !strings.Contains(text, "schema= was raised from 2 to 3") {
		t.Errorf("deleting a field with a raised schema is not reported as such:\n%s", text)
	}
}

func writeDef(t *testing.T, dir, source string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "def.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
}

// generateDefs 在 out 里生成 source 定义的 DAO；out 里已有的生成物就是上一次的输出（基线）。
func generateDefs(t *testing.T, out, source string) string {
	t.Helper()
	generateDefsTo(t, out, io.Discard, source)
	return out
}

func generateDefsTo(t *testing.T, out string, w io.Writer, source string) {
	t.Helper()
	defDir := t.TempDir()
	writeDef(t, defDir, source)
	if err := Run([]string{"-def", defDir, "-out", out, "-pkg", "out"}, w); err != nil {
		t.Fatal(err)
	}
}

func readGenerated(t *testing.T, dir, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// squeezeSpaces 把连续空白压成一个空格，比较时不受 gofmt 对齐影响。
func squeezeSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// 持久文档结构体是键的唯一来源：生成物里增量补丁、整字段重写、内存事务的字段名、同步表与同步文档用到的每个键字面量，
// 都必须是持久文档（或回滚快照里非持久字段）结构体上的某个 bson 标签。模板里只剩 FieldDef.Key 这一种取键方式，这条
// 测试防止以后再出现第二种拼法。
func TestEveryKeyTheGeneratedDaoSpellsIsATagOfItsDocumentStructs(t *testing.T) {
	out := generateDefs(t, t.TempDir(), `package def

//roost:dao coll=wallets db=game
type WalletDao struct {
	Coins   int64            `+"`bson:\"gold\" dao:\"persist,sync\"`"+`
	Items   map[int64]int32  `+"`bson:\"bag_items\" dao:\"persist,sync,map=fast\"`"+`
	Gear    map[int32]*Piece `+"`bson:\"gear\"`"+`
	Shown   int64            `+"`bson:\"shown_value\" dao:\"nopersist,sync\"`"+`
	Pending map[int32]int64  `+"`bson:\"pending_set\" dao:\"nopersist,nosync,map=fast\"`"+`
}

type Piece struct {
	Level int32 `+"`bson:\"lv\"`"+`
}
`)
	source := readGenerated(t, out, "gen_wallet_dao.go")
	tags := map[string]bool{}
	tagRe := regexp.MustCompile("`bson:\"([^\",]+)")
	for _, typ := range []string{"walletDaoPersistDoc", "walletDaoRollbackDoc"} {
		for _, m := range tagRe.FindAllStringSubmatch(generatedType(source, typ), -1) {
			tags[m[1]] = true
		}
	}
	persisted := map[string]bool{}
	for _, m := range tagRe.FindAllStringSubmatch(generatedType(source, "walletDaoPersistDoc"), -1) {
		persisted[m[1]] = true
	}
	for name, want := range map[string]map[string]bool{
		`MapPatchPath\("([^"]+)"`:                 persisted,
		`MarkPersistFull\(d, \w+, "([^"]+)"\)`:    persisted,
		`set\["([^"]+)"\]`:                        persisted,
		`FullFields\["([^"]+)"\]`:                 persisted,
		`persistChangeHasPath\(change, "([^"]+)"`: persisted,
		`names = append\(names, "([^"]+)"\)`:      persisted,
		`WireName: "([^"]+)"`:                     tags,
		`doc\["([^"]+)"\]`:                        tags,
	} {
		matches := regexp.MustCompile(name).FindAllStringSubmatch(source, -1)
		if len(matches) == 0 {
			t.Errorf("pattern %s found nothing; the template changed shape", name)
		}
		for _, m := range matches {
			if m[1] != "_id" && !want[m[1]] {
				t.Errorf("%s spells key %q, which no document struct declares", name, m[1])
			}
		}
	}
	for _, key := range []string{"gold", "bag_items", "gear"} {
		if !persisted[key] {
			t.Errorf("persisted document lacks %q", key)
		}
	}
	if persisted["shown_value"] || persisted["pending_set"] || !tags["shown_value"] || !tags["pending_set"] {
		t.Errorf("non-persisted fields are in the wrong document: persisted=%v all=%v", persisted, tags)
	}
	if nested := readGenerated(t, out, "gen_piece_nested.go"); !strings.Contains(squeezeSpaces(nested), "Level int32 `bson:\"lv\"`") {
		t.Errorf("nested wire doc ignores the declared name:\n%s", nested)
	}
}
