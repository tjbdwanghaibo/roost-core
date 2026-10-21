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
// 读机器时区（time.Local、time.LoadLocation）同样提示：业务日按部署配置的 time.business_timezone 算（D18）。
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

// machineZoneReads 是会被提示的机器时区读取（DESIGN-TODO D18）：time.Local 取决于 TZ 与 /etc/localtime，
// 开发机是本地时区、容器是 UTC；业务包自己 LoadLocation 会绕开部署配置的业务时区。业务日与每日重置按
// time.business_timezone 算（app.BusinessCalendar(registry)、clock.BusinessDay）。同一个豁免指令。
var machineZoneReads = map[string]bool{"Local": true, "LoadLocation": true}

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
			if !ok || !systemClockReads[selector.Sel.Name] && !machineZoneReads[selector.Sel.Name] {
				return true
			}
			if ident, ok := selector.X.(*ast.Ident); !ok || ident.Name != alias || ident.Obj != nil {
				return true // 不是 time 包（ident.Obj 非 nil 是同名局部变量）
			}
			position := fileSet.Position(selector.Pos())
			if exempt[position.Line] {
				return true
			}
			if machineZoneReads[selector.Sel.Name] {
				fmt.Printf("%s: hint: business package reads a machine time zone (time.%s); business days and daily resets follow the deployment's time.business_timezone — use app.BusinessCalendar(registry) or clock.BusinessDay / NextDayStart; a log or tool output in local time is not business time: mark it //%s <reason> (D18)\n",
					position, selector.Sel.Name, systemClockDirective)
				hints++
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

// Nest handler 里的业务时钟读取（DESIGN-TODO D22-1，维护者 2026-10-21 决定 1A）：业务时间唯一入口是 fctx.Now()，
// 每条消息开始时冻结一次，同一条消息里读到同一个值；handler 参数 nowUnix / nowUnixMilli 由生成的分发包装从它填。
// handler 里再读 app.BusinessClock(r).Now() 或进程业务时钟 clock.Now() / clock.UnixMilli() 是第二次读取，与心跳的
// Beat.Now、entitytimer 的 Fire.Now、同一事务里别处读的时间不是同一时刻。这里只看 Nest handler 本身（函数文档有
// //roost:nest，或业务包里以 handler 开头的函数）的函数体，不跟进它调用的 helper；服务 goroutine、admin 命令等不在
// Nest 消息里的代码照旧读 app.BusinessClock。确实要现读的（例如量一段耗时），写 //glsvet:live-business-clock <理由>，
// 规则与 system-clock 相同（同一行、上一行或函数文档）。只提示，不计入违例。

// liveBusinessClockDirective 豁免 Nest handler 里一处现读业务时钟。
const liveBusinessClockDirective = "glsvet:live-business-clock"

const (
	appImportPath   = "github.com/tjbdwanghaibo/roost-core/framework/app"
	clockImportPath = "github.com/tjbdwanghaibo/roost-core/infra/base/clock"
)

// processClockReads 是 infra/base/clock 里读进程业务时钟当前时刻的函数。
var processClockReads = map[string]bool{"Now": true, "UnixMilli": true}

// reportHandlerClockHints 打印 file 里 Nest handler 对业务时钟的现读，返回提示条数。
func reportHandlerClockHints(fileSet *token.FileSet, file *ast.File) int {
	appAlias, clockAlias := importName(file, appImportPath, "app"), importName(file, clockImportPath, "clock")
	if appAlias == "" && clockAlias == "" {
		return 0
	}
	exempt := directiveLines(fileSet, file, liveBusinessClockDirective)
	hints := 0
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil || !isNestHandler(function) || docHasText(function.Doc, liveBusinessClockDirective) {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := selector.X.(*ast.Ident)
			if !ok || ident.Obj != nil {
				return true // 不是包名（ident.Obj 非 nil 是同名局部变量）
			}
			var read string
			switch {
			case appAlias != "" && ident.Name == appAlias && selector.Sel.Name == "BusinessClock":
				read = appAlias + ".BusinessClock"
			case clockAlias != "" && ident.Name == clockAlias && processClockReads[selector.Sel.Name]:
				read = clockAlias + "." + selector.Sel.Name
			default:
				return true
			}
			position := fileSet.Position(selector.Pos())
			if exempt[position.Line] {
				return true
			}
			fmt.Printf("%s: hint: Nest handler %s reads the business clock (%s); business time in a Nest message is fctx.Now() — frozen once per message, the same instant as Beat.Now, Fire.Now and the nowUnix / nowUnixMilli parameters the generated dispatch fills; a deliberate live read: mark it //%s <reason> (D22-1)\n",
				position, function.Name.Name, read, liveBusinessClockDirective)
			hints++
			return true
		})
	}
	return hints
}

// importName 返回 file 里导入 path 的引用名（未改名时为 fallback）；没有导入或以 _ / . 导入时返回空。
func importName(file *ast.File, path, fallback string) string {
	for _, spec := range file.Imports {
		if imported, err := strconv.Unquote(spec.Path.Value); err != nil || imported != path {
			continue
		}
		if spec.Name == nil {
			return fallback
		}
		if spec.Name.Name == "_" || spec.Name.Name == "." {
			return ""
		}
		return spec.Name.Name
	}
	return ""
}

// directiveLines 返回带 directive 的注释所在行与下一行（行尾注释与上一行注释两种写法）。
func directiveLines(fileSet *token.FileSet, file *ast.File, directive string) map[int]bool {
	lines := make(map[int]bool)
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if strings.Contains(comment.Text, directive) {
				line := fileSet.Position(comment.Slash).Line
				lines[line] = true
				lines[line+1] = true
			}
		}
	}
	return lines
}

// docHasText 逐条看文档注释里是否有 text（CommentGroup.Text() 会丢掉 //glsvet:… 这类指令行）。
func docHasText(doc *ast.CommentGroup, text string) bool {
	if doc == nil {
		return false
	}
	for _, comment := range doc.List {
		if strings.Contains(comment.Text, text) {
			return true
		}
	}
	return false
}
