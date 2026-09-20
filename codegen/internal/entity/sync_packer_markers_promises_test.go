package entity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// U-0229 · C4 · RR-20260918-01：sync=true 的生成物必须只写 Core 的
// EntitySyncBuilderParam 真有的字段（Enabled / Topic / PackerFactory）。旧行为：
// 还写了 FlushPolicy（字段与常量都已不存在）和 SubjectPackerFactory（从未存在），
// 于是任何 sync=true 的工程编译不过，而本包只比对文本，一直全绿。
// 两种 packer 标记（subjectPacker 现行、syncPacker 旧写法）指的是同一个字段，
// 同时给两个要当场报错，而不是任选其一。

func writeSyncMarkerSource(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedSyncBlockOnlyNamesFieldsCoreHas(t *testing.T) {
	for name, marker := range map[string]string{
		"current spelling": "//roost:entity entityKind=EntityKindAvatar sync=true syncTopic=\"avatar\" subjectPacker=AvatarPacker",
		"legacy spelling":  "//roost:entity entityKind=EntityKindAvatar sync=true syncTopic=\"avatar\" syncPacker=AvatarPacker",
		"no packer":        "//roost:entity entityKind=EntityKindAvatar sync=true syncTopic=\"avatar\"",
	} {
		dir := t.TempDir()
		writeSyncMarkerSource(t, dir, "avatar.go", `package avatar

import "github.com/tjbdwanghaibo/roost-core/entity"

const EntityKindAvatar entity.EntityKind = 141
const SyncTopicAvatar = "avatar"

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
		if !strings.Contains(collapsed, "Enabled: true") || !strings.Contains(collapsed, `Topic: "avatar"`) {
			t.Errorf("%s: sync block lost its enabled/topic wiring:\n%s", name, generated)
		}
		wantFactory := strings.Contains(marker, "Packer=")
		if got := strings.Contains(collapsed, "PackerFactory: AvatarPacker"); got != wantFactory {
			t.Errorf("%s: PackerFactory present = %v, want %v:\n%s", name, got, wantFactory, generated)
		}
	}
}

// One field, one value: a marker naming both spellings is a contradiction, so
// it is refused with the actionable message rather than resolved silently.
func TestBothPackerMarkersAreRefused(t *testing.T) {
	dir := t.TempDir()
	writeSyncMarkerSource(t, dir, "avatar.go", `package avatar

import "github.com/tjbdwanghaibo/roost-core/entity"

const EntityKindAvatar entity.EntityKind = 141

func AvatarPacker(entity.IThreadSafeEntity) entity.SubjectSyncPacker { return nil }

//roost:entity entityKind=EntityKindAvatar sync=true syncPacker=AvatarPacker subjectPacker=AvatarPacker
type Avatar struct {
	*entity.EntityBase
	entity.ComponentManager
	entity.DaoManager
}
`)
	err := Run([]string{"-dir", dir, "-force"}, os.Stdout)
	if err == nil {
		t.Fatal("generating an entity that sets both packer markers was accepted")
	}
	if !strings.Contains(err.Error(), "they name the same factory") {
		t.Errorf("refusal does not say what to do: %v", err)
	}
}

// A packer without sync=true is a marker that does nothing; saying so beats
// generating an entity whose factory is never used.
func TestPackerWithoutSyncIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeSyncMarkerSource(t, dir, "avatar.go", `package avatar

import "github.com/tjbdwanghaibo/roost-core/entity"

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
