package dao

import (
	"strings"
	"testing"
)

// U-0151 · C2 · gap map codegen `internal/dao` 5/20：redisdao 标记无参数 / 缺 key 与 prefix / 绑到多个结构体 /
// 孤立无结构体各报其错；dao 标签同时写 sync 与 nosync 拒绝。
func TestParseRefusesEachMalformedRedisMarkerAndConflictingDaoTag(t *testing.T) {
	cases := []struct{ name, source, want string }{
		{"redisdao without parameters", "package def\n\n//roost:redisdao\ntype HeroCache struct {\n\tName string\n}\n", "//roost:redisdao marker is missing parameters"},
		{"redisdao missing key and prefix", "package def\n\n//roost:redisdao mode=ref-hmap\ntype HeroCache struct {\n\tName string\n}\n", "requires key= and prefix="},
		{"redisdao binding a group", "package def\n\n//roost:redisdao mode=ref-hmap key=id prefix=hero\ntype (\n\tAlphaCache struct{ Name string }\n\tBetaCache  struct{ Name string }\n)\n", "binds to multiple structs"},
		{"orphan redisdao", "package def\n\n//roost:redisdao mode=ref-hmap key=id prefix=hero\n\nvar unrelated = 1\n\ntype HeroCache struct {\n\tName string\n}\n", "not attached to a struct"},
		{"dao tag sync and nosync", "package def\n\n//roost:dao coll=heroes db=game\ntype HeroDao struct {\n\tName string `dao:\"sync,nosync\"`\n}\n", "states both sync and nosync"},
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
