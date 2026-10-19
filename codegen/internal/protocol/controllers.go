package protocol

import (
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

const controllerScaffoldMarker = "// Roost controller scaffold: application-owned; never overwritten."

// IsControllerScaffold 仅识别业务骨架，不赋予生成器覆盖或删除已有文件的权限。
func IsControllerScaffold(path string, body []byte) bool {
	return strings.HasPrefix(filepath.ToSlash(path), "game/controllers/") &&
		strings.HasSuffix(path, ".go") && strings.HasPrefix(string(body), controllerScaffoldMarker+"\n")
}

// IsUnimplementedControllerScaffold 认出协议生成器为 handlerName 写的、仍未改动的“未实现”占位：带骨架标记，
// 唯一的函数是 Controller 上的 handlerName 方法，函数体只有一条 return，最后一个返回值是
// fmt.Errorf("<handlerName>: not implemented")、其余都是 nil。roost add endpoint 用它决定能否接管这个文件：
// 业务改过方法体（多了语句、换了返回值、加了别的函数）就不是占位，不能覆盖。只看行为、不看注释与格式。
func IsUnimplementedControllerScaffold(body []byte, handlerName string) bool {
	if !strings.HasPrefix(string(body), controllerScaffoldMarker+"\n") {
		return false
	}
	file, err := parser.ParseFile(token.NewFileSet(), "", body, 0)
	if err != nil {
		return false
	}
	var method *ast.FuncDecl
	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.GenDecl:
			if decl.Tok != token.IMPORT {
				return false
			}
		case *ast.FuncDecl:
			if method != nil {
				return false
			}
			method = decl
		}
	}
	if method == nil || method.Name.Name != handlerName || method.Recv == nil || len(method.Recv.List) != 1 {
		return false
	}
	receiver := method.Recv.List[0].Type
	if pointer, ok := receiver.(*ast.StarExpr); ok {
		receiver = pointer.X
	}
	if name, ok := receiver.(*ast.Ident); !ok || name.Name != "Controller" {
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

// generateControllers 只创建缺失文件；扫描整个业务包，允许开发者移动或合并方法。
func generateControllers(root string, defs *Definitions, stdout io.Writer) error {
	groups := make(map[string][]MsgDef)
	for _, msg := range defs.Messages {
		if msg.Handler != "" {
			groups[msg.Handler] = append(groups[msg.Handler], msg)
		}
	}
	var domains []string
	for domain := range groups {
		domains = append(domains, domain)
	}
	sort.Strings(domains)
	for _, domain := range domains {
		dir := filepath.Join(root, domain)
		entries, err := os.ReadDir(dir)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		methods := map[string]bool{}
		hasController, hasFactory := false, false
		packageName := domain
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, entry.Name()), nil, 0)
			if err != nil {
				return err
			}
			packageName = file.Name.Name
			for _, decl := range file.Decls {
				switch decl := decl.(type) {
				case *ast.GenDecl:
					for _, spec := range decl.Specs {
						if typ, ok := spec.(*ast.TypeSpec); ok && typ.Name.Name == "Controller" {
							hasController = true
						}
					}
				case *ast.FuncDecl:
					if decl.Recv == nil {
						if decl.Name.Name == "FromRegistry" {
							hasFactory = true
						}
						continue
					}
					receiver := decl.Recv.List[0].Type
					if pointer, ok := receiver.(*ast.StarExpr); ok {
						receiver = pointer.X
					}
					if name, ok := receiver.(*ast.Ident); ok && name.Name == "Controller" {
						methods[decl.Name.Name] = true
					}
				}
			}
		}
		if hasController != hasFactory {
			return fmt.Errorf("controller %s: Controller and FromRegistry must both exist", domain)
		}
		files := map[string]string{}
		if !hasController {
			files["controller.go"] = fmt.Sprintf(`package %s

import (
 "fmt"
 "github.com/tjbdwanghaibo/roost-core/framework/app"
 corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// Controller 持有业务依赖；协议分发不会自动取得 Entity 的执行权。
type Controller struct {
 registry *app.Registry
 nestClient corenest.Client
}
func FromRegistry(registry *app.Registry) (*Controller, error) {
 if registry == nil { return nil, fmt.Errorf("controller: app registry is required") }
 client, ok := app.Lookup[corenest.Client](registry, app.ModName("nest"))
 if !ok || client == nil { return nil, fmt.Errorf("controller: nest client is unavailable") }
 return &Controller{registry: registry, nestClient: client}, nil
}
func (controller *Controller) NestClient() corenest.Client {
 if controller == nil { return nil }; return controller.nestClient
}
// Registry 供不走 Nest 的边界使用：登录 endpoint 从这里取 Entity lifecycle（roost add endpoint --lifecycle）。
func (controller *Controller) Registry() *app.Registry {
 if controller == nil { return nil }; return controller.registry
}
`, packageName)
		}
		for _, msg := range groups[domain] {
			name := "Handle" + msgName(msg)
			if methods[name] {
				continue
			}
			result, ret := "error", ""
			if msg.Resp != "" {
				result, ret = "(*pb."+msg.Resp+", error)", "nil, "
			}
			filename := toSnake(msgName(msg)) + ".go"
			if filename == "controller.go" {
				filename = "handle_controller.go"
			}
			if _, exists := files[filename]; exists {
				return fmt.Errorf("controller %s: duplicate scaffold filename %s", domain, filename)
			}
			files[filename] = fmt.Sprintf(`package %s
import (
 "fmt"
 %q
 %q
)
// %s 由业务实现；需要修改 Entity 时请通过 Nest handler 执行。
// 返回响应后由接入层编码回包；异步或手动回包时不要再返回同一响应。
func (controller *Controller) %s(ctx *playeraccess.Context, req *pb.%s) %s {
 return %sfmt.Errorf(%q)
}
`, packageName, defs.ModulePath+PlayerAccessImportSuffix, defs.ModulePath+PBImportSuffix, name, name, msg.Req, result, ret, name+": not implemented")
		}
		var names []string
		for name := range files {
			names = append(names, name)
		}
		sort.Strings(names)
		// 先检查全部目标，已有同名文件但没有目标声明时明确报错，绝不覆盖。
		for _, name := range names {
			if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
				return fmt.Errorf("controller scaffold conflicts with existing file: %s", filepath.Join(dir, name))
			} else if !os.IsNotExist(err) {
				return err
			}
		}
		for _, name := range names {
			body, err := formatGo([]byte(controllerScaffoldMarker + "\n" + files[name]))
			if err != nil {
				return err
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			path := filepath.Join(dir, name)
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				return err
			}
			_, writeErr := file.Write(body)
			closeErr := file.Close()
			if writeErr != nil {
				return writeErr
			}
			if closeErr != nil {
				return closeErr
			}
			printChange(stdout, path, true)
		}
	}
	return nil
}
