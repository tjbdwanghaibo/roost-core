package roostcore_test

import (
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 文档提交号门禁。
//
// 现行文档常写“已修复（`b717fd14`）”“代码为 `ca0cbddf`”。分支经 rebase / cherry-pick 合入 main 后，
// 这些提交号仍指向 rebase 前的旧提交：它们不在 main 的历史里，按文档追修复的人找不到，或者找到一份
// 没进 main、内容可能不同的版本。新用户试用第五轮登记时，KNOWN-LIMITS / CHANGELOG / docs 里有 10 个
// 这样的引用（`b717fd14`、`990af957`、`f8851f4a` 等），build、vet、测试全绿，因为没有检查读 Markdown。
//
// 判定：跟踪的 *.md 里，反引号括起来的 7～40 位小写十六进制串（至少一个数字、至少一个 a～f，排除纯数字的
// 时间戳 / 端口与纯字母单词），视为提交号，必须是 HEAD 的祖先（git rev-list HEAD 里有以它开头的提交）。
// 只查“能在 main 历史里找到”，不查它是不是文档所说的那个改动。
//
// 限制：纯数字的短 SHA（约 2% 的 8 位 SHA）与不带反引号的提交号不在检查范围；浅克隆（CI 默认 depth=1）
// 里历史不全，整条跳过——这条门禁在本地完整检出、release 工作流（fetch-depth: 0）里生效。
// 自己分支上的提交在 rebase 后同样会失效：文档里引用本轮提交，合入后要改成 main 上的提交号，或者引用
// 测试名 / CHANGELOG 条目而不写提交号。

// documentedCommitPattern 交给 git grep -E 粗筛候选行；精确判定由 commitTokenRE 与 looksLikeCommit 完成。
const documentedCommitPattern = "`[0-9a-f]{7,40}`"

var commitTokenRE = regexp.MustCompile("`([0-9a-f]{7,40})`")

// documentedCommitExemptions 是有意引用、不在 HEAD 历史里的提交（例如说明某个分支上的原始提交与最终合入
// 版本不同）。键是文档里写的短 SHA，值写明它在哪个分支、为什么必须保留。保持很小：能改成 main 上的提交号
// 就改；条目不再被任何文档引用、或已能在 HEAD 历史里找到时，本测试要求删掉它。
var documentedCommitExemptions = map[string]string{}

// docCommitIncident 是本门禁要抓的那一次：该提交的 KNOWN-LIMITS 写着 rebase 前的 `b717fd14`。
const (
	docCommitIncidentRev  = "cac62833"
	docCommitIncidentFile = "docs/maintenance/KNOWN-LIMITS.md"
	docCommitIncidentSHA  = "b717fd14"
)

func TestDocumentedCommitsAreReachableFromHEAD(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if out, err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Skip("not inside a git work tree (module zip / vendored copy)")
	}
	if out, err := exec.Command("git", "rev-parse", "--is-shallow-repository").Output(); err != nil || strings.TrimSpace(string(out)) != "false" {
		t.Skip("shallow clone: history is incomplete, documented commits cannot be resolved")
	}
	reachable, err := reachableCommits()
	if err != nil {
		t.Fatal(err)
	}

	// 自检：门禁能抓到登记时真实存在的那一处（历史里没有该提交时跳过自检，不跳过正式扫描）。
	if exec.Command("git", "cat-file", "-e", docCommitIncidentRev+"^{commit}").Run() == nil {
		incident, err := unreachableDocumentedCommits(docCommitIncidentRev, reachable)
		if err != nil {
			t.Fatalf("scan %s: %v", docCommitIncidentRev, err)
		}
		if !strings.Contains(strings.Join(incident[docCommitIncidentSHA], "\n"), docCommitIncidentFile+":") {
			t.Errorf("the gate does not report %s in %s at %s; the matcher is broken (found %v)", docCommitIncidentSHA, docCommitIncidentFile, docCommitIncidentRev, incident)
		}
	}

	found, err := unreachableDocumentedCommits("", reachable)
	if err != nil {
		t.Fatalf("scan working tree: %v", err)
	}
	shas := make([]string, 0, len(found))
	for sha := range found {
		shas = append(shas, sha)
	}
	sort.Strings(shas)
	for _, sha := range shas {
		if _, exempt := documentedCommitExemptions[sha]; exempt {
			continue
		}
		t.Errorf("`%s` is cited as a commit but is not in HEAD's history (a pre-rebase or branch-only commit?); cite the commit that is on main, or add a justified entry to documentedCommitExemptions:\n  %s", sha, strings.Join(found[sha], "\n  "))
	}
	for sha, why := range documentedCommitExemptions {
		if _, stillCited := found[sha]; !stillCited {
			t.Errorf("documentedCommitExemptions[%q] (%s) is no longer needed: no document cites it as an unreachable commit; delete the entry", sha, why)
		}
	}
}

// reachableCommits 返回 HEAD 能到达的全部提交（完整 SHA，已排序），供按前缀查找。
func reachableCommits() ([]string, error) {
	out, err := exec.Command("git", "rev-list", "HEAD").Output()
	if err != nil {
		return nil, fmt.Errorf("git rev-list HEAD: %w", err)
	}
	revs := strings.Fields(string(out))
	sort.Strings(revs)
	return revs, nil
}

func hasCommitWithPrefix(sorted []string, prefix string) bool {
	i := sort.SearchStrings(sorted, prefix)
	return i < len(sorted) && strings.HasPrefix(sorted[i], prefix)
}

// looksLikeCommit 排除纯数字（时间戳、端口、计数）与纯字母（英文单词）的反引号串。
func looksLikeCommit(token string) bool {
	return strings.ContainsAny(token, "0123456789") && strings.ContainsAny(token, "abcdef")
}

// unreachableDocumentedCommits 扫描 rev（空串为工作区）里跟踪的 *.md，返回不在 reachable 里的提交号及其
// 出处（path:line）。
func unreachableDocumentedCommits(rev string, reachable []string) (map[string][]string, error) {
	args := []string{"grep", "-n", "-I", "-E", documentedCommitPattern}
	if rev != "" {
		args = append(args, rev)
	}
	args = append(args, "--", "*.md", ":(exclude)artifacts")
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
			return nil, nil // git grep: no match
		}
		return nil, fmt.Errorf("git grep: %w", err)
	}
	found := make(map[string][]string)
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		// 工作区：path:line:text；按提交：rev:path:line:text。
		location := line
		if rev != "" {
			location = strings.TrimPrefix(location, rev+":")
		}
		parts := strings.SplitN(location, ":", 3)
		if len(parts) < 3 {
			continue
		}
		where := parts[0] + ":" + parts[1]
		for _, match := range commitTokenRE.FindAllStringSubmatch(parts[2], -1) {
			token := match[1]
			if !looksLikeCommit(token) || hasCommitWithPrefix(reachable, token) {
				continue
			}
			if locations := found[token]; len(locations) == 0 || locations[len(locations)-1] != where {
				found[token] = append(locations, where)
			}
		}
	}
	return found, nil
}
