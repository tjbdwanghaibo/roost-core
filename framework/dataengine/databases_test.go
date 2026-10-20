package dataengine

import (
	"errors"
	"strings"
	"testing"
)

// 映射表的合成规则：回调非空结果 > 配置 > 逻辑名本身；回调对每个声明的逻辑库只调用一次；表冻结后查表不再调用回调。
func TestDatabaseMapPriorityAndResolverCalledOncePerLogicalDatabase(t *testing.T) {
	calls := map[string]int{}
	resolver := func(logical string) (string, error) {
		calls[logical]++
		if logical == "audit" {
			return "audit_eu", nil
		}
		return "", nil // 不表态：沿用配置或逻辑名
	}
	databases, warnings, err := NewDatabaseMap(DatabaseMapConfig{
		Declared:   []LogicalDatabase{{Name: "main"}, {Name: "audit"}, {Name: "logs"}},
		Configured: map[string]string{"main": "planet", "audit": "audit_cfg"},
		Resolver:   resolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	for logical, want := range map[string]string{"main": "planet", "audit": "audit_eu", "logs": "logs", "undeclared": "undeclared", "": ""} {
		if got := databases.Physical(logical); got != want {
			t.Errorf("Physical(%q) = %q, want %q", logical, got, want)
		}
	}
	for _, logical := range []string{"main", "audit", "logs"} {
		if calls[logical] != 1 {
			t.Errorf("resolver called %d times for %q, want exactly once at construction", calls[logical], logical)
		}
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, `"audit_eu", overriding dataengine.databases ("audit_cfg")`) || !strings.Contains(joined, `logical database "logs" has no mapping`) {
		t.Fatalf("warnings = %q; want the resolver override and the identity fallback named", warnings)
	}
	var nilMap *DatabaseMap
	if nilMap.Physical("main") != "main" {
		t.Fatal("a nil map must map every logical name to itself")
	}
}

// 加载时一次校验，失败点名：未声明的逻辑名（拼写）、非法物理名、系统库、加 _<sid> 后超长、两个逻辑库同一物理库、回调错误。
func TestDatabaseMapRefusesEachInvalidMapping(t *testing.T) {
	declared := []LogicalDatabase{{Name: "main", ServerScoped: true}, {Name: "audit"}}
	long52 := strings.Repeat("a", 52)
	cases := []struct {
		name     string
		cfg      DatabaseMapConfig
		wantText string
	}{
		{"unknown logical name", DatabaseMapConfig{Declared: declared, Configured: map[string]string{"mian": "x"}}, `dataengine.databases.mian: no DAO declares logical database "mian"`},
		{"illegal character", DatabaseMapConfig{Declared: declared, Configured: map[string]string{"main": "planet.x"}}, `contains a character Mongo does not allow`},
		{"system database", DatabaseMapConfig{Declared: declared, Configured: map[string]string{"audit": "Admin"}}, `is a Mongo system database`},
		{"too long with the sid suffix", DatabaseMapConfig{Declared: declared, Configured: map[string]string{"main": long52 + "a"}, ServerID: 1234567890}, `with the _<sid> suffix`},
		{"global name too long", DatabaseMapConfig{Declared: declared, Configured: map[string]string{"audit": long52 + "b" + strings.Repeat("c", 11)}}, `64 bytes; Mongo allows 63`},
		{"two logical databases on one physical database", DatabaseMapConfig{Declared: declared, Configured: map[string]string{"main": "planet", "audit": "PLANET"}}, `both map to physical database`},
		{"identity collides with a mapping", DatabaseMapConfig{Declared: declared, Configured: map[string]string{"main": "audit"}}, `both map to physical database "audit"`},
		{"resolver error", DatabaseMapConfig{Declared: declared, Resolver: func(string) (string, error) { return "", errors.New("vault down") }}, `database resolver for logical database "audit": vault down`},
		{"resolver returns an illegal name", DatabaseMapConfig{Declared: declared, Resolver: func(string) (string, error) { return "a/b", nil }}, `(resolver)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			databases, _, err := NewDatabaseMap(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("NewDatabaseMap = %v, %v; want an error containing %q", databases, err, tc.wantText)
			}
		})
	}
	// 52 字节加最长的 _<sid>（11 字节）恰好 63，合法；53 字节只有 sid 足够短时合法。
	if _, _, err := NewDatabaseMap(DatabaseMapConfig{Declared: declared, Configured: map[string]string{"main": long52}, ServerID: 1234567890}); err != nil {
		t.Fatalf("52 bytes + _1234567890 is 63 bytes: %v", err)
	}
	if _, _, err := NewDatabaseMap(DatabaseMapConfig{Declared: declared, Configured: map[string]string{"main": long52 + "a"}, ServerID: 1000}); err != nil {
		t.Fatalf("53 bytes + _1000 is 58 bytes: %v", err)
	}
}

// 代码一个逻辑库都没声明（工程还没有 DAO）时，配置里的名字无从判断拼写：只告警，物理名照常校验。
func TestDatabaseMapWithoutDeclarationsOnlyWarnsAboutUnknownNames(t *testing.T) {
	databases, warnings, err := NewDatabaseMap(DatabaseMapConfig{Configured: map[string]string{"main": "planet"}})
	if err != nil || databases.Physical("main") != "planet" || len(warnings) != 1 || !strings.Contains(warnings[0], "cannot be checked") {
		t.Fatalf("map=%v warnings=%q err=%v", databases, warnings, err)
	}
	if err := ValidateDatabaseConfig(nil, map[string]string{"main": "local"}, 0); err == nil {
		t.Fatal("a system database accepted because nothing is declared")
	}
}

func TestDeclareDatabasesMergesServerScopeAndRefusesBadNames(t *testing.T) {
	DeclareDatabases(LogicalDatabase{Name: "decl_test_db"})
	DeclareDatabases(LogicalDatabase{Name: "decl_test_db", ServerScoped: true})
	found := false
	for _, database := range DeclaredDatabases() {
		if database.Name == "decl_test_db" {
			found = database.ServerScoped
		}
	}
	if !found {
		t.Fatal("a later sid-scoped declaration of the same name must mark it sid-scoped")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("an uppercase logical name was declared")
		}
	}()
	DeclareDatabases(LogicalDatabase{Name: "Main"})
}
