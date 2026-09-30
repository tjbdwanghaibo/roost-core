package roost

// RR-20260930-16：add mod / add saga 给服务配置追加新 Mod 段（appendModConfigSections）时沿用文件原有的
// 行尾：开发配置与生产示例按 CRLF 检出（Windows core.autocrlf=true）就按 CRLF 追加，LF 文件与空文件
// 保持 LF。旧行为：无论文件是什么行尾，新段一律按 LF 追加，一份文件里 CRLF 与 LF 混用（YAML 照样能读，
// 但 diff、编辑器与 .gitattributes 的行尾检查都会报；下一次停机块刷新或 add transport tcp 才把整份
// 写回 CRLF）。Secret 示例早在 RR-20260928-13 就按原行尾写回，这里补齐另外两份文件。

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// modConfigTargets 是 appendModConfigSections 直接改写的两份文件；Secret 走 RR-20260928-13 的路径。
var modConfigTargets = []string{
	"configs/service/config.game.yaml",
	"configs/service/config.game.prod.example.yaml",
}

func TestAddedConfigSectionsFollowTheFilesLineEndings(t *testing.T) {
	t.Parallel()
	t.Run("crlf files get crlf sections", func(t *testing.T) {
		t.Parallel()
		root := copyOfNewProject(t, "saga")
		for _, rel := range modConfigTargets {
			rewriteFile(t, root, rel, func(raw []byte) []byte {
				return bytes.ReplaceAll(raw, []byte("\n"), []byte("\r\n"))
			})
		}
		if _, err := Add(root, AddOptions{Kind: "mod", Name: "redis", Service: "game"}); err != nil {
			t.Fatalf("add mod redis: %v", err)
		}
		for _, rel := range modConfigTargets {
			got := readProjectBytes(t, root, rel)
			if !bytes.Contains(got, []byte("\r\nredis:\r\n")) {
				t.Errorf("%s: add mod redis did not append a CRLF redis: section:\n%q", rel, tailBytes(got, 200))
			}
			if bareLineFeed.Match(got) {
				t.Errorf("%s: a CRLF file now has LF-only lines after add mod (the appended section used LF):\n%q", rel, tailBytes(got, 200))
			}
		}
	})
	t.Run("lf files stay lf", func(t *testing.T) {
		t.Parallel()
		root := copyOfNewProject(t, "saga")
		if _, err := Add(root, AddOptions{Kind: "mod", Name: "redis", Service: "game"}); err != nil {
			t.Fatalf("add mod redis: %v", err)
		}
		for _, rel := range modConfigTargets {
			got := readProjectBytes(t, root, rel)
			if !bytes.Contains(got, []byte("\nredis:\n")) {
				t.Errorf("%s: add mod redis did not append a redis: section", rel)
			}
			if bytes.Contains(got, []byte("\r")) {
				t.Errorf("%s: an LF file gained a carriage return", rel)
			}
		}
	})
	t.Run("an empty dev config gets lf sections", func(t *testing.T) {
		t.Parallel()
		root := copyOfNewProject(t, "saga")
		rewriteFile(t, root, modConfigTargets[0], func([]byte) []byte { return nil })
		if _, err := Add(root, AddOptions{Kind: "mod", Name: "redis", Service: "game"}); err != nil {
			t.Fatalf("add mod redis: %v", err)
		}
		got := readProjectBytes(t, root, modConfigTargets[0])
		if !bytes.HasPrefix(got, []byte("redis:\n")) || bytes.Contains(got, []byte("\r")) {
			t.Errorf("empty dev config after add mod redis = %q, want an LF redis: section", got)
		}
	})
}

func rewriteFile(t *testing.T, root, rel string, edit func([]byte) []byte) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, edit(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readProjectBytes(t *testing.T, root, rel string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func tailBytes(raw []byte, n int) []byte {
	if len(raw) <= n {
		return raw
	}
	return raw[len(raw)-n:]
}
