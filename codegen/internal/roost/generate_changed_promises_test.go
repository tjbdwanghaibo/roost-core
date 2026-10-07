package roost

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// RR-20261006-58：`generate --changed` 只跑改动涉及的生成器，判断依据是
// git 给出的改动路径与生成器前缀（相对工程根）的比较。
//
// 旧行为：`git status --porcelain` 的路径总是相对**仓库根**，工程在仓库子目录
// （monorepo、examples/ 下的工程）时 `sub/proj/db/def/x.go` 永远不以 `db/def/`
// 开头；带空格的路径被 porcelain 加上引号（`"sub/proj/db/def/a b.go"`），也匹配
// 不上。结果除 Always 的 registry 外一个生成器都不跑，命令照样报成功。

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateChangedSeesChangesOfAProjectInARepositorySubdirectory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	project := filepath.Join(repo, "sub", "proj")
	writeFile(t, filepath.Join(project, "protocol", "def", "game.go"), "package def\n")
	writeFile(t, filepath.Join(repo, "other", "x.go"), "package other\n")
	gitIn(t, repo, "init", "-q")
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-q", "-m", "base")

	writeFile(t, filepath.Join(project, "protocol", "def", "game.go"), "package def\n\n// changed\n")
	writeFile(t, filepath.Join(project, "db", "def", "hero cache.go"), "package def\n")
	writeFile(t, filepath.Join(repo, "other", "y.go"), "package other\n")

	changed, err := gitChanged(project)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(changed)
	want := []string{"db/def/hero cache.go", "protocol/def/game.go"}
	if !slices.Equal(changed, want) {
		t.Fatalf("gitChanged = %q, want %q (paths relative to the project root, unquoted, nothing outside the project)", changed, want)
	}

	gens := []generator{
		{Name: "dao", Prefixes: []string{"db/def/"}},
		{Name: "protocol", Prefixes: []string{"protocol/def/"}},
		{Name: "tables", Prefixes: []string{"config/meta/"}},
		{Name: "registry", Always: true},
	}
	var names []string
	for _, gen := range filterChanged(gens, changed) {
		names = append(names, gen.Name)
	}
	if !slices.Equal(names, []string{"dao", "protocol", "registry"}) {
		t.Fatalf("filterChanged selected %q, want dao, protocol and registry", names)
	}
}

// A rename moves a definition out of a generator's input; the generator must
// still run so it can retire what the old file produced.
func TestGenerateChangedCountsBothSidesOfARename(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "db", "def", "hero.go"), "package def\n")
	gitIn(t, repo, "init", "-q")
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-q", "-m", "base")
	if err := os.MkdirAll(filepath.Join(repo, "attic"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "mv", "db/def/hero.go", "attic/hero.go")

	changed, err := gitChanged(repo)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(changed)
	if want := []string{"attic/hero.go", "db/def/hero.go"}; !slices.Equal(changed, want) {
		t.Fatalf("gitChanged = %q, want %q", changed, want)
	}
}
