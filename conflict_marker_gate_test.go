package roostcore_test

import (
	"errors"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// 合并冲突标记门禁（2026-10-06，revleft 小防护 A）。
//
// 0aa2e1b9（80902948 rebase 后的提交）把一段未解决的冲突（`<<<<<<< HEAD` / `=======` /
// `>>>>>>> 80902948 ...`）带进了 main 的接力清单，直到 2c1c7be7 才被顺手删掉；build、vet、
// 测试全绿，因为冲突落在 Markdown 里，没有任何检查读它。这里在普通测试里扫一遍全部跟踪文件。
//
// 判定：一个文件只要有一行以 `<<<<<<<` 或 `>>>>>>>` 开头（后接空格或行尾），就是残留冲突；
// 单独的 `=======` 行是 Markdown 的 setext 标题下划线，合法，只在同一文件已有前两种标记时才一并列出。
// artifacts/ 是本地产物目录（被忽略，可能保存源码备份），排除。

// conflictMarkerPattern 交给 git grep -E：三种标记行。
const conflictMarkerPattern = `^(<<<<<<<|>>>>>>>)( |$)|^=======$`

// conflictIncidentCommit 是真实出过事的提交；历史里有它时，扫描它必须报出那份接力清单。
const (
	conflictIncidentCommit = "0aa2e1b9"
	conflictIncidentFile   = "docs/review/REMAINING-REVIEW-HANDOFF-2026-10-05.md"
)

func TestNoMergeConflictMarkersInTrackedFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if out, err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Skip("not inside a git work tree (module zip / vendored copy)")
	}

	// 自检：门禁能抓到真实出过事的那一次（浅克隆里没有这个提交时跳过自检，不跳过正式扫描）。
	if exec.Command("git", "cat-file", "-e", conflictIncidentCommit+"^{commit}").Run() == nil {
		incident, err := conflictMarkerFiles(conflictIncidentCommit)
		if err != nil {
			t.Fatalf("scan %s: %v", conflictIncidentCommit, err)
		}
		if _, ok := incident[conflictIncidentFile]; !ok {
			t.Errorf("the gate does not see the conflict that %s brought into %s; the matcher is broken (found %v)", conflictIncidentCommit, conflictIncidentFile, incident)
		}
	}

	found, err := conflictMarkerFiles("")
	if err != nil {
		t.Fatalf("scan working tree: %v", err)
	}
	files := make([]string, 0, len(found))
	for file := range found {
		files = append(files, file)
	}
	sort.Strings(files)
	for _, file := range files {
		t.Errorf("%s still carries merge conflict markers (resolve the merge before committing):\n  %s", file, strings.Join(found[file], "\n  "))
	}
}

// conflictMarkerFiles 用 git grep 扫描 rev（空串为工作区里的跟踪文件），返回带残留冲突的文件及其标记行。
func conflictMarkerFiles(rev string) (map[string][]string, error) {
	args := []string{"grep", "-n", "-I", "-E", conflictMarkerPattern}
	if rev != "" {
		args = append(args, rev)
	}
	args = append(args, "--", ".", ":(exclude)artifacts")
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return map[string][]string{}, nil // git grep：没有匹配
		}
		return nil, err
	}
	lines := map[string][]string{}
	hasAngleMarker := map[string]bool{}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if rev != "" {
			line = strings.TrimPrefix(line, rev+":")
		}
		// file:lineno:text
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		file, text := parts[0], parts[2]
		lines[file] = append(lines[file], parts[1]+": "+text)
		if strings.HasPrefix(text, "<<<<<<<") || strings.HasPrefix(text, ">>>>>>>") {
			hasAngleMarker[file] = true
		}
	}
	found := map[string][]string{}
	for file, hits := range lines {
		if hasAngleMarker[file] {
			found[file] = hits
		}
	}
	return found, nil
}
