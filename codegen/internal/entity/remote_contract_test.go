package entity

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteCapableMarkerIsRejectedWithTheCategoryReplacement(t *testing.T) {
	for _, value := range []string{"capable", "true", "yes", "1", "on"} {
		err := validateMarkerValues(map[string]string{"remote": value})
		if err == nil {
			t.Errorf("remote=%s was accepted; it must be refused", value)
			continue
		}
		for _, want := range []string{"no longer supported", "category"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("remote=%s error %q does not mention %q", value, err, want)
			}
		}
	}

	// The surviving spellings still parse, and an unrelated typo still gets the
	// plain "not one of" message rather than the migration hint. remote=mirror
	// is a migration error since Mirror step 5 (mirror_test.go).
	for _, value := range []string{"none", "managed", "no", "false", "off", ""} {
		if err := validateMarkerValues(map[string]string{"remote": value}); err != nil {
			t.Errorf("remote=%q must still be accepted: %v", value, err)
		}
	}
	err := validateMarkerValues(map[string]string{"remote": "bogus"})
	if err == nil || strings.Contains(err.Error(), "no longer supported") {
		t.Fatalf("remote=bogus should get the plain spelling error, got %v", err)
	}
	if !strings.Contains(err.Error(), "none|managed") {
		t.Errorf("remote=bogus error %q does not list the accepted spellings", err)
	}
}

func TestRemoteManagedEntityRejectsServerScopedDao(t *testing.T) {
	const daoSid = "package db\n\nimport \"github.com/tjbdwanghaibo/roost-core/framework/dataengine\"\n\ntype GuildDao struct{}\n\nfunc (d *GuildDao) DbScope() dataengine.DatabaseScope { return dataengine.DatabaseServer }\n"
	const daoGlobal = "package db\n\nimport \"github.com/tjbdwanghaibo/roost-core/framework/dataengine\"\n\ntype GuildDao struct{}\n\nfunc (d *GuildDao) DbScope() dataengine.DatabaseScope { return dataengine.DatabaseGlobal }\n"
	entitySource := func(remote string, daoType string, imports ...string) string {
		var b strings.Builder
		b.WriteString("package guild\n\nimport (\n")
		for _, imp := range imports {
			b.WriteString("\t\"" + imp + "\"\n")
		}
		b.WriteString("\t\"github.com/tjbdwanghaibo/roost-core/framework/entity\"\n)\n\nconst EntityKindGuild entity.EntityKind = 5\n\n")
		base := "*entity.EntityBase"
		marker := "//roost:entity entityKind=EntityKindGuild"
		if remote != "" {
			base = "*entity.RemoteEntityBase"
			marker += " remote=" + remote
		}
		b.WriteString(marker + "\ntype Guild struct {\n\t" + base + "\n\tentity.DaoManager\n\tdao " + daoType + " `dao:\"guild\"`\n}\n")
		return b.String()
	}
	cases := []struct {
		name   string
		files  map[string]string
		reject bool
	}{
		{"managed/external sid", map[string]string{
			"db/gen_guild_dao.go":          daoSid,
			"game/entities/guild/guild.go": entitySource("managed", "*db.GuildDao", "example.com/game/db"),
		}, true},
		{"managed/same-package sid", map[string]string{
			"game/entities/guild/guild.go":   entitySource("managed", "*GuildDao"),
			"game/entities/guild/gen_dao.go": strings.Replace(daoSid, "package db", "package guild", 1),
		}, true},
		{"managed/external global", map[string]string{
			"db/gen_guild_dao.go":          daoGlobal,
			"game/entities/guild/guild.go": entitySource("managed", "*db.GuildDao", "example.com/game/db"),
		}, false},
		{"managed/no DbScope method", map[string]string{
			"db/gen_guild_dao.go":          "package db\n\ntype GuildDao struct{}\n",
			"game/entities/guild/guild.go": entitySource("managed", "*db.GuildDao", "example.com/game/db"),
		}, false},
		{"local/external sid", map[string]string{
			"db/gen_guild_dao.go":          daoSid,
			"game/entities/guild/guild.go": entitySource("", "*db.GuildDao", "example.com/game/db"),
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.files["go.mod"] = "module example.com/game\n\ngo 1.27.0\n"
			for name, content := range tc.files {
				path := filepath.Join(root, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			err := Run([]string{"-dir", filepath.Join(root, "game")}, io.Discard)
			wire := filepath.Join(root, "game", "entities", "guild", "guild_gen_wire.go")
			_, statErr := os.Stat(wire)
			if !tc.reject {
				if err != nil {
					t.Fatalf("legal combination rejected: %v", err)
				}
				if statErr != nil {
					t.Fatalf("legal combination produced no wire file: %v", statErr)
				}
				return
			}
			if err == nil {
				t.Fatal("remote=managed entity with a dbscope=sid DAO was generated; it must be refused")
			}
			for _, want := range []string{"remote=managed", "dbscope=sid", "dbscope=global", "GuildDao"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			if statErr == nil {
				t.Fatal("a refused generation still wrote the wire file")
			}
		})
	}
}

// 远程提交守卫在生成代码里，本仓库没有测试能执行它们：生成器为每个带热 DAO 的 remote-managed 实体在业务工程里生成配套测试，
// 未变化时对账保持，实体不再是 remote-managed 时删掉旧测试（留着会编译失败）。
func TestGenerateEmitsAndReconcilesRemoteGuardTests(t *testing.T) {
	entities, pkg, err := parseDir(filepath.Join("testdata", "remote"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entities) != 1 {
		t.Fatalf("entities = %d", len(entities))
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "guild_gen_wire.go")
	if _, err := generate(entities[0], pkg, out, true); err != nil {
		t.Fatal(err)
	}
	testFile := guardTestFileFor(out)
	raw, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("companion guard test was not generated: %v", err)
	}
	generated := string(raw)
	for _, required := range []string{
		"// Code generated by tool/entity. DO NOT EDIT.",
		"func TestGuildRemoteCommitGuards(t *testing.T)",
		"RegisterEntity()",
		"NewGuild(&entity.EntityCreateParam{IsCreate: true, UniqueID: 1, Kind: EntityKindGuild})",
		"missing transaction-local remote persistence changes",
		"DAO-level remote delete requires entity removal",
		"remote acknowledgement identity mismatch",
	} {
		if !strings.Contains(generated, required) {
			t.Errorf("guard test missing %q", required)
		}
	}

	// Unchanged wiring, deleted companion: a plain run restores it.
	if err := os.Remove(testFile); err != nil {
		t.Fatal(err)
	}
	changed, err := generate(entities[0], pkg, out, false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("restoring the companion test was not reported as a change")
	}
	if _, err := os.Stat(testFile); err != nil {
		t.Fatalf("companion test not restored on an unchanged run: %v", err)
	}

	// The entity stops being remote-managed: the stale companion is removed.
	local := entities[0]
	local.RemotePolicy = ""
	if _, err := generate(local, pkg, out, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(testFile); !os.IsNotExist(err) {
		t.Fatalf("stale companion test survived the entity losing remote management: %v", err)
	}
}
