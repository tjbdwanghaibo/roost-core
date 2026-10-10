package entity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncNamespaceRejectsABareIdentifier(t *testing.T) {
	for name, value := range map[string]string{
		"same package constant": "SyncNamespacePlayer",
		"exported and short":    "Topic",
	} {
		if err := validateSyncNamespaceParam(value); err == nil {
			t.Errorf("%s: syncNamespace=%s was accepted; it would be written out as the string %q", name, value, value)
		}
	}
}

// The two unambiguous spellings keep working, and a qualified constant is
// still emitted as a reference rather than quoted.
func TestSyncNamespaceAcceptsLiteralsAndQualifiedConstants(t *testing.T) {
	for name, test := range map[string]struct {
		value string
		want  string
	}{
		"lower case literal": {"player", `"player"`},
		"quoted literal":     {`"Player"`, `"Player"`},
		"qualified constant": {"clientsync.PlayerTopic", "clientsync.PlayerTopic"},
		"empty":              {"", `""`},
	} {
		if err := validateSyncNamespaceParam(test.value); err != nil {
			t.Errorf("%s: syncNamespace=%s was rejected: %v", name, test.value, err)
			continue
		}
		if got := syncNamespaceExpr(test.value); got != test.want {
			t.Errorf("%s: syncNamespaceExpr(%q) = %s, want %s", name, test.value, got, test.want)
		}
	}
}

func writeSyncMarkerSource(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedSyncBlockOnlyNamesFieldsCoreHas(t *testing.T) {
	for name, marker := range map[string]string{
		"current spelling": "//roost:entity entityKind=EntityKindAvatar sync=true syncNamespace=\"avatar\" subjectPacker=AvatarPacker",
		"no packer":        "//roost:entity entityKind=EntityKindAvatar sync=true syncNamespace=\"avatar\"",
	} {
		dir := t.TempDir()
		writeSyncMarkerSource(t, dir, "avatar.go", `package avatar

import "github.com/tjbdwanghaibo/roost-core/framework/entity"

const EntityKindAvatar entity.EntityKind = 141
const SyncNamespaceAvatar = "avatar"

func AvatarPacker(entity.IThreadSafeEntity) entity.SubjectSyncPacker { return nil }

`+marker+`
type Avatar struct {
	*entity.EntityBase
	entity.ComponentManager
	entity.DaoManager
}
`)
		if err := Run([]string{"-dir", dir, "-force"}, os.Stdout); err != nil {
			t.Fatalf("%s: generate: %v", name, err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "avatar_gen_wire.go"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		generated := string(raw)
		for _, gone := range []string{"FlushPolicy", "SyncFlushOnEntityRelease", "SubjectPackerFactory"} {
			if strings.Contains(generated, gone) {
				t.Errorf("%s: generated wiring still names %q, which entity.EntitySyncBuilderParam does not have:\n%s", name, gone, generated)
			}
		}
		// gofmt aligns the struct literal, so compare with the padding
		// collapsed rather than against one exact spelling.
		collapsed := strings.Join(strings.Fields(generated), " ")
		if !strings.Contains(collapsed, "Enabled: true") || !strings.Contains(collapsed, `Namespace: "avatar"`) {
			t.Errorf("%s: sync block lost its enabled/namespace wiring:\n%s", name, generated)
		}
		wantFactory := strings.Contains(marker, "Packer=")
		if got := strings.Contains(collapsed, "PackerFactory: AvatarPacker"); got != wantFactory {
			t.Errorf("%s: PackerFactory present = %v, want %v:\n%s", name, got, wantFactory, generated)
		}
	}
}

// 旧标记不能被静默忽略，否则生成实体会丢失业务 packer。
func TestRetiredPackerMarkerIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeSyncMarkerSource(t, dir, "avatar.go", `package avatar

import "github.com/tjbdwanghaibo/roost-core/framework/entity"

const EntityKindAvatar entity.EntityKind = 141

func AvatarPacker(entity.IThreadSafeEntity) entity.SubjectSyncPacker { return nil }

//roost:entity entityKind=EntityKindAvatar sync=true syncPacker=AvatarPacker
type Avatar struct {
	*entity.EntityBase
	entity.ComponentManager
	entity.DaoManager
}
`)
	err := Run([]string{"-dir", dir, "-force"}, os.Stdout)
	if err == nil {
		t.Fatal("retired packer marker was accepted")
	}
	if !strings.Contains(err.Error(), "use subjectPacker") {
		t.Errorf("refusal does not say what to do: %v", err)
	}
}

// A packer without sync=true is a marker that does nothing; saying so beats
// generating an entity whose factory is never used.
func TestPackerWithoutSyncIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeSyncMarkerSource(t, dir, "avatar.go", `package avatar

import "github.com/tjbdwanghaibo/roost-core/framework/entity"

const EntityKindAvatar entity.EntityKind = 141

func AvatarPacker(entity.IThreadSafeEntity) entity.SubjectSyncPacker { return nil }

//roost:entity entityKind=EntityKindAvatar subjectPacker=AvatarPacker
type Avatar struct {
	*entity.EntityBase
	entity.ComponentManager
	entity.DaoManager
}
`)
	if err := Run([]string{"-dir", dir, "-force"}, os.Stdout); err == nil {
		t.Fatal("a packer marker without sync=true was accepted")
	}
}
