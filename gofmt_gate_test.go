package roostcore_test

import (
	"bytes"
	"errors"
	"go/format"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// gofmt 门禁（2026-10-13）。
//
// 包迁移之后 95 个文件的 import 顺序没人重排（dce4be5b 一次性 gofmt），build、vet、测试都看不见：
// 格式只在有人碰到那个文件时才顺手修，修的提交里混着无关 diff。这里在普通测试里检查一遍全部 .go 文件。
//
// 范围：git 跟踪的 .go 加上未忽略的新文件（还没 git add 的也算，提交时会一起进来），与链接门禁一致；
// 被忽略的 artifacts/ 自然不在其中。判定与 gofmt 相同：go/format.Source 的结果必须与文件逐字节相等。
// 解析失败同样报出（gofmt 也拒绝它）；testdata 里不是 Go 源码的样本不应以 .go 结尾。
func TestTrackedGoFilesAreGofmtClean(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if out, err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Skip("not inside a git work tree (module zip / vendored copy)")
	}

	// 自检：判定能抓到包迁移留下的那种 import 顺序，也不误报已格式化的源码。
	if unformatted := gofmtDiffers(t, []byte("package p\n\nimport (\n\t\"b/z\"\n\t\"a/y\"\n)\n")); !unformatted {
		t.Fatal("the gate does not see unsorted imports; the check is broken")
	}
	if gofmtDiffers(t, []byte("package p\n\nimport (\n\t\"a/y\"\n\t\"b/z\"\n)\n")) {
		t.Fatal("the gate flags formatted source; the check is broken")
	}

	out, err := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var unformatted []string
	for _, file := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if file == "" {
			continue
		}
		src, err := os.ReadFile(file)
		if errors.Is(err, fs.ErrNotExist) {
			continue // 工作区里已删除、尚未提交删除的跟踪文件
		}
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		formatted, err := format.Source(src)
		if err != nil {
			unformatted = append(unformatted, file+" (does not parse: "+err.Error()+")")
			continue
		}
		if !bytes.Equal(src, formatted) {
			unformatted = append(unformatted, file)
		}
	}
	if len(unformatted) > 0 {
		t.Errorf("%d .go file(s) are not gofmt-clean; run gofmt -w on them:\n  %s", len(unformatted), strings.Join(unformatted, "\n  "))
	}
}

// gofmtDiffers 报告 src 经 gofmt 后是否会变。
func gofmtDiffers(t *testing.T, src []byte) bool {
	t.Helper()
	formatted, err := format.Source(src)
	if err != nil {
		t.Fatalf("format sample: %v", err)
	}
	return !bytes.Equal(src, formatted)
}
