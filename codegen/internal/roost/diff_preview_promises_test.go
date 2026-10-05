package roost

// RR-20261005-NC-71：`roost project diff` 与 `roost project upgrade --dry-run` 是 sync / upgrade 的预览
// （“只显示将发生的变化”“preview an upgrade without writing files”）。SyncProject 渲染模板之前会把
// 应用自有配置里未手改的生成 shutdown: 块刷新到当前 Mod 的计划（RR-20260926-66/80：
// configs/service/config.<svc>.yaml、config.<svc>.prod.example.yaml、deploy/k8s/base/secret.<svc>.example.yaml），
// 模板的宽限期再按刷新后的值渲染。旧预览只按磁盘上的旧配置渲染模板、且跳过已存在的应用自有文件，
// 于是 Mod 增减或升级跨过停机预算公式变化时，预览漏掉这三份应用自有配置——sync 会改它们，用户在
// 预览里却看不到。承诺：预览列出的文件集合等于随后 sync 实际改写的文件集合，预览本身不写工程。

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func previewedPaths(output string) []string {
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" || strings.HasPrefix(line, "summary:") {
			continue
		}
		paths = append(paths, line)
	}
	slices.Sort(paths)
	return paths
}

func syncedPaths(result SyncResult) []string {
	paths := slices.Concat(result.Created, result.Updated, result.Removed)
	slices.Sort(paths)
	return paths
}

func snapshotTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		files[filepath.ToSlash(rel)] = raw
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

func assertTreeUnchanged(t *testing.T, what string, before, after map[string][]byte) {
	t.Helper()
	for rel, body := range before {
		if got, ok := after[rel]; !ok || !bytes.Equal(got, body) {
			t.Errorf("%s changed %s", what, rel)
		}
	}
	for rel := range after {
		if _, ok := before[rel]; !ok {
			t.Errorf("%s created %s", what, rel)
		}
	}
}

func TestProjectDiffListsEveryFileTheNextSyncRewrites(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "configdata")
	editManifest(t, root, func(m *Manifest) {
		spec := m.Services["game"]
		spec.Mods = append(spec.Mods, "redis")
		m.Services["game"] = spec
	})
	before := snapshotTree(t, root)
	var diff bytes.Buffer
	if err := DiffProject(root, &diff); err != nil {
		t.Fatal(err)
	}
	assertTreeUnchanged(t, "project diff", before, snapshotTree(t, root))
	result, err := SyncProject(root)
	if err != nil {
		t.Fatal(err)
	}
	previewed, synced := previewedPaths(diff.String()), syncedPaths(result)
	if !slices.Equal(previewed, synced) {
		t.Fatalf("project diff previewed %d files, the sync rewrote %d\n missing from the preview: %v\n previewed but not rewritten: %v",
			len(previewed), len(synced), missingFrom(previewed, synced), missingFrom(synced, previewed))
	}
	for _, rel := range []string{"configs/service/config.game.yaml", "configs/service/config.game.prod.example.yaml", "deploy/k8s/base/secret.game.example.yaml"} {
		if !slices.Contains(previewed, rel) {
			t.Errorf("project diff did not preview the shutdown refresh of %s", rel)
		}
	}
	assertProjectDiffEmpty(t, root)
}

func TestUpgradeDryRunListsEveryFileTheUpgradeRewrites(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "configdata")
	editManifest(t, root, func(m *Manifest) {
		spec := m.Services["game"]
		spec.Mods = append(spec.Mods, "redis")
		m.Services["game"] = spec
	})
	before := snapshotTree(t, root)
	var preview bytes.Buffer
	if err := Run([]string{"project", "upgrade", "--root", root, "--dry-run", "-core", "latest"}, &preview, io.Discard); err != nil {
		t.Fatalf("upgrade --dry-run: %v", err)
	}
	assertTreeUnchanged(t, "upgrade --dry-run", before, snapshotTree(t, root))
	manifestBefore, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	m, err := loadManifestForUpgrade(root)
	if err != nil {
		t.Fatal(err)
	}
	mergeVersions(&m.Versions, VersionSpec{Core: "latest"})
	result, err := commitManifestSyncResult(context.Background(), root, manifestBefore, m)
	if err != nil {
		t.Fatal(err)
	}
	previewed := previewedPaths(preview.String())
	previewed = slices.DeleteFunc(previewed, func(rel string) bool { return rel == ManifestName })
	if synced := syncedPaths(result); !slices.Equal(previewed, synced) {
		t.Fatalf("upgrade --dry-run previewed %d files, the upgrade rewrote %d\n missing from the preview: %v\n previewed but not rewritten: %v",
			len(previewed), len(synced), missingFrom(previewed, synced), missingFrom(synced, previewed))
	}
}

// missingFrom returns the entries of want that got lacks.
func missingFrom(got, want []string) []string {
	var out []string
	for _, rel := range want {
		if !slices.Contains(got, rel) {
			out = append(out, rel)
		}
	}
	return out
}
