package roostcore_test

import (
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// 文档相对链接门禁（2026-10-06，RR-20261006-10 同批）。
//
// 合仓之后 skill/README.md 里 52 个相对链接仍按旧 roost-skill 仓的布局写（`docs/skill.md`、
// `skill/parse.go`……），从 skill/ 目录解析全部落空；build、vet、测试全绿，因为仓里没有任何检查读
// Markdown 链接（conflict_marker_gate 只找冲突标记）。这里在普通测试里扫全部跟踪的 *.md：每个
// 相对链接（去掉 #锚点 与 ?查询）必须指向一个跟踪的（或未被忽略、即将提交的）文件或目录。
//
// 只认这两类：链接到被忽略的本机文件，换一台机器就是死链。不检查锚点（标题 slug 规则随渲染器
// 而变）、外部 URL、代码块与行内代码里的方括号。
//
// 豁免（显式列出，新增须写理由）：
//   - 目标在 artifacts/ 下：被忽略的本机证据目录（压测结果、pprof），文档按约定链接它，仓里没有；
//   - docLinkExemptSources 列出的源文件前缀：按原貌保留的历史文档。
var docLinkExemptSources = map[string]string{
	"docs/history/": "09-22 以前的历史账本与归档，按原貌保留，不随目录搬迁改写",
}

// docLinkPattern 匹配行内链接与图片 `[text](target "title")` 的 target；target 可用 <> 包住。
var docLinkPattern = regexp.MustCompile(`\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)`)

var docInlineCode = regexp.MustCompile("`[^`]*`")

func TestTrackedMarkdownRelativeLinksResolve(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if out, err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Skip("not inside a git work tree (module zip / vendored copy)")
	}
	// 跟踪文件加上未忽略的新文件（还没 git add 的新文档也算，提交时会一起进来）。
	out, err := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	tracked := map[string]bool{".": true}
	var markdown []string
	for _, file := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if file == "" {
			continue
		}
		tracked[file] = true
		for dir := path.Dir(file); dir != "." && !tracked[dir]; dir = path.Dir(dir) {
			tracked[dir] = true
		}
		if strings.HasSuffix(file, ".md") {
			markdown = append(markdown, file)
		}
	}
	exists := func(target string) bool { return tracked[target] }

	// 自检：匹配器能抓到合仓遗留的那种链接，且不误报代码块、行内代码、锚点与外部链接。
	sample := strings.Join([]string{
		"[api](docs/skill.md) [here](parse.go) [dir](.) [up](../LICENSE#license)",
		"`[code](missing.md)` [web](https://example.com/x.md) [anchor](#top) ![img](<missing.png>)",
		"```",
		"[fenced](missing.md)",
		"```",
		"[evidence](../artifacts/perf/x/client.json)",
	}, "\n")
	sampleExists := func(target string) bool {
		return target == "skill/parse.go" || target == "skill" || target == "LICENSE"
	}
	got := brokenDocLinks("skill/README.md", sample, sampleExists)
	want := []string{"skill/README.md:1: docs/skill.md", "skill/README.md:2: missing.png"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("gate self-check: got %q, want %q; the matcher is broken", got, want)
	}

	var broken []string
	for _, file := range markdown {
		if exemptDocLinkSource(file) {
			continue
		}
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		broken = append(broken, brokenDocLinks(file, string(content), exists)...)
	}
	sort.Strings(broken)
	if len(broken) > 0 {
		t.Errorf("%d relative links in tracked Markdown point at nothing tracked (fix the path, or add a reasoned exemption to docLinkExemptSources):\n  %s",
			len(broken), strings.Join(broken, "\n  "))
	}
}

func exemptDocLinkSource(file string) bool {
	for prefix := range docLinkExemptSources {
		if strings.HasPrefix(file, prefix) {
			return true
		}
	}
	return false
}

// brokenDocLinks 返回 file（仓库相对路径）里解析不到的相对链接，格式 "file:行号: 原链接"。
func brokenDocLinks(file, content string, exists func(string) bool) []string {
	var broken []string
	fenced := false
	for i, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		line = docInlineCode.ReplaceAllString(line, "")
		for _, match := range docLinkPattern.FindAllStringSubmatch(line, -1) {
			raw := match[1]
			target := raw
			if cut := strings.IndexAny(target, "#?"); cut >= 0 {
				target = target[:cut]
			}
			if target == "" || strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			if unescaped, err := url.PathUnescape(target); err == nil {
				target = unescaped
			}
			resolved := path.Clean(path.Join(path.Dir(file), target))
			if strings.HasPrefix(target, "/") {
				resolved = path.Clean(strings.TrimPrefix(target, "/"))
			}
			if resolved == "artifacts" || strings.HasPrefix(resolved, "artifacts/") {
				continue
			}
			if !exists(resolved) {
				broken = append(broken, file+":"+strconv.Itoa(i+1)+": "+raw)
			}
		}
	}
	return broken
}
