package roost

import (
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// U-0156 · C2 · RR-20260908-03：旧业务文件只有一条不带括号的 import，又同时用到留在 kit 的
// Mod 胶水与搬到 core 的符号时，"混合"分流要新增一个 import。此前新 ImportSpec 的文本被
// 无条件塞在第一个 spec 之后、默认外面有 `import (...)`，单行输入得到一条裸露的
// `coreredis "…"`，format 失败，整个文件升级失败。
func TestConsolidateSplitsASingleLineImportIntoAValidSecondDeclaration(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "go.mod", legacyGoMod)
	m := DefaultManifest("planet", "example.com/planet", nil, nil, nil)
	raw, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, root, ManifestName, string(raw))
	consumer := writeProjectFile(t, root, "internal/consumer/consumer.go",
		"package consumer\n\nimport \"github.com/tjbdwanghaibo/roost-kit/redis\"\n\nvar _ = redis.NewRedisMod\nvar _ = redis.NewClient\n")
	if _, err := ConsolidateProject(root, false, nil); err != nil {
		t.Fatalf("single-line import split rejected: %v", err)
	}
	rewritten, err := os.ReadFile(consumer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := format.Source(rewritten); err != nil {
		t.Fatalf("rewritten file does not format: %v\n%s", err, rewritten)
	}
	text := string(rewritten)
	for _, want := range []string{
		`"github.com/tjbdwanghaibo/roost-kit/redis"`,
		`coreredis "github.com/tjbdwanghaibo/roost-core/redis/driver"`,
		"redis.NewRedisMod",
		"coreredis.NewClient",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("rewritten file lacks %q:\n%s", want, text)
		}
	}
	_ = filepath.Join
}
