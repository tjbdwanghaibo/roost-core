package roost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// U-0152 · C2 · add.go:299（U-0151 留待）：commitManifestSyncResult 里"同步失败且回滚也失败"只在 SyncProject
// 执行期间清单被并发改写或 I/O 故障时可达，外部无法构造，记为 rollbackSync 错误的防御性包装。这里钉住
// rollbackSync 尚未覆盖的另一条失败分支：回滚前连文件都读不了（路径成了目录）要报"inspect ... before
// rollback"，而不是把目录当成"文件不存在"去重建。
func TestRollbackSyncReportsAFileItCannotInspect(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "roost.yaml")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	err := rollbackSync([]syncChange{{rel: "roost.yaml", path: path, before: []byte("before\n"), body: []byte("after\n"), existed: true}})
	if err == nil || !strings.Contains(err.Error(), "inspect roost.yaml before rollback") {
		t.Fatalf("rollbackSync over a directory = %v", err)
	}
	if info, statErr := os.Stat(path); statErr != nil || !info.IsDir() {
		t.Fatal("rollback replaced the directory it could not inspect")
	}
}
