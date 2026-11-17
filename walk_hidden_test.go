package roostcore_test

import (
	"io/fs"
	"strings"
)

// hiddenDir 报告遍历到的是不是以 "." 开头的子目录（.git、.claude、.idea 等）。根包的门禁按 Go 工具链的惯例跳过它们：
// .claude/worktrees 里是 agent 的整仓 worktree 副本，扫进去会把副本里的用例、包算成本仓的（2026-11-17）。
func hiddenDir(path string, entry fs.DirEntry) bool {
	return entry != nil && entry.IsDir() && path != "." && strings.HasPrefix(entry.Name(), ".")
}
