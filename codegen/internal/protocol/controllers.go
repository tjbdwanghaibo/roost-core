package protocol

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// game/controllers 是一个包（维护者 2026-11-16）：生成的 controller.go 放 Controller、FromRegistry 与公共辅助，
// 业务（模块、协议存根、endpoint）都是 Controller 上的方法，按 <分组>.go / <模块>.go 分文件，不再按分组建子目录。
const (
	// ControllersPackage 是 game/controllers 的包名。
	ControllersPackage = "controllers"
	// ControllerFileName 是生成器所有的公共文件，每次生成整份覆盖。
	ControllerFileName = "controller.go"
)

// ControllerScaffoldMarker 是业务骨架的第一行：带它的新文件由生成器创建一次，之后归业务所有、生成器不覆盖。
const ControllerScaffoldMarker = "// Roost controller scaffold: application-owned; never overwritten."

// IsControllerScaffold 仅识别业务骨架，不赋予生成器覆盖或删除已有文件的权限。
func IsControllerScaffold(path string, body []byte) bool {
	return strings.HasPrefix(filepath.ToSlash(path), "game/controllers/") &&
		strings.HasSuffix(path, ".go") && strings.HasPrefix(string(body), ControllerScaffoldMarker+"\n")
}

// IsUnimplementedControllerMethod 认出协议生成器为 handlerName 写的、仍未改动的“未实现”占位方法：Controller 上的
// handlerName 方法，函数体只有一条 return，最后一个返回值是 fmt.Errorf("<handlerName>: not implemented")、其余都是
// nil。roost add endpoint 用它决定能否接管这个方法：业务改过方法体（多了语句、换了返回值）就不是占位，不能覆盖。
// 只看行为、不看注释与格式。
func IsUnimplementedControllerMethod(method *ast.FuncDecl, handlerName string) bool {
	if method == nil || method.Name.Name != handlerName || !isControllerMethod(method) {
		return false
	}
	if method.Body == nil || len(method.Body.List) != 1 {
		return false
	}
	ret, ok := method.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) == 0 {
		return false
	}
	for _, result := range ret.Results[:len(ret.Results)-1] {
		if ident, ok := result.(*ast.Ident); !ok || ident.Name != "nil" {
			return false
		}
	}
	call, ok := ret.Results[len(ret.Results)-1].(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	fun, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || fun.Sel.Name != "Errorf" {
		return false
	}
	if pkg, ok := fun.X.(*ast.Ident); !ok || pkg.Name != "fmt" {
		return false
	}
	message, ok := call.Args[0].(*ast.BasicLit)
	return ok && message.Kind == token.STRING && message.Value == strconv.Quote(handlerName+": not implemented")
}

// isControllerMethod 报告 decl 是不是 Controller（值或指针接收者）上的方法。
func isControllerMethod(decl *ast.FuncDecl) bool {
	if decl.Recv == nil || len(decl.Recv.List) != 1 {
		return false
	}
	receiver := decl.Recv.List[0].Type
	if pointer, ok := receiver.(*ast.StarExpr); ok {
		receiver = pointer.X
	}
	name, ok := receiver.(*ast.Ident)
	return ok && name.Name == "Controller"
}

// ControllerMethod 是 game/controllers 包里一个 Controller 方法所在的文件与字节范围（含文档注释）。
type ControllerMethod struct {
	Path        string
	Source      []byte
	Start, End  int
	Placeholder bool // 是协议生成器写的、仍未改动的“未实现”占位
}

// FindControllerMethod 在 game/controllers 包（dir）的非测试文件里找 Controller 的 name 方法；没有时返回 nil。
// 生成文件（controller.go、*_endpoint_gen.go）也算在内：它们里的方法同样占着这个名字。
func FindControllerMethod(dir, name string) (*ControllerMethod, error) {
	pkg, err := scanControllersPackage(dir)
	if err != nil {
		return nil, err
	}
	for _, file := range pkg.files {
		for _, decl := range file.ast.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !isControllerMethod(fn) || fn.Name.Name != name {
				continue
			}
			start := fn.Pos()
			if fn.Doc != nil {
				start = fn.Doc.Pos()
			}
			return &ControllerMethod{
				Path: file.path, Source: file.raw,
				Start: file.fset.Position(start).Offset, End: file.fset.Position(fn.End()).Offset,
				Placeholder: IsUnimplementedControllerMethod(fn, name),
			}, nil
		}
	}
	return nil, nil
}

// ReplaceControllerMethod 用 decl（方法源码，会先 gofmt）替换 method 所在的那段源码，补上 decl 要的 import，
// 并去掉占位独用、替换后不再用到的 fmt。文件其余字节不变。
func ReplaceControllerMethod(method *ControllerMethod, imports []GoImport, decl string) ([]byte, error) {
	formatted, err := formatGo([]byte(strings.TrimSpace(decl) + "\n"))
	if err != nil {
		return nil, fmt.Errorf("format replacement method: %w", err)
	}
	src := append(append(append([]byte(nil), method.Source[:method.Start]...), bytes.TrimRight(formatted, "\n")...), method.Source[method.End:]...)
	src, err = addGoImports(src, imports)
	if err != nil {
		return nil, err
	}
	return dropUnusedGoImport(src, "fmt")
}

// ControllerSource 是生成的 game/controllers/controller.go。roost add endpoint 在它还不存在时写同一份内容，
// 之后由协议生成器每次覆盖。
func ControllerSource() ([]byte, error) {
	return formatGo([]byte(protocolGeneratedHeader + "\n\n" + controllerSource))
}

// controllerSource 是公共 controller.go 的正文（不含生成头）。改动这里就是改所有生成工程的 Controller：
// 模块模板依赖这里的名字（Controller 的字段、FromRegistry、NestClient、Registry、推送辅助、moduleState）。
const controllerSource = `// Package controllers 是玩家协议的业务入口。生成的协议登记（internal/access/player/protocols_gen.go）在每个
// 进程里调用一次 FromRegistry，把同一个 *Controller 交给全部分组；每个分组的 Handle* 方法都挂在它上面。
//
// 本文件归生成器所有，每次 roost generate 整份覆盖，不要手改。业务写在同一个包的 <模块>.go（模块带来）或
// <分组>.go（协议生成器在这里追加缺少的 Handle* 存根）里。
package controllers

import (
	"context"
	"fmt"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// Controller 持有协议入口共用的依赖。协议分发不会自动取得 Entity 的执行权：要改 Entity，经 Nest handler
// （NestClient 上生成的 Sender）执行。
//
// 模块自己的依赖不加字段：无状态的（服务客户端等）由模块文件里的私有方法每次从 Registry() 取；有状态的
// （在线名单、缓存的推送面）用 moduleState 懒建，在一个 Controller 上只建一次。
type Controller struct {
	registry   *app.Registry
	nestClient corenest.Client
	// states 是 moduleState 的存放处：模块名 → *moduleStateSlot。零值可用，测试直接写
	// &Controller{registry: registry} 也能用懒建状态。Controller 含锁，只按指针传递。
	states sync.Map
}

// FromRegistry 由生成的协议登记在每个进程里调用一次；Nest 客户端此时必须已经发布。
// 别的依赖（玩家传输、服务客户端）可能发布得更晚，用到时再从 Registry() 取，不在这里取。
func FromRegistry(registry *app.Registry) (*Controller, error) {
	if registry == nil {
		return nil, fmt.Errorf("controller: app registry is required")
	}
	client, ok := app.Lookup[corenest.Client](registry, app.ModName("nest"))
	if !ok || client == nil {
		return nil, fmt.Errorf("controller: nest client is unavailable")
	}
	return &Controller{registry: registry, nestClient: client}, nil
}

// NestClient 是改 Entity 的唯一入口：生成的 Sender 都从它构造。
func (controller *Controller) NestClient() corenest.Client {
	if controller == nil {
		return nil
	}
	return controller.nestClient
}

// Registry 供不走 Nest 的边界使用：登录 endpoint 从这里取 Entity lifecycle（roost add endpoint --lifecycle），
// 模块从这里取服务客户端与玩家传输。
func (controller *Controller) Registry() *app.Registry {
	if controller == nil {
		return nil
	}
	return controller.registry
}

// PlayerTCPCapability 是玩家传输发布 Runtime 用的能力名（internal/access/player/tcp 的 Name）。嵌入 TCP 与
// 独立 Gate 的 Game Ingress 用同一个名字发布，业务不区分。
const PlayerTCPCapability app.ModName = "access.player.tcp"

// PlayerPusher 是推送最常用的那个方法：按玩家推给他在本进程的全部连接。玩家传输的 Runtime 还有更多方法
// （ActiveSessions、PushSession、CloseSessions、OnSessionClosed，见 core 的 gateway.PlayerRuntime）；
// 要用时声明自己的窄接口，经 LookupPlayerTransport 取。
//
// 这里写成接口、能力按名字取，是因为传输包 import 构造 Controller 的协议登记——反过来 import 它就成环了。
type PlayerPusher interface {
	PushPlayer(ctx context.Context, playerID int64, messageID uint32, value any) error
}

// LookupPlayerTransport 按能力名取玩家传输，断言成调用方要的接口 T（PlayerPusher，或模块自己的窄接口）。
// registry 为 nil、传输没装配、或 Runtime 不实现 T 时返回错误，错误里写明能力名；调用方决定记日志还是返回。
// 传输 Mod 可能晚于 FromRegistry 发布，所以在用到时取。
func LookupPlayerTransport[T any](registry *app.Registry) (T, error) {
	var zero T
	if registry == nil {
		return zero, fmt.Errorf("controller: app registry is required to reach capability %q", PlayerTCPCapability)
	}
	transport, ok := app.Lookup[T](registry, PlayerTCPCapability)
	if !ok {
		return zero, fmt.Errorf("controller: capability %q not found or lacks the methods %T needs; the player tcp transport is not assembled", PlayerTCPCapability, (*T)(nil))
	}
	return transport, nil
}

// PushToPlayer 把 value 以 messageID 推给 playerID 在本进程的全部连接。推送只是捷径、不是记录：失败（传输没装配、
// 玩家不连在本进程、发送队列满）只报给调用方，客户端要能经请求补取同样的数据。不要在持有 Entity 锁的
// Nest handler 里调用；在 Sender 返回之后、锁外推送。
func PushToPlayer(ctx context.Context, registry *app.Registry, playerID int64, messageID uint32, value any) error {
	transport, err := LookupPlayerTransport[PlayerPusher](registry)
	if err != nil {
		return err
	}
	return transport.PushPlayer(ctx, playerID, messageID, value)
}

// PushToPlayer 同包级的 PushToPlayer，用这个 Controller 的 Registry。
func (controller *Controller) PushToPlayer(ctx context.Context, playerID int64, messageID uint32, value any) error {
	return PushToPlayer(ctx, controller.Registry(), playerID, messageID, value)
}

// moduleStateSlot 是一个模块的懒建状态：once 保证 build 只跑一次。
type moduleStateSlot struct {
	once  sync.Once
	value any
}

// moduleState 返回 module 在这个 Controller 上的状态，第一次调用时用 build 建，之后返回同一个值；并发调用安全，
// build 只跑一次（并发的调用方等它建完）。module 用模块名，一个模块一个名字；同一个名字换了类型 T 是编程错误，
// 直接 panic。build 不能返回 nil，也不要在里面再调 moduleState 取同一个模块（会死锁）；会失败的初始化
// （取能力等）放进状态自己的方法里，用到时再做。
func moduleState[T any](controller *Controller, module string, build func() *T) *T {
	if controller == nil {
		panic(fmt.Sprintf("controller: module %q state on a nil Controller", module))
	}
	raw, _ := controller.states.LoadOrStore(module, &moduleStateSlot{})
	slot := raw.(*moduleStateSlot)
	slot.once.Do(func() { slot.value = build() })
	value, ok := slot.value.(*T)
	if !ok {
		panic(fmt.Sprintf("controller: module %q state is %T, not %T", module, slot.value, (*T)(nil)))
	}
	return value
}
`

// controllersPackageFile 是 game/controllers 包里一个已解析的非测试 Go 文件。
type controllersPackageFile struct {
	path string
	raw  []byte
	fset *token.FileSet
	ast  *ast.File
}

type controllersPackageScan struct {
	files []controllersPackageFile
	// controllerRaw 是现有 controller.go 的内容（nil 表示不存在）。
	controllerRaw []byte
	// hasOtherFiles 表示除 controller.go 之外还有 Go 文件（模块文件、存根、endpoint、测试）。
	hasOtherFiles bool
}

// scanControllersPackage 读 game/controllers（dir）这一个包，并做全包一次的检查：
//   - 子目录里不能有 Go 文件（旧的按分组分包布局，2026-11-16 起不再支持，旧工程按 CHANGELOG 手动迁移）；
//   - 每个文件的包名都是 controllers；
//   - Controller、FromRegistry 只由生成的 controller.go 声明，业务文件里再声明就报错；
//   - 同一个 Controller 方法不能声明两次。
//
// 目录不存在时返回空结果。
func scanControllersPackage(dir string) (controllersPackageScan, error) {
	var scan controllersPackageScan
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return scan, nil
	}
	if err != nil {
		return scan, err
	}
	methods := map[string]string{}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			if hasGo, err := dirHasGoFiles(path); err != nil {
				return scan, err
			} else if hasGo {
				return scan, fmt.Errorf("%s: game/controllers is one package (package controllers) without subdirectories; move the files into game/controllers/ as <module>.go / <group>.go methods on the generated Controller", path)
			}
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		if entry.Name() != ControllerFileName {
			scan.hasOtherFiles = true
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return scan, err
		}
		if entry.Name() == ControllerFileName {
			scan.controllerRaw = raw
		}
		if strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, raw, parser.ParseComments)
		if err != nil {
			return scan, err
		}
		if file.Name.Name != ControllersPackage {
			return scan, fmt.Errorf("%s is package %s; every file in game/controllers is package %s", path, file.Name.Name, ControllersPackage)
		}
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					if typ, ok := spec.(*ast.TypeSpec); ok && typ.Name.Name == "Controller" && entry.Name() != ControllerFileName {
						return scan, fmt.Errorf("%s declares Controller; it is generated in game/controllers/%s, put the module's state behind moduleState instead", path, ControllerFileName)
					}
				}
			case *ast.FuncDecl:
				if decl.Recv == nil {
					if decl.Name.Name == "FromRegistry" && entry.Name() != ControllerFileName {
						return scan, fmt.Errorf("%s declares FromRegistry; it is generated in game/controllers/%s", path, ControllerFileName)
					}
					continue
				}
				if !isControllerMethod(decl) {
					continue
				}
				if previous, ok := methods[decl.Name.Name]; ok {
					return scan, fmt.Errorf("Controller.%s is declared in both %s and %s", decl.Name.Name, previous, path)
				}
				methods[decl.Name.Name] = path
			}
		}
		scan.files = append(scan.files, controllersPackageFile{path: path, raw: raw, fset: fset, ast: file})
	}
	return scan, nil
}

func dirHasGoFiles(dir string) (bool, error) {
	found := false
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".go") {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found, err
}

func (scan controllersPackageScan) methods() map[string]bool {
	out := map[string]bool{}
	for _, file := range scan.files {
		for _, decl := range file.ast.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && isControllerMethod(fn) {
				out[fn.Name.Name] = true
			}
		}
	}
	return out
}

// ControllerGroupFile 是 handler=<group> 的协议存根与 endpoint 落在 game/controllers 里的文件名。
func ControllerGroupFile(group string) (string, error) {
	switch {
	case group+".go" == ControllerFileName:
		return "", fmt.Errorf("handler=%s is reserved: game/controllers/%s is generated", group, ControllerFileName)
	case strings.HasSuffix(group, "_test"):
		return "", fmt.Errorf("handler=%s would make game/controllers/%s.go a test file; pick another group name", group, group)
	}
	return group + ".go", nil
}

// controllerStubImports 是“未实现”存根用到的 import。
func controllerStubImports(modulePath string) []GoImport {
	return []GoImport{{Path: "fmt"}, {Name: "playeraccess", Path: PlayerAccessImport}, {Name: "pb", Path: modulePath + PBImportSuffix}}
}

// renderControllerStub 是一个协议的“未实现”占位方法；roost add endpoint 认得它（IsUnimplementedControllerMethod）。
func renderControllerStub(msg MsgDef) string {
	name := "Handle" + msgName(msg)
	result, ret := "error", ""
	if msg.Resp != "" {
		result, ret = "(*pb."+msg.Resp+", error)", "nil, "
	}
	return fmt.Sprintf(`// %s 由业务实现；需要修改 Entity 时请通过 Nest handler 执行。
// 返回响应后由接入层编码回包；异步或手动回包时不要再返回同一响应。
func (controller *Controller) %s(ctx *playeraccess.Context, req *pb.%s) %s {
	return %sfmt.Errorf(%q)
}
`, name, name, msg.Req, result, ret, name+": not implemented")
}

// NewControllerGroupFile 是新建的 game/controllers/<分组>.go：带业务骨架标记（归业务所有、生成器不覆盖），
// imports 与 decls 之后由业务继续写。
func NewControllerGroupFile(imports []GoImport, decls string) ([]byte, error) {
	var b strings.Builder
	b.WriteString(ControllerScaffoldMarker + "\n")
	b.WriteString("package " + ControllersPackage + "\n\n")
	WriteGoImportBlock(&b, imports)
	b.WriteString("\n" + decls)
	return formatGo([]byte(b.String()))
}

// generateControllers 维护 game/controllers 这一个包（root 是这个目录）：
//   - controller.go 归生成器，每次整份覆盖；现有文件不是生成的（没有生成头）就拒绝，不覆盖业务代码；
//   - 每个 handler=<分组> 协议缺少的 Handle* 存根追加到 <分组>.go 末尾（文件不存在就新建，带骨架标记），
//     多个协议的存根合在同一个文件；只追加，不改已有内容。方法在包里任何文件都算已有，业务可以移动或合并方法。
//
// 全部内容先算好再写，任何一处要报错时什么都不写。
func generateControllers(root string, defs *Definitions, stdout io.Writer) error {
	groups := make(map[string][]MsgDef)
	for _, msg := range defs.Messages {
		if msg.Handler != "" {
			groups[msg.Handler] = append(groups[msg.Handler], msg)
		}
	}
	domains := make([]string, 0, len(groups))
	for domain := range groups {
		domains = append(domains, domain)
	}
	sort.Strings(domains)
	scan, err := scanControllersPackage(root)
	if err != nil {
		return err
	}
	if len(domains) == 0 && !scan.hasOtherFiles {
		return nil
	}
	if scan.controllerRaw != nil && !bytes.HasPrefix(scan.controllerRaw, []byte(protocolGeneratedHeader+"\n")) {
		return fmt.Errorf("%s is not the generated controller (no %q header); move its code into another file of the package and delete it, roost generate writes it", filepath.Join(root, ControllerFileName), protocolGeneratedHeader)
	}
	controller, err := ControllerSource()
	if err != nil {
		return err
	}
	type write struct {
		path    string
		content []byte
	}
	writes := []write{{filepath.Join(root, ControllerFileName), controller}}
	methods := scan.methods()
	for _, domain := range domains {
		name, err := ControllerGroupFile(domain)
		if err != nil {
			return err
		}
		messages := append([]MsgDef(nil), groups[domain]...)
		sort.Slice(messages, func(i, j int) bool { return messages[i].ReqID < messages[j].ReqID })
		var stubs strings.Builder
		for _, msg := range messages {
			if methods["Handle"+msgName(msg)] {
				continue
			}
			if stubs.Len() > 0 {
				stubs.WriteString("\n")
			}
			stubs.WriteString(renderControllerStub(msg))
		}
		if stubs.Len() == 0 {
			continue
		}
		path := filepath.Join(root, name)
		imports := controllerStubImports(defs.ModulePath)
		raw, err := os.ReadFile(path)
		var content []byte
		switch {
		case os.IsNotExist(err):
			content, err = NewControllerGroupFile(imports, stubs.String())
		case err != nil:
		default:
			content, err = AppendGoDecls(raw, imports, stubs.String())
		}
		if err != nil {
			return fmt.Errorf("controller stubs %s: %w", path, err)
		}
		writes = append(writes, write{path, content})
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	for _, item := range writes {
		changed, err := writeIfChanged(item.path, item.content, false)
		if err != nil {
			return err
		}
		printChange(stdout, item.path, changed)
	}
	return nil
}

// IsControllerStubAppend 报告 after 是否恰好是在 before 上做了协议生成器的追加（AppendGoDecls）：before 的字节原样
// 保留，最多在最后一个 import 声明（或 package 子句）之后插入一个 import 块，末尾追加的只有“未实现”占位方法。
// roost 的暂存提交（project sync）靠它把追加了存根的业务文件带回真实工程——别的改动一律不认，已有文件仍归业务。
func IsControllerStubAppend(before, after []byte) bool {
	if bytes.Equal(before, after) {
		return false
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", before, parser.ImportsOnly)
	if err != nil {
		return false
	}
	insertAt := fset.Position(file.Name.End()).Offset
	for _, decl := range file.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.IMPORT {
			insertAt = fset.Position(gen.End()).Offset
		}
	}
	if len(after) < insertAt || !bytes.Equal(after[:insertAt], before[:insertAt]) {
		return false
	}
	rest, tail := after[insertAt:], before[insertAt:]
	split := func(inserted int) (block, appended []byte, ok bool) {
		if !bytes.HasPrefix(rest[inserted:], tail) {
			return nil, nil, false
		}
		return rest[:inserted], rest[inserted+len(tail):], true
	}
	candidates := []int{0}
	if bytes.HasPrefix(rest, []byte(goImportBlockPrefix)) {
		if end := bytes.Index(rest[len(goImportBlockPrefix):], []byte("\n)")); end >= 0 {
			candidates = append(candidates, len(goImportBlockPrefix)+end+2)
		}
	}
	for _, inserted := range candidates {
		block, appended, ok := split(inserted)
		if ok && validImportBlock(block) && onlyControllerPlaceholders(appended) {
			return true
		}
	}
	return false
}

func validImportBlock(block []byte) bool {
	if len(block) == 0 {
		return true
	}
	file, err := parser.ParseFile(token.NewFileSet(), "", append([]byte("package p"), block...), 0)
	if err != nil {
		return false
	}
	for _, decl := range file.Decls {
		if gen, ok := decl.(*ast.GenDecl); !ok || gen.Tok != token.IMPORT {
			return false
		}
	}
	return len(file.Decls) == 1
}

func onlyControllerPlaceholders(appended []byte) bool {
	if len(bytes.TrimSpace(appended)) == 0 {
		return false
	}
	file, err := parser.ParseFile(token.NewFileSet(), "", append([]byte("package p\n"), appended...), 0)
	if err != nil || len(file.Decls) == 0 {
		return false
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !strings.HasPrefix(fn.Name.Name, "Handle") || !IsUnimplementedControllerMethod(fn, fn.Name.Name) {
			return false
		}
	}
	return true
}
