package dao

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestParseDefDir(t *testing.T) {
	dir, err := filepath.Abs("./testdata/def")
	if err != nil {
		t.Fatal(err)
	}

	defs, err := parseDefDir(dir)
	if err != nil {
		t.Fatalf("parseDefDir: %v", err)
	}

	// Should find 1 DAO
	if len(defs.Daos) != 2 {
		t.Fatalf("expected 2 daos, got %d", len(defs.Daos))
	}
	if len(defs.RedisDaos) != 2 {
		t.Fatalf("expected 2 redis daos, got %d", len(defs.RedisDaos))
	}

	dao := defs.Daos[0]
	if dao.Name != "HeroDao" {
		t.Fatalf("expected dao name 'HeroDao', got %q", dao.Name)
	}
	if dao.Coll != "heroes" {
		t.Fatalf("expected coll 'heroes', got %q", dao.Coll)
	}
	if dao.Db != "game" {
		t.Fatalf("expected db 'game', got %q", dao.Db)
	}
	if dao.DbScope != "sid" {
		t.Fatalf("expected dbscope 'sid', got %q", dao.DbScope)
	}

	// Tmp (dao:"-") should be skipped, so 10 fields total
	if len(dao.Fields) != 10 {
		t.Fatalf("expected 10 fields, got %d: %v", len(dao.Fields), fieldNames(dao.Fields))
	}

	// Check LoginAt has persist-only tag
	loginAt := findField(dao.Fields, "LoginAt")
	if loginAt == nil {
		t.Fatal("LoginAt field not found")
	}
	if !loginAt.Tag.Persist || loginAt.Tag.Sync {
		t.Fatalf("LoginAt should be persist=true, sync=false, got %+v", loginAt.Tag)
	}

	// Check Items is map
	items := findField(dao.Fields, "Items")
	if items == nil {
		t.Fatal("Items field not found")
	}
	if items.Kind != KindMap {
		t.Fatalf("Items should be KindMap, got %d", items.Kind)
	}
	if items.MapKey != "int64" || items.MapVal != "int32" {
		t.Fatalf("Items map types wrong: key=%q val=%q", items.MapKey, items.MapVal)
	}

	// Check Friends is slice
	friends := findField(dao.Fields, "Friends")
	if friends == nil {
		t.Fatal("Friends field not found")
	}
	if friends.Kind != KindSlice {
		t.Fatalf("Friends should be KindSlice, got %d", friends.Kind)
	}
	if friends.SliceElem != "int64" {
		t.Fatalf("Friends slice elem should be int64, got %q", friends.SliceElem)
	}

	// Check Pos is nested struct
	pos := findField(dao.Fields, "Pos")
	if pos == nil {
		t.Fatal("Pos field not found")
	}
	if pos.Kind != KindStruct {
		t.Fatalf("Pos should be KindStruct, got %d", pos.Kind)
	}

	// Check Equips is map with pointer value
	equips := findField(dao.Fields, "Equips")
	if equips == nil {
		t.Fatal("Equips field not found")
	}
	if equips.Kind != KindMap || !equips.IsPtr {
		t.Fatalf("Equips should be KindMap with IsPtr=true, got kind=%d isPtr=%v", equips.Kind, equips.IsPtr)
	}
	if equips.MapVal != "EquipInfo" {
		t.Fatalf("Equips map val should be EquipInfo, got %q", equips.MapVal)
	}

	// Should auto-detect nested structs recursively: Position, EquipInfo, GemInfo
	if len(defs.Nested) != 3 {
		t.Fatalf("expected 3 nested, got %d", len(defs.Nested))
	}

	nestedNames := make(map[string]bool)
	for _, n := range defs.Nested {
		nestedNames[n.Name] = true
	}
	if !nestedNames["Position"] {
		t.Fatal("Position not detected as nested")
	}
	if !nestedNames["EquipInfo"] {
		t.Fatal("EquipInfo not detected as nested")
	}
	if !nestedNames["GemInfo"] {
		t.Fatal("GemInfo not detected as nested")
	}

	redisDao := defs.RedisDaos[0]
	if redisDao.Name != "CacheSession" {
		t.Fatalf("expected redis dao name CacheSession, got %q", redisDao.Name)
	}
	if redisDao.Mode != "ref-hmap" {
		t.Fatalf("expected redis mode ref-hmap, got %q", redisDao.Mode)
	}
	if redisDao.Key != "Snapshot.InstanceRunID" {
		t.Fatalf("expected redis key Snapshot.InstanceRunID, got %q", redisDao.Key)
	}
	if redisDao.KeyType != "int64" {
		t.Fatalf("expected redis key type int64, got %q", redisDao.KeyType)
	}
	if redisDao.Prefix != "roost:test:session" {
		t.Fatalf("expected redis prefix roost:test:session, got %q", redisDao.Prefix)
	}
	if redisDao.Version != "Version" {
		t.Fatalf("expected redis version Version, got %q", redisDao.Version)
	}
	if redisDao.TTL != "1h" {
		t.Fatalf("expected redis ttl 1h, got %q", redisDao.TTL)
	}
}

func TestGenerateDao(t *testing.T) {
	dir, err := filepath.Abs("./testdata/def")
	if err != nil {
		t.Fatal(err)
	}

	defs, err := parseDefDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()

	// Generate DAO
	outFile := filepath.Join(outDir, "gen_hero_dao_dao.go")
	changed, err := generateDao(defs.Daos[0], defs, "testdata", outFile, true)
	if err != nil {
		t.Fatalf("generateDao: %v", err)
	}
	if !changed {
		t.Fatal("expected file to be generated")
	}

	content, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	s := string(content)

	checks := []string{
		"package testdata",
		"type HeroDao struct {",
		"dataengine.Tracker",
		"func NewHeroDao() *HeroDao",
		"HeroDaoDBName",
		"HeroDaoCollection",
		`"heroes"`,
		"HeroDaoSchemaVersion uint32 = 1",
		"func (d *HeroDao) Id() int64",
		"func (d *HeroDao) DbName() string",
		"return HeroDaoDBName",
		"func (d *HeroDao) DbScope() dataengine.DatabaseScope",
		"return dataengine.DatabaseServer",
		"func (d *HeroDao) CollName() string",
		"return HeroDaoCollection",
		"func (d *HeroDao) SchemaVersion() uint32",
		"return HeroDaoSchemaVersion",
		"func (d *HeroDao) Migrate(raw []byte, from uint32) ([]byte, error)",
		"func (d *HeroDao) DirtyTracker() *dataengine.Tracker",
		"var _ nest.RollbackSnapshotter = (*HeroDao)(nil)",
		"var _ nest.MutationParticipant = (*HeroDao)(nil)",
		"func (d *HeroDao) CaptureRollbackState() ([]byte, error)",
		"func (d *HeroDao) RestoreRollbackState(raw []byte) error",
		"func (d *HeroDao) marshalCommitState() ([]byte, error)",
		"func (d *HeroDao) PrepareMutation(change nest.PersistChange) (dataengine.Mutation, error)",
		"Kind: dataengine.MutationPatch",
		"ExpectedVersion: version",
		"NextVersion: version + 1",
		"func (d *HeroDao) AcceptMutation(mutation dataengine.Mutation) error",
		"return d.tracker.AcceptVersion(mutation.ExpectedVersion, mutation.NextVersion)",
		"func (d *HeroDao) GetName() string",
		"func (d *HeroDao) GetLevel() int32",
		"func (d *HeroDao) GetFriends(idx int) (int64, bool)",
		"func (d *HeroDao) GetPos() *Position",
		"func (d *HeroDao) SetName(v string)",
		"func (d *HeroDao) SetLevel(v int32)",
		"func (d *HeroDao) SetItems(key int64, val int32)",
		"func (d *HeroDao) DelItems(key int64)",
		"d.recordUndoToken(tx, heroDaoFieldItems, key",
		"panic(fmt.Errorf(\"HeroDao: record undo:",
		"panic(fmt.Errorf(\"HeroDao: marshal persist data:",
		"func (d *HeroDao) markItemsKeyDirty(key int64, val int32)",
		`dataengine.MapPatchPath("items", key)`,
		"nest.MarkPersistSet(d, heroDaoFieldItems, path, val)",
		"nest.MarkPersistUnset(d, heroDaoFieldItems, path)",
		"*fmap.SmallSafeMap[int64, int32]",
		"d.items = fmap.NewSmallSafeMap[int64, int32](0)",
		`"items":    d.heroDaoItemsRawMap()`,
		"func (d *HeroDao) AddFriends(v int64)",
		"func (d *HeroDao) SetPos(v Position)",
		"d.pos.bindDirty(daoDirtyOwner{holder: d, field: heroDaoFieldPos}, d.markPosDirty)",
		"func (d *HeroDao) SetEquips(key int64, val *EquipInfo)",
		"*fmap.SmallSafeMap[int64, *EquipInfo]",
		"val.bindDirty(daoDirtyOwner{holder: d, field: heroDaoFieldEquips, key: key},",
		"func (d *HeroDao) Marshal() []byte",
		"func (d *HeroDao) MarshalPersist(mask uint64) []byte",
		`"_schema"`,
		"HeroDaoSchemaVersion",
		"func (d *HeroDao) marshalPersistPatchBSON(change nest.PersistChange) (dataengine.FieldPatch, error)",
		"func (d *HeroDao) MarshalSync(mask uint64) []byte",
		"func (d *HeroDao) ApplySync(raw []byte) error",
		"func (d *HeroDao) Unmarshal(raw []byte) error",
		"d.Init()",
	}
	for _, check := range checks {
		if !strings.Contains(s, check) {
			t.Errorf("generated DAO code missing: %q", check)
		}
	}
	assertStructFieldsUnexported(t, outFile, "HeroDao")
	for _, forbidden := range []string{
		`"github.com/tjbdwanghaibo/roost-core/checkpoint"`,
		"checkpoint.DirtyTracker",
		"persistPatchSet",
		"func (d *HeroDao) PrepareCommit(",
		"FullData:",
		"func (d *HeroDao) MarshalPersistPatch(",
	} {
		if strings.Contains(s, forbidden) {
			t.Errorf("generated DAO retains legacy persistence path %q", forbidden)
		}
	}

	// LoginAt should be in Marshal and have a setter because persist-only
	// fields still need to mark persist dirty.
	if !strings.Contains(s, `"login_at": d.loginAt`) {
		t.Error("LoginAt should appear in Marshal (persist field)")
	}
	if !strings.Contains(s, "func (d *HeroDao) SetLoginAt") {
		t.Error("LoginAt should have a setter (persist field)")
	}
	if !strings.Contains(s, "d.markLoginAtDirty()") {
		t.Error("SetLoginAt should mark persist dirty")
	}

	// Tmp should not appear at all
	if strings.Contains(s, "Tmp") {
		t.Error("Tmp (dao:\"-\") should not appear in generated code")
	}

	// Second run should be unchanged
	changed, err = generateDao(defs.Daos[0], defs, "testdata", outFile, false)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("expected no change on second run")
	}
}

func TestGenerateNested(t *testing.T) {
	dir, err := filepath.Abs("./testdata/def")
	if err != nil {
		t.Fatal(err)
	}

	defs, err := parseDefDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()

	// Find Position nested
	var posDef NestedDef
	for _, n := range defs.Nested {
		if n.Name == "Position" {
			posDef = n
			break
		}
	}

	outFile := filepath.Join(outDir, "gen_position_nested.go")
	changed, err := generateNested(posDef, defs, "testdata", outFile, true)
	if err != nil {
		t.Fatalf("generateNested: %v", err)
	}
	if !changed {
		t.Fatal("expected file to be generated")
	}

	content, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	// gofmt aligns struct fields into columns (the hook's struct tag widens
	// them, U-0224), so compare with runs of blanks collapsed.
	s := regexp.MustCompile(`[ \t]+`).ReplaceAllString(string(content), " ")

	checks := []string{
		"package testdata",
		"type Position struct {",
		"dataengine.DirtyHook",
		"x int32",
		"y int32",
		"func (s *Position) GetX() int32",
		"func (s *Position) GetY() int32",
		"func (s *Position) SetX(v int32)",
		"func (s *Position) SetY(v int32)",
		"s.Mark()",
	}
	for _, check := range checks {
		if !strings.Contains(s, check) {
			t.Errorf("generated nested code missing: %q", check)
		}
	}
	assertStructFieldsUnexported(t, outFile, "Position")
}

func TestGenerateNestedPointerGetterDoesNotAddPointerLevel(t *testing.T) {
	outFile := filepath.Join(t.TempDir(), "gen_pointer_holder_nested.go")
	nested := NestedDef{Name: "PointerHolder", Fields: []FieldDef{{Name: "Child", TypeStr: "*Position", Kind: KindStruct, IsPtr: true}}}
	if _, err := generateNested(nested, &Definitions{}, "testdata", outFile, true); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	source := string(content)
	if !strings.Contains(source, "func (s *PointerHolder) GetChild() *Position { return s.child }") {
		t.Fatalf("pointer getter must return the stored pointer without producing **Position:\n%s", source)
	}
	assertStructFieldsUnexported(t, outFile, "PointerHolder")
}

func TestFieldVarNamePreservesInitialisms(t *testing.T) {
	for input, want := range map[string]string{
		"Name": "name", "ID": "id", "URLValue": "urlValue", "X": "x",
	} {
		if got := fieldVarName(input); got != want {
			t.Errorf("fieldVarName(%q)=%q want %q", input, got, want)
		}
	}
}

func TestValidateGeneratedStorageFieldsRejectsCollisions(t *testing.T) {
	tests := []struct {
		name     string
		fields   []FieldDef
		reserved []string
		want     string
	}{
		{name: "framework", fields: []FieldDef{{Name: "ID"}}, reserved: []string{"id"}, want: `private storage name "id"`},
		{name: "case folding", fields: []FieldDef{{Name: "URL"}, {Name: "Url"}}, want: `conflicts with field URL`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateGeneratedStorageFields("CollisionDao", test.fields, test.reserved...)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateGeneratedStorageFields() error=%v, want substring %q", err, test.want)
			}
		})
	}
}

func TestGenerateRedisDao(t *testing.T) {
	dir, err := filepath.Abs("./testdata/def")
	if err != nil {
		t.Fatal(err)
	}

	defs, err := parseDefDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs.RedisDaos) != 2 {
		t.Fatalf("expected 2 redis daos, got %d", len(defs.RedisDaos))
	}

	outDir := t.TempDir()
	outFile := filepath.Join(outDir, "gen_cache_session_redis_dao.go")
	changed, err := generateRedisDao(defs.RedisDaos[0], "testdata", outFile, true)
	if err != nil {
		t.Fatalf("generateRedisDao: %v", err)
	}
	if !changed {
		t.Fatal("expected file to be generated")
	}

	content, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	s := string(content)

	checks := []string{
		"package testdata",
		`CacheSessionRedisDAODefaultPrefix = "roost:test:session"`,
		"CacheSessionRedisDAOName",
		`"cache_session"`,
		"type CacheSessionRedisDAO interface {",
		"GetCacheSession(ctx context.Context, key int64) (CacheSession, bool, error)",
		"SetCacheSession(ctx context.Context, value CacheSession) error",
		"DeleteCacheSession(ctx context.Context, key int64) error",
		"PatchCacheSession(ctx context.Context, key int64, path string, value any) error",
		"func NewLocalCacheSessionRedisDAO() CacheSessionRedisDAO",
		"func NewRedisCacheSessionRedisDAO(redis fredis.IRedis, prefix string, ttl time.Duration) CacheSessionRedisDAO",
		"fcache.NewRedisRefHMapStore[int64, CacheSession]",
		"Name:        CacheSessionRedisDAOName",
		"TTL:         ttl",
		"KeyOf: func(value CacheSession) int64 {",
		"return value.Snapshot.InstanceRunID",
		"Stale: fcache.VersionStale(func(value CacheSession) int64 { return int64(value.Version) })",
		"func NewCachedCacheSessionRedisDAOWithTTL(local CacheSessionRedisDAO, remote CacheSessionRedisDAO, ttl time.Duration) CacheSessionRedisDAO",
		"patcher fcache.RefHMapPatcher[int64]",
		"return s.patcher.Patch(ctx, key, path, value)",
		"fcache.PatchStructPath(&current, path, value)",
	}
	for _, check := range checks {
		if !strings.Contains(s, check) {
			t.Errorf("generated Redis DAO code missing: %q", check)
		}
	}
}

func TestGenerateRawRedisDao(t *testing.T) {
	dao := RedisDaoDef{
		Name:    "CacheSession",
		Mode:    "raw",
		Key:     "Snapshot.InstanceRunID",
		KeyType: "int64",
		Prefix:  "roost:test:session",
		Version: "Version",
		TTL:     "1h",
	}
	outFile := filepath.Join(t.TempDir(), "gen_cache_session_redis_dao.go")

	changed, err := generateRedisDao(dao, "testdata", outFile, true)
	if err != nil {
		t.Fatalf("generateRedisDao raw: %v", err)
	}
	if !changed {
		t.Fatal("expected raw redis dao file to be generated")
	}
	content, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	s := string(content)
	for _, check := range []string{
		"fcache.NewRedisRawJSONStore[int64, CacheSession]",
		"func NewRedisCacheSessionRedisDAO(redis fredis.IRedis, prefix string, ttl time.Duration) CacheSessionRedisDAO",
		"func (s cacheSessionRedisDAOStore) PatchCacheSession(ctx context.Context, key int64, path string, value any) error",
		"fcache.PatchStructPath(&current, path, value)",
	} {
		if !strings.Contains(s, check) {
			t.Errorf("generated raw Redis DAO code missing: %q", check)
		}
	}
	if strings.Contains(s, "RefHMap") {
		t.Fatalf("raw Redis DAO should not use RefHMap:\n%s", s)
	}
}

func TestToSnakeDao(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Name", "name"},
		{"LoginAt", "login_at"},
		{"EquipInfo", "equip_info"},
	}
	for _, c := range cases {
		got := toSnake(c.in)
		if got != c.want {
			t.Errorf("toSnake(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func findField(fields []FieldDef, name string) *FieldDef {
	for i := range fields {
		if fields[i].Name == name {
			return &fields[i]
		}
	}
	return nil
}

func fieldNames(fields []FieldDef) []string {
	var names []string
	for _, f := range fields {
		names = append(names, f.Name)
	}
	return names
}

func assertStructFieldsUnexported(t *testing.T, filename, typeName string) {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
	if err != nil {
		t.Fatalf("parse generated file %s: %v", filename, err)
	}
	for _, declaration := range file.Decls {
		generic, ok := declaration.(*ast.GenDecl)
		if !ok || generic.Tok != token.TYPE {
			continue
		}
		for _, spec := range generic.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != typeName {
				continue
			}
			structure, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				t.Fatalf("generated %s is not a struct", typeName)
			}
			for _, field := range structure.Fields.List {
				for _, name := range field.Names {
					if ast.IsExported(name.Name) {
						t.Errorf("generated %s exposes storage field %s; reads must use methods", typeName, name.Name)
					}
				}
			}
			return
		}
	}
	t.Fatalf("generated type %s not found", typeName)
}
