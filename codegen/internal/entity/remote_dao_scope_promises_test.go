package entity

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// RR-20260926-45 · remote=managed 实体不能使用 dbscope=sid 的 DAO。
//
// 托管实体可以被任一进程写入、所有权可以迁移；提交按提交方 sid 选库、加载按本服 sid 选库，
// 两边不一致会让实体在跨服写或迁移后读到旧版本 / 缺失，从而永久不可写。生成时直接拒绝，
// 并给出改 dbscope=global 的指引；不落任何 wire 文件。
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
