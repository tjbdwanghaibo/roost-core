package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// 业务时钟提示（维护者决定 D-L3，docs/feature/D-L3-BUSINESS-SYSTEM-CLOCK-2026-10-06.md）。
//
// 时间分两个钟：业务时钟 = 真实时间 + time.logic_offset（活动窗口、World 定时器、日 / 周重置、冷却、
// 业务过期、赛季、排行周期、游戏时间），从 app.BusinessClock(registry) 拿；系统时钟 = 真实时间
// （帧率、租约与锁、超时、重试、存储 TTL、Ack、日志与 WAL 时间戳），直接用 time 包。业务包里直接读
// time.Now / time.Since / time.Until 多半是该用业务时钟的地方，这里打印 hint:；确实是系统时间的，
// 在同一行或上一行写 `//glsvet:system-clock <理由>`，或写在函数的文档注释里豁免整个函数。
// 提示只打印、不计入违例、不改退出码；-clockhints=false 关闭。
//
// 哪些包算业务包：相对模块根（向上找 go.mod）的路径里有一段目录名在 -businessdirs 里（缺省 game，
// 生成工程的 game/... 与 internal/service/game）。框架自己的核心包没有这样的目录，不受影响。

var clockHints = flag.Bool("clockhints", true, "print a hint for time.Now / time.Since / time.Until in business packages (D-L3; hints never fail the run)")

var businessDirs = flag.String("businessdirs", "game", "comma-separated directory names that mark business packages for -clockhints")

// systemClockDirective 豁免一处系统时间的使用。
const systemClockDirective = "glsvet:system-clock"

// systemClockReads 是会被提示的 time 包函数：读当前时刻，或拿当前时刻做差。
var systemClockReads = map[string]bool{"Now": true, "Since": true, "Until": true}

// isBusinessDirectory 报告 directory 是否属于业务包：模块根之下的路径里有一段在 names 里。
func isBusinessDirectory(directory string, names []string) bool {
	relative := moduleRelative(directory)
	for _, segment := range strings.Split(filepath.ToSlash(relative), "/") {
		for _, name := range names {
			if name != "" && segment == name {
				return true
			}
		}
	}
	return false
}

// moduleRelative 返回 directory 相对最近的 go.mod 所在目录的路径；找不到 go.mod 时原样返回，
// 这样仓库放在名叫 game 的目录下也不会让每个包都成为业务包。
func moduleRelative(directory string) string {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return directory
	}
	for root := absolute; ; {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			relative, err := filepath.Rel(root, absolute)
			if err != nil {
				return directory
			}
			return relative
		}
		parent := filepath.Dir(root)
		if parent == root {
			return directory
		}
		root = parent
	}
}

func businessDirNames() []string {
	var names []string
	for _, name := range strings.Split(*businessDirs, ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// reportClockHints 打印 file 里对系统时钟的直接读取，返回提示条数。
func reportClockHints(fileSet *token.FileSet, file *ast.File) int {
	alias := timeImportName(file)
	if alias == "" {
		return 0
	}
	exempt := make(map[int]bool)
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if strings.Contains(comment.Text, systemClockDirective) {
				line := fileSet.Position(comment.Slash).Line
				exempt[line] = true   // 行尾注释
				exempt[line+1] = true // 上一行的注释
			}
		}
	}
	hints := 0
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && docHasDirective(function.Doc) {
			continue // 整个函数声明为系统时间
		}
		ast.Inspect(declaration, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok || !systemClockReads[selector.Sel.Name] {
				return true
			}
			if ident, ok := selector.X.(*ast.Ident); !ok || ident.Name != alias || ident.Obj != nil {
				return true // 不是 time 包（ident.Obj 非 nil 是同名局部变量）
			}
			position := fileSet.Position(selector.Pos())
			if exempt[position.Line] {
				return true
			}
			fmt.Printf("%s: hint: business package reads the system clock (time.%s); business time — windows, timers, cooldowns, expiry, seasons — comes from the business clock (app.BusinessClock(registry) or an injected clock.Business); a lease, timeout, retry or storage TTL is system time: mark it //%s <reason> (D-L3)\n",
				position, selector.Sel.Name, systemClockDirective)
			hints++
			return true
		})
	}
	return hints
}

// docHasDirective 逐条看文档注释：CommentGroup.Text() 会丢掉 //glsvet:… 这类指令行。
func docHasDirective(doc *ast.CommentGroup) bool {
	if doc == nil {
		return false
	}
	for _, comment := range doc.List {
		if strings.Contains(comment.Text, systemClockDirective) {
			return true
		}
	}
	return false
}

// timeImportName 返回 file 里 "time" 包的引用名；没有导入或以 _ / . 导入时返回空。
func timeImportName(file *ast.File) string {
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != "time" {
			continue
		}
		if spec.Name == nil {
			return "time"
		}
		if spec.Name.Name == "_" || spec.Name.Name == "." {
			return ""
		}
		return spec.Name.Name
	}
	return ""
}
