package errcode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// RR-20261005-NC-63：使用说明承诺 errcode 扫描“重复 code/name 或非常量调用失败”。旧实现只查重复
// code：编号或名字不是字面量的 Define（`errcode.Define(CodeX, ...)`）、以别名导入的 errcode 包，
// 都被悄悄跳过——既不进导出表，也逃过重复检查（`roost id check` 用同样的字面量正则，也看不见）；
// 两个不同编号用同一个名字也照常导出。承诺：这些情形生成期失败并指出文件；字面量定义照常导出。
func TestExtractDefinitionsRefusesWhatItCannotRead(t *testing.T) {
	const imp = "import \"github.com/tjbdwanghaibo/roost-core/infra/base/errcode\"\n\n"
	cases := []struct {
		label string
		files map[string]string
		want  []string
	}{
		{
			label: "code from a constant",
			files: map[string]string{"game/shop/errors.go": "package shop\n\n" + imp + "const CodeSoldOut = 500102\n\nvar ErrSoldOut = errcode.Define(CodeSoldOut, \"shop.sold_out\", \"sold out\")\n"},
			want:  []string{"game/shop/errors.go", "integer and string literals"},
		},
		{
			label: "name from a constant",
			files: map[string]string{"game/shop/errors.go": "package shop\n\n" + imp + "const name = \"shop.sold_out\"\n\nvar ErrSoldOut = errcode.Define(500102, name, \"sold out\")\n"},
			want:  []string{"game/shop/errors.go", "integer and string literals"},
		},
		{
			label: "aliased import with a constant code",
			files: map[string]string{"game/shop/errors.go": "package shop\n\nimport ec \"github.com/tjbdwanghaibo/roost-core/infra/base/errcode\"\n\nconst CodeSoldOut = 500102\n\nvar ErrSoldOut = ec.Define(CodeSoldOut, \"shop.sold_out\", \"sold out\")\n"},
			want:  []string{"game/shop/errors.go", "integer and string literals"},
		},
		{
			label: "aliased import duplicating a code",
			files: map[string]string{
				"game/shop/errors.go":  "package shop\n\nimport ec \"github.com/tjbdwanghaibo/roost-core/infra/base/errcode\"\n\nvar ErrA = ec.Define(500101, \"shop.a\", \"a\")\n",
				"game/guild/errors.go": "package guild\n\n" + imp + "var ErrB = errcode.Define(500101, \"guild.b\", \"b\")\n",
			},
			want: []string{"duplicate errcode 500101"},
		},
		{
			label: "one name for two codes",
			files: map[string]string{
				"game/shop/errors.go":  "package shop\n\n" + imp + "var ErrA = errcode.Define(500101, \"shared\", \"a\")\n",
				"game/guild/errors.go": "package guild\n\n" + imp + "var ErrB = errcode.Define(500102, \"shared\", \"b\")\n",
			},
			want: []string{"duplicate errcode name \"shared\"", "shop", "guild"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.label, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, testCase.files)
			defs, err := extractDefinitions(root)
			if err == nil {
				t.Fatalf("scan accepted it and exported %d definitions: %#v", len(defs), defs)
			}
			for _, want := range testCase.want {
				if !strings.Contains(filepath.ToSlash(err.Error()), want) {
					t.Fatalf("error %q does not mention %q", err, want)
				}
			}
		})
	}

	// The literal form is still read, through an alias or a dot import too.
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"game/shop/errors.go":  "package shop\n\nimport ec \"github.com/tjbdwanghaibo/roost-core/infra/base/errcode\"\n\nvar ErrA = ec.Define(500101, \"shop.a\", \"a\")\n",
		"game/guild/errors.go": "package guild\n\n" + imp + "var ErrB = errcode.Define(500102, \"guild.b\", \"b\")\n",
		"game/dot/errors.go":   "package dot\n\nimport . \"github.com/tjbdwanghaibo/roost-core/infra/base/errcode\"\n\nvar ErrC = Define(500103, \"dot.c\", \"c\")\n",
	})
	defs, err := extractDefinitions(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 3 || defs[0].Code != 500101 || defs[1].Code != 500102 || defs[2].Code != 500103 {
		t.Fatalf("definitions = %#v", defs)
	}
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
