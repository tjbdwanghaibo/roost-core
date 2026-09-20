package entity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDir(t *testing.T) {
	dir, err := filepath.Abs("./testdata")
	if err != nil {
		t.Fatal(err)
	}

	entities, pkg, err := parseDir(dir)
	if err != nil {
		t.Fatalf("parseDir: %v", err)
	}

	if pkg != "testdata" {
		t.Fatalf("expected package 'testdata', got %q", pkg)
	}

	if len(entities) != 1 {
		t.Fatalf("expected 1 entity, got %d", len(entities))
	}

	ent := entities[0]
	if ent.Name != "Player" {
		t.Fatalf("expected entity name 'Player', got %q", ent.Name)
	}
	if ent.EntityKind != "EntityKindPlayer" {
		t.Fatalf("expected entityKind 'EntityKindPlayer', got %q", ent.EntityKind)
	}
	// One packer factory, one marker: Core's EntitySyncBuilderParam has a
	// single PackerFactory, so subjectPacker is the spelling and syncPacker
	// is its legacy alias. Setting both is refused at parse time
	// (RR-20260918-01, sync_packer_markers_promises_test.go).
	if !ent.Sync || ent.SyncTopic != `"player"` || ent.SubjectPacker != "clientsync.PlayerSubjectPacker" || ent.SyncPacker != "" {
		t.Fatalf("sync config = enabled:%v topic:%q packer:%q subject:%q", ent.Sync, ent.SyncTopic, ent.SyncPacker, ent.SubjectPacker)
	}

	if len(ent.Components) != 2 {
		t.Fatalf("expected 2 components, got %d", len(ent.Components))
	}
	if ent.Components[0].FieldName != "bag" {
		t.Fatalf("expected first component 'bag', got %q", ent.Components[0].FieldName)
	}
	if ent.Components[0].CompType != "CompTypeBag" {
		t.Fatalf("expected CompType 'CompTypeBag', got %q", ent.Components[0].CompType)
	}
	if ent.Components[1].FieldName != "battle" {
		t.Fatalf("expected second component 'battle', got %q", ent.Components[1].FieldName)
	}

	if len(ent.Daos) != 2 {
		t.Fatalf("expected 2 daos, got %d", len(ent.Daos))
	}
	if ent.Daos[0].FieldName != "dao" {
		t.Fatalf("expected dao field 'dao', got %q", ent.Daos[0].FieldName)
	}
	if ent.Daos[0].CollName != "players" {
		t.Fatalf("expected collName 'players', got %q", ent.Daos[0].CollName)
	}
	if ent.Daos[1].FieldName != "mail" {
		t.Fatalf("expected dao field 'mail', got %q", ent.Daos[1].FieldName)
	}
	if ent.Daos[1].CollName != "mails" {
		t.Fatalf("expected collName 'mails', got %q", ent.Daos[1].CollName)
	}
}

func TestGenerate(t *testing.T) {
	dir, err := filepath.Abs("./testdata")
	if err != nil {
		t.Fatal(err)
	}

	entities, pkg, err := parseDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	tmpFile := filepath.Join(t.TempDir(), "player_gen_wire.go")
	changed, err := generate(entities[0], pkg, tmpFile, true)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !changed {
		t.Fatal("expected file to be generated")
	}

	content, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatal(err)
	}

	// Verify key content
	s := string(content)
	checks := []string{
		"func NewPlayer(param *entity.EntityCreateParam)",
		"Category: entity.MustEntityCategoryOfKind(EntityKindPlayer)",
		"param.NormalizeID(EntityKindPlayer)",
		`"player"`,
		// Core's EntitySyncBuilderParam has one packer field; the fixture
		// names it with the current marker (RR-20260918-01).
		"PackerFactory: clientsync.PlayerSubjectPacker",
		"e.EntityBase = entity.NewEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind)",
		"func (e *Player) Base() *entity.EntityBase",
		"func (e *Player) BagComp() *BagComponent",
		"func (e *Player) BattleComp() *BattleComponent",
		"func (e *Player) Dao() *PlayerDao",
		"func (e *Player) MailDao() *MailDao",
		"entity.CreateComponent(CompTypeBag, e, param)",
		"entity.CreateComponent(CompTypeBattle, e, param)",
		"e.dao = NewPlayerDao()",
		"e.mail = NewMailDao()",
		"func (e *Player) generatedOnClear()",
		"func (e *Player) generatedOnDestroy(reason entity.EntityDestroyReason)",
		"func (e *Player) PrepareDelete(tx *nest.RollbackTx) error",
		"tx.MarkPersistDelete(participant)",
	}
	for _, check := range checks {
		if !contains(s, check) {
			t.Errorf("generated code missing: %q", check)
		}
	}
	if strings.Contains(s, ".Tracker") {
		t.Fatalf("entity generator bypassed DAO tracker accessor:\n%s", s)
	}
	for _, forbidden := range []string{"func (e *Player) Snapshot()", "func (e *Player) RemoveSnapshot()", "checkpoint.SaveItem"} {
		if strings.Contains(s, forbidden) {
			t.Fatalf("entity generator retains legacy checkpoint path %q", forbidden)
		}
	}

	// Second run should be unchanged
	changed, err = generate(entities[0], pkg, tmpFile, false)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("expected no change on second run")
	}
}

func TestGenerateRemoteManagedV2Participant(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("testdata", "player.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(source),
		"//roost:entity entityKind=EntityKindPlayer sync=true",
		"//roost:entity entityKind=EntityKindPlayer remote=managed sync=true", 1)
	text = strings.Replace(text, "*entity.EntityBase", "*entity.RemoteEntityBase", 1)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "player.go"), []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
	entities, pkg, err := parseDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entities) != 1 || !entities[0].RemoteBase {
		t.Fatalf("remote base was not detected: %+v", entities)
	}
	out := filepath.Join(dir, "player_gen_wire.go")
	if _, err := generate(entities[0], pkg, out, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	generated := string(raw)
	for _, required := range []string{
		"entity.NewRemoteEntityBaseWithMutex(",
		"var _ entity.IRemoteCommitParticipant",
		"var _ entity.IRemoteCommitChangeParticipant",
		"HasRemoteCommitLocked(",
		"BuildRemoteCommitLocked(",
		"outcome.PersistChanges.RemotePersistChangeFor(",
		"AcknowledgeRemoteCommit(",
		"DirtyTracker().AdvanceVersion(",
		"RollbackRemoteCommit(",
		"entity.RemoteDataMutation{",
		"commit.Invalidations = append(",
		"entity.MustRegisterRemoteSnapshotDecoder(",
	} {
		if !strings.Contains(generated, required) {
			t.Errorf("generated V2 participant missing %q", required)
		}
	}
	if strings.Contains(generated, "TakePersistDirty") || strings.Contains(generated, "RollbackPersist") {
		t.Fatalf("generated V2 participant still reads non-transactional persistence dirty state")
	}
}

func TestGeneratePreservesManualSyncMethods(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("testdata", "player.go"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	source = append(source, []byte(`

func (e *Player) OnDataChange(_ []byte, _ int64) {}
`)...)
	if err := os.WriteFile(filepath.Join(dir, "player.go"), source, 0644); err != nil {
		t.Fatal(err)
	}
	entities, pkg, err := parseDir(dir)
	if err != nil {
		t.Fatalf("parseDir: %v", err)
	}
	if len(entities) != 1 {
		t.Fatalf("entities = %d, want 1", len(entities))
	}
	out := filepath.Join(dir, "player_gen_wire.go")
	if _, err := generate(entities[0], pkg, out, true); err != nil {
		t.Fatalf("generate: %v", err)
	}
	content, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	generated := string(content)
	if contains(generated, "func (e *Player) OnDataChange(") {
		t.Fatal("generated OnDataChange must not duplicate a manual method")
	}
	if contains(generated, "func (e *Player) ApplyRemoteSync(") {
		t.Fatal("generator must not emit the removed V1 remote sync protocol")
	}
	if !contains(generated, `"github.com/tjbdwanghaibo/roost-core/nest"`) || contains(generated, "func (e *Player) RemoveSnapshot(") {
		t.Fatal("persistent entity must use transactional delete without generated RemoveSnapshot")
	}
}

func TestToSnake(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Player", "player"},
		{"PlayerBase", "player_base"},
		{"HTTPServer", "h_t_t_p_server"},
		{"A", "a"},
	}
	for _, c := range cases {
		got := toSnake(c.in)
		if got != c.want {
			t.Errorf("toSnake(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && stringContains(s, substr)
}

func stringContains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
