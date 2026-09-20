package dao

import (
	"path/filepath"
	"strings"
	"testing"
)

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
