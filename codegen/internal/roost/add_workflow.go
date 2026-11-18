package roost

import (
	"bytes"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/entityowner"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/marker"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/protocol"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

func lifecycleService(manifest Manifest, requested string) string {
	requested = toSnake(requested)
	if _, exists := manifest.Services[requested]; exists {
		return requested
	}
	if requested != "" {
		return requested
	}
	if access, exists := manifest.Access["player"]; requested == "" && exists {
		return access.Service
	}
	services := sortedServiceNames(manifest)
	if len(services) == 1 {
		return services[0]
	}
	return "<service>"
}

func addProtocolEndpoint(root string, manifest Manifest, options AddOptions) ([]string, error) {
	if _, enabled := manifest.Access["player"]; !enabled {
		return nil, fmt.Errorf("endpoint requires player access")
	}
	if !validName(toSnake(options.Handler)) {
		return nil, fmt.Errorf("endpoint requires --handler <protocol-controller-group>")
	}
	var changes []syncChange
	var err error
	if strings.TrimSpace(options.Lifecycle) != "" {
		if options.NestHandler != "" {
			return nil, fmt.Errorf("endpoint --lifecycle cannot use --nest-handler")
		}
		if strings.TrimSpace(options.Target) != "" {
			return nil, fmt.Errorf("endpoint --lifecycle cannot use --target: the login endpoint addresses the --lifecycle Entity")
		}
		changes, err = planLifecycleEndpoint(root, manifest, options, toSnake(options.Handler))
	} else {
		changes, err = planProtocolEndpoint(root, manifest, options, false)
	}
	if err != nil {
		return nil, err
	}
	if err := commitSyncChanges(changes); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(changes))
	for _, change := range changes {
		paths = append(paths, change.rel)
	}
	return paths, nil
}

// game/controllers 是一个包（2026-11-16 起）：endpoint 的手写入口是 Controller 上的方法，追加到
// game/controllers/<--handler 分组>.go（不存在就新建）；协议生成器先写了“未实现”占位时，就地替换那个方法。
// 自动转交的部分在 game/controllers/<协议>_endpoint_gen.go，每次生成覆盖。controller.go 归协议生成器，
// add endpoint 只在它还不存在时写同一份内容。

// controllersDir 是 game/controllers 包的目录。
func controllersDir(root string) string {
	return filepath.Join(root, "game", "controllers")
}

// endpointExistingMethod 找 handlerName 在 game/controllers 包里的现有声明：没有时返回 nil；是协议生成器留下、仍未
// 改动的“未实现”占位时返回它，由调用方在同一次提交里替换（syncChange 的 before 保证提交前没人改过这个文件）；
// 别的声明一律拒绝并写明怎么处置，绝不覆盖业务代码。
func endpointExistingMethod(root, handlerName string) (*protocol.ControllerMethod, error) {
	method, err := protocol.FindControllerMethod(controllersDir(root), handlerName)
	if err != nil || method == nil {
		return nil, err
	}
	if method.Placeholder {
		return method, nil
	}
	return nil, fmt.Errorf("%s already declares Controller.%s, and it is not the untouched placeholder roost generate writes, so add endpoint will not overwrite it; move any code you want to keep, delete the method and run this command again", relativeSlash(root, method.Path), handlerName)
}

// endpointControllerChange 在 game/controllers/controller.go 还不存在时写生成的那一份（与协议生成器相同）。
func endpointControllerChange(root string) (*syncChange, error) {
	path := filepath.Join(controllersDir(root), protocol.ControllerFileName)
	if _, err := os.Stat(path); err == nil {
		return nil, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	body, err := protocol.ControllerSource()
	if err != nil {
		return nil, err
	}
	return &syncChange{rel: relativeSlash(root, path), path: path, body: body}, nil
}

// endpointMethodChange 把手写入口 decl 写进包里：替换占位 existing，或追加到 <group>.go（不存在就新建）。
func endpointMethodChange(root, group string, existing *protocol.ControllerMethod, imports []protocol.GoImport, decl string) (syncChange, error) {
	if existing != nil {
		body, err := protocol.ReplaceControllerMethod(existing, imports, decl)
		if err != nil {
			return syncChange{}, err
		}
		return syncChange{rel: relativeSlash(root, existing.Path), path: existing.Path, body: body, before: existing.Source, existed: true}, nil
	}
	name, err := protocol.ControllerGroupFile(group)
	if err != nil {
		return syncChange{}, err
	}
	path := filepath.Join(controllersDir(root), name)
	before, err := os.ReadFile(path)
	var body []byte
	switch {
	case os.IsNotExist(err):
		before = nil
		body, err = protocol.NewControllerGroupFile(imports, decl)
	case err == nil:
		body, err = protocol.AppendGoDecls(before, imports, decl)
	}
	if err != nil {
		return syncChange{}, fmt.Errorf("%s: %w", relativeSlash(root, path), err)
	}
	return syncChange{rel: relativeSlash(root, path), path: path, body: body, before: before, existed: before != nil}, nil
}

// renderControllersFile 是一个完整的 game/controllers 源文件：header（可为空）、包子句、imports 与 decl。
func renderControllersFile(header string, imports []protocol.GoImport, decl string) ([]byte, error) {
	var b strings.Builder
	if header != "" {
		b.WriteString(header + "\n")
	}
	b.WriteString("package " + protocol.ControllersPackage + "\n\n")
	protocol.WriteGoImportBlock(&b, imports)
	b.WriteString("\n" + decl)
	formatted, err := format.Source([]byte(b.String()))
	if err != nil {
		return nil, fmt.Errorf("format endpoint scaffold: %w\n%s", err, b.String())
	}
	return formatted, nil
}

// planProtocolEndpoint 只规划文件；add 与 generate（refresh）共用模板，写入沿用已有事务提交边界。
// refresh 时只重算自动转交的 <协议>_endpoint_gen.go，不碰手写入口。
func planProtocolEndpoint(root string, manifest Manifest, options AddOptions, refresh bool) ([]syncChange, error) {
	if _, enabled := manifest.Access["player"]; !enabled {
		return nil, fmt.Errorf("endpoint requires player access; run: roost add access player --service %s", lifecycleService(manifest, options.Service))
	}
	group := toSnake(options.Handler)
	if !refresh && !validName(group) {
		return nil, fmt.Errorf("endpoint requires --handler <protocol-controller-group>")
	}
	protocolName := firstNonEmpty(options.Protocol, options.Name)
	nestHandler := firstNonEmpty(options.NestHandler, options.Name)
	protocolSnake, protocolType := toSnake(protocolName), toPascal(protocolName)
	nestSnake, nestType := toSnake(nestHandler), toPascal(nestHandler)
	if !validName(protocolSnake) {
		return nil, fmt.Errorf("invalid endpoint protocol name %q", protocolName)
	}
	if !validName(nestSnake) {
		return nil, fmt.Errorf("invalid endpoint Nest handler name %q", nestHandler)
	}
	protocolPath := filepath.Join(root, "protocol", "def", protocolSnake+".go")
	handlerPath := filepath.Join(root, "game", "handler", nestSnake+".go")
	for label, path := range map[string]string{"protocol": protocolPath, "Nest handler": handlerPath} {
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("%s %q does not exist", label, strings.TrimSuffix(filepath.Base(path), ".go"))
			}
			return nil, err
		}
	}
	// 边界把业务失败写成 (Code, Reason) 响应；响应缺这两个字段时失败无处表达，只能断连接，所以生成前拒绝。
	responseFields, err := protocolFieldNames(protocolPath, protocolType+"Response")
	if err != nil {
		return nil, err
	}
	if responseFields["code"] != "Code" || responseFields["reason"] != "Reason" {
		return nil, fmt.Errorf("protocol %sResponse needs Code int32 and Reason string fields to answer business errors (roost add protocol creates them)", protocolType)
	}
	signature, err := endpointArguments(protocolPath, protocolType, handlerPath, nestType, manifest.Project.Module+protocol.PBImportSuffix, strings.TrimSpace(options.Target))
	if err != nil {
		return nil, err
	}
	arguments, target := signature.arguments, signature.entity
	var existing *protocol.ControllerMethod
	var controllerChange *syncChange
	if !refresh {
		if existing, err = endpointExistingMethod(root, "Handle"+protocolType); err != nil {
			return nil, err
		}
		if controllerChange, err = endpointControllerChange(root); err != nil {
			return nil, err
		}
	}

	callArgs := "entityID"
	if len(arguments) > 0 {
		callArgs += ", " + strings.Join(arguments, ", ")
	}
	// Sync_ 返回 handler 的全部非 error 结果再加 err；骨架必须全部接住才能编译，结果留给业务填进响应。
	call := fmt.Sprintf("err = sender.Sync_%s(context.Context(), %s)", nestType, callArgs)
	// 业务时间（nowUnix / nowUnixMilli）不在这里填：生成的分发包装在 handler 执行时从 fctx 填这条消息冻结的业务时间，
	// Sync_ 的签名里没有它。
	useResults := ""
	if signature.results > 0 {
		results := []string{"result"}
		if signature.results > 1 {
			results = results[:0]
			for index := range signature.results {
				results = append(results, fmt.Sprintf("result%d", index))
			}
		}
		call = fmt.Sprintf("%s, err := sender.Sync_%s(context.Context(), %s)", strings.Join(results, ", "), nestType, callArgs)
		useResults = fmt.Sprintf("\t// Copy what the client needs from the handler result into the response.\n\t%s = %s\n",
			strings.TrimSuffix(strings.Repeat("_, ", len(results)), ", "), strings.Join(results, ", "))
	}
	returnResponse := fmt.Sprintf("return &pb.%sResponse{}, nil", protocolType)
	if signature.response {
		// nil,nil 不是成功响应；保留连接并返回统一内部错误，避免编码 nil 或伪造成功。
		useResults = ""
		returnResponse = fmt.Sprintf(`if result == nil {
		slog.Error("%s returned a nil response", "player_id", context.PlayerID)
		code, reason := errcode.ClientError(fmt.Errorf("nil handler response"))
		return &pb.%sResponse{Code: code, Reason: reason}, nil
	}
	return result, nil`, protocolSnake, protocolType)
	}
	imports := []protocol.GoImport{
		{Path: "fmt"},
		{Path: "log/slog"},
		{Name: target.alias, Path: target.importPath},
		{Name: "playeraccess", Path: protocol.PlayerAccessImport},
		{Name: nestSnake + "syncsender", Path: manifest.Project.Module + "/game/handler/syncsender"},
		{Path: "github.com/tjbdwanghaibo/roost-core/framework/entity"},
		{Path: "github.com/tjbdwanghaibo/roost-core/infra/base/errcode"},
		{Path: manifest.Project.Module + protocol.PBImportSuffix},
	}
	method := fmt.Sprintf(`// Handle%s is the protocol boundary. Authentication is already represented by
// context.PlayerID; all Entity access continues through the generated Sender.
//
// An error returned from here makes the access layer close the connection,
// which is right for a malformed request and wrong for a business failure. So
// a handler error becomes a response: errcode.ClientError maps a coded error
// to its code and client-facing reason, and anything without a code collapses
// to errcode.CodeInternal with a fixed reason; only that case is logged.
func (controller *Controller) Handle%s(context *playeraccess.Context, request *pb.%sRequest) (*pb.%sResponse, error) {
	if context == nil || request == nil {
		return nil, fmt.Errorf("%s endpoint: context and request are required")
	}
	// context.PlayerID is the player's unique id; Nest addresses a full entity
	// id, which carries the kind and its lock category as well.
	entityID, err := entity.BuildEntityID(context.PlayerID, %s.%s)
	if err != nil {
		return nil, fmt.Errorf("%s endpoint: %%w", err)
	}
	sender := %ssyncsender.New%sSender(controller.NestClient())
	%s
	if err != nil {
		code, reason := errcode.ClientError(err)
		if code == errcode.CodeInternal {
			slog.Error("%s failed", "player_id", context.PlayerID, "err", err)
		}
		return &pb.%sResponse{Code: code, Reason: reason}, nil
	}
%s	%s
}
`, protocolType, protocolType, protocolType, protocolType, protocolSnake,
		target.alias, target.kind, protocolSnake,
		nestSnake, nestType, call,
		protocolSnake, protocolType,
		useResults, returnResponse)
	changes := make([]syncChange, 0, 3)
	if controllerChange != nil {
		changes = append(changes, *controllerChange)
	}
	if signature.results == 0 || signature.response {
		// 可机械转交的部分持续生成；业务入口只创建一次，生成器从不改它的函数体。
		helper := strings.Replace(method, "// Handle"+protocolType, "// dispatch"+protocolType, 1)
		helper = strings.Replace(helper, "func (controller *Controller) Handle"+protocolType+"(", "func (controller *Controller) dispatch"+protocolType+"(", 1)
		body, err := renderControllersFile(endpointGeneratedHeader, imports, helper)
		if err != nil {
			return nil, err
		}
		generatedPath := filepath.Join(controllersDir(root), protocolSnake+"_endpoint_gen.go")
		before, readErr := os.ReadFile(generatedPath)
		if readErr != nil && !os.IsNotExist(readErr) {
			return nil, readErr
		}
		if readErr == nil && !bytes.HasPrefix(before, []byte(endpointGeneratedHeader+"\n")) {
			return nil, fmt.Errorf("endpoint refuses to overwrite application file %s", generatedPath)
		}
		if !bytes.Equal(before, body) {
			changes = append(changes, syncChange{rel: relativeSlash(root, generatedPath), path: generatedPath, body: body, before: before, existed: readErr == nil})
		}
		if !refresh {
			// --target 写进标记：generate 刷新转交时按同一个实体解析。
			markerOptions := "nest=" + nestType
			if target := strings.TrimSpace(options.Target); target != "" {
				markerOptions += " target=" + target
			}
			wrapper := fmt.Sprintf(`// Handle%s 是手写接入层：可在调用前做校验和授权，不要在此持有 Entity 锁。
// dispatch 已把 handler 返回的业务错误转成回包的 Code / Reason；在这里自己做的校验
// 拒绝也构造带错误码的响应返回，直接返回 error 会由接入层按分发失败处理。
//roost:endpoint %s
func (controller *Controller) Handle%s(ctx *playeraccess.Context, req *pb.%sRequest) (*pb.%sResponse, error) {
	return controller.dispatch%s(ctx, req)
}
`, protocolType, markerOptions, protocolType, protocolType, protocolType, protocolType)
			wrapperImports := []protocol.GoImport{{Name: "playeraccess", Path: protocol.PlayerAccessImport}, {Path: manifest.Project.Module + protocol.PBImportSuffix}}
			change, err := endpointMethodChange(root, group, existing, wrapperImports, wrapper)
			if err != nil {
				return nil, err
			}
			changes = append(changes, change)
		}
	} else {
		if refresh {
			return nil, fmt.Errorf("endpoint %s: handler must return no business result or *pb.%sResponse; custom result mapping belongs in an application-owned endpoint", protocolType, protocolType)
		}
		if _, err := format.Source([]byte(method)); err != nil {
			return nil, fmt.Errorf("format endpoint scaffold: %w\n%s", err, method)
		}
		change, err := endpointMethodChange(root, group, existing, imports, method)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	return changes, nil
}

// planLifecycleEndpoint 生成登录边界：endpoint 经 <Entity>FromRegistry 取得实例级 lifecycle，对
// context.PlayerID 调 GetOrCreate，保证之后的玩法请求打到的 Entity 已存在。它不走 Nest Sender：
// 首登时 Entity 还不存在，Nest 冷加载只会回 errcode.CodeEntityNotFound（DESIGN-TODO D7）。
// 业务失败与 Sender 型 endpoint 一样变成 (Code, Reason) 响应，只给 CodeInternal 记日志。
func planLifecycleEndpoint(root string, manifest Manifest, options AddOptions, group string) ([]syncChange, error) {
	protocolName := firstNonEmpty(options.Protocol, options.Name)
	protocolSnake, protocolType := toSnake(protocolName), toPascal(protocolName)
	if !validName(protocolSnake) {
		return nil, fmt.Errorf("invalid endpoint protocol name %q", protocolName)
	}
	entitySnake := toSnake(options.Lifecycle)
	if !validName(entitySnake) {
		return nil, fmt.Errorf("invalid endpoint --lifecycle Entity %q", options.Lifecycle)
	}
	protocolPath := filepath.Join(root, "protocol", "def", protocolSnake+".go")
	if _, err := os.Stat(protocolPath); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("protocol %q does not exist; run: roost add protocol %s --group game --handler %s", protocolSnake, protocolType, group)
		}
		return nil, err
	}
	// lifecycle 是实体包里生成的 lifecycle_gen.go（v1.47.0 起）：实体在就有。
	if _, err := os.Stat(filepath.Join(root, "game", "entities", entitySnake, "entity.go")); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("entity %q does not exist; run: roost add entity %s", entitySnake, toPascal(entitySnake))
		}
		return nil, err
	}
	responseFields, err := protocolFieldNames(protocolPath, protocolType+"Response")
	if err != nil {
		return nil, err
	}
	if responseFields["code"] != "Code" || responseFields["reason"] != "Reason" {
		return nil, fmt.Errorf("protocol %sResponse needs Code int32 and Reason string fields to answer business errors (roost add protocol creates them)", protocolType)
	}
	existing, err := endpointExistingMethod(root, "Handle"+protocolType)
	if err != nil {
		return nil, err
	}
	controllerChange, err := endpointControllerChange(root)
	if err != nil {
		return nil, err
	}
	entityType := toPascal(entitySnake)
	imports := []protocol.GoImport{
		{Path: "fmt"},
		{Path: "log/slog"},
		{Name: entitySnake, Path: manifest.Project.Module + "/game/entities/" + entitySnake},
		{Name: "playeraccess", Path: protocol.PlayerAccessImport},
		{Path: "github.com/tjbdwanghaibo/roost-core/infra/base/errcode"},
		{Path: manifest.Project.Module + protocol.PBImportSuffix},
	}
	method := fmt.Sprintf(`// Handle%s is the login boundary. It makes sure the authenticated player has
// a %s Entity before any gameplay request reaches Nest: a Nest handler
// addresses an Entity that exists, and a first login has none, so a request
// sent before this one answers errcode.CodeEntityNotFound. GetOrCreate only
// creates when persistence confirms the Entity is absent; a database, decode
// or timeout error is returned, never mistaken for "absent". A deleted Entity
// (only tombstones left) is not created again: its id cannot be used again,
// and the client gets errcode.CodeEntityDeleted (entity_deleted), which a
// retry or a new login will not change.
//
// GetOrCreate is safe to race and to retry: two logins of the same new player
// both end holding the one Entity. A failure becomes a response; only
// errcode.CodeInternal (no business code) is logged.
func (controller *Controller) Handle%s(context *playeraccess.Context, request *pb.%sRequest) (*pb.%sResponse, error) {
	if context == nil || request == nil {
		return nil, fmt.Errorf("%s endpoint: context and request are required")
	}
	entities, err := %s.LifecycleFromRegistry(controller.Registry())
	if err == nil {
		_, _, err = entities.GetOrCreate(context.Context(), context.PlayerID)
	}
	if err != nil {
		code, reason := errcode.ClientError(err)
		if code == errcode.CodeInternal {
			slog.Error("%s failed", "player_id", context.PlayerID, "err", err)
		}
		return &pb.%sResponse{Code: code, Reason: reason}, nil
	}
	return &pb.%sResponse{}, nil
}
`, protocolType, entityType,
		protocolType, protocolType, protocolType, protocolSnake,
		entitySnake, protocolSnake, protocolType, protocolType)
	if _, err := format.Source([]byte(method)); err != nil {
		return nil, fmt.Errorf("format login endpoint scaffold: %w\n%s", err, method)
	}
	change, err := endpointMethodChange(root, group, existing, imports, method)
	if err != nil {
		return nil, err
	}
	changes := []syncChange{change}
	if controllerChange != nil {
		changes = append(changes, *controllerChange)
	}
	return changes, nil
}

// endpointEntity is the Entity a Nest handler's first parameter names: the
// package it lives in and the kind constant that package declares. The
// endpoint needs it because Nest addresses a full entity id — unique id plus
// kind plus lock category — and context.PlayerID is only the unique id. An
// endpoint that passed the bare id reached Nest as "kind N category is not
// registered" on the first real request; nothing before a live run could see
// it, because the scaffold compiled.
type endpointEntity struct {
	alias      string
	importPath string
	kind       string
}

// endpointSignature 是骨架调用 Sync_ 需要的 handler 签名：按请求字段映射的实参、非 error 返回值个数与目标 Entity。
type endpointSignature struct {
	arguments []string
	results   int
	response  bool // 唯一业务结果是当前协议的 PB 响应指针，直接返回。
	entity    endpointEntity
}

// serverBusinessTimeArguments 是不从请求取的 handler 参数：业务时间由服务器决定，放进请求就等于让客户端决定（可以
// 延长限时效果、提前领冷却奖励）。按约定名识别，类型必须是 int64；它们也不由 endpoint 填——Nest 生成器的分发包装在
// handler 执行时从 fctx 填这条消息冻结的业务时间，Sync_ 的签名里没有它们，骨架跳过。
var serverBusinessTimeArguments = map[string]bool{"nowUnix": true, "nowUnixMilli": true}

// endpointArguments 读 Nest handler 的签名。target 是 endpoint 上写的 target=<实体>（--target），可以为空，
// 见 endpointTargetDir。
func endpointArguments(protocolPath, protocolType, handlerPath, handlerType, pbImport, target string) (endpointSignature, error) {
	var signature endpointSignature
	requestFields, err := protocolFieldNames(protocolPath, protocolType+"Request")
	if err != nil {
		return signature, err
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, handlerPath, nil, parser.ParseComments)
	if err != nil {
		return signature, err
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "handler"+handlerType || function.Type.Params == nil {
			continue
		}
		if len(function.Type.Params.List) == 0 {
			return signature, fmt.Errorf("Nest handler %s has no Entity target", handlerType)
		}
		if len(function.Type.Params.List[0].Names) != 1 {
			return signature, fmt.Errorf("Nest handler %s endpoint requires exactly one named Player Entity target", handlerType)
		}
		// handler 在 <root>/game/handler/<名>.go。
		root := filepath.Dir(filepath.Dir(filepath.Dir(handlerPath)))
		targetDir, err := endpointTargetDir(root, handlerType, target, function.Doc)
		if err != nil {
			return signature, err
		}
		signature.entity, err = endpointEntityOf(file, function.Type.Params.List[0].Type, handlerType, root, targetDir)
		if err != nil {
			return signature, err
		}
		for index, field := range function.Type.Params.List {
			if index == 0 {
				continue
			}
			if len(field.Names) == 0 {
				return signature, fmt.Errorf("Nest handler %s parameter %d must be named", handlerType, index+1)
			}
			for _, name := range field.Names {
				parameter := name.Name
				if serverBusinessTimeArguments[parameter] {
					if ident, ok := field.Type.(*ast.Ident); !ok || ident.Name != "int64" {
						return signature, fmt.Errorf("Nest handler %s parameter %q is the business time the generated Nest dispatch fills when the handler runs; declare it int64", handlerType, parameter)
					}
					continue
				}
				// 只接受本工程当前协议的 PB 指针；按实际 import 路径匹配，不能仅凭包别名猜测。
				if endpointPBPointer(file, field.Type, pbImport, protocolType+"Request") {
					signature.arguments = append(signature.arguments, "request")
					continue
				}
				requestField, exists := requestFields[toSnake(parameter)]
				if !exists {
					return signature, fmt.Errorf("protocol %sRequest has no field matching Nest parameter %q. Add the field to the request only if the client decides this value. "+
						"A value the server decides must not come from the request: for the business time name the parameter nowUnix or nowUnixMilli (int64) "+
						"and the generated Nest dispatch fills it when the handler runs (the business time its message froze, fctx.Now); anything else the server decides, compute inside the handler, not from the request",
						protocolType, parameter)
				}
				signature.arguments = append(signature.arguments, "request."+requestField)
			}
		}
		// 末尾的 error 不算结果（nest 生成器拒绝非末尾的 error），其余每个返回值都是 Sync_ 的一个结果。
		if results := function.Type.Results; results != nil {
			for _, field := range results.List {
				signature.results += max(1, len(field.Names))
			}
			if last, ok := results.List[len(results.List)-1].Type.(*ast.Ident); ok && last.Name == "error" {
				signature.results--
			}
		}
		if signature.results == 1 && function.Type.Results != nil {
			signature.response = endpointPBPointer(file, function.Type.Results.List[0].Type, pbImport, protocolType+"Response")
		}
		return signature, nil
	}
	return signature, fmt.Errorf("Nest handler function handler%s not found in %s", handlerType, handlerPath)
}

// endpointPBPointer 将协议归属绑定到 import 路径，而不是 pb 别名。PB 含内部状态，不能按值复制。
func endpointPBPointer(file *ast.File, expression ast.Expr, importPath, typeName string) bool {
	pointer, ok := expression.(*ast.StarExpr)
	if !ok {
		return false
	}
	selector, ok := pointer.X.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != typeName {
		return false
	}
	alias, ok := selector.X.(*ast.Ident)
	if !ok {
		return false
	}
	for _, spec := range file.Imports {
		imported, err := strconv.Unquote(spec.Path.Value)
		if err != nil || imported != importPath {
			continue
		}
		name := path.Base(imported)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		return name == alias.Name
	}
	return false
}

// endpointTargetDir 决定 endpoint 寻址哪个实体包（game/entities/<目录>），返回空表示没有指定、按参数类型解析。
// 来源有两处，认实体的规则与 Nest 生成器的 target= 相同（结构体名或实体包名，大小写不敏感，包名可带下划线）：
//   - endpoint 上的 target=<实体>（//roost:endpoint ... target=，roost add endpoint --target）：必须对上一个实体；
//   - Nest handler 的 //roost:nest target=<实体>（targets= 取第一个，对应第一个锁目标）：对上实体时采用，对不上
//     （语义名，例如 target=RoomKind）不影响，与 Nest 生成器 paramOwner 的退回规则一致。
//
// 两处都写了且是不同实体时报错：endpoint 拼的实体 ID 必须就是 handler 锁住、Nest 按归属注册的那个实体。
func endpointTargetDir(root, handlerType, target string, doc *ast.CommentGroup) (string, error) {
	nestTarget := ""
	if doc != nil {
		for _, comment := range doc.List {
			rest, ok := marker.Cut(comment.Text, "nest")
			if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
				continue
			}
			options, err := marker.Nest.Parse(rest)
			if err != nil {
				return "", fmt.Errorf("Nest handler %s: //roost:nest %w", handlerType, err)
			}
			raw := firstNonEmpty(options["target"], options["targets"])
			nestTarget, _, _ = strings.Cut(raw, ",")
			nestTarget = strings.TrimSpace(nestTarget)
		}
	}
	nestDir := ""
	if nestTarget != "" {
		dir, err := entityDirNamed(root, nestTarget)
		if err != nil {
			return "", err
		}
		nestDir = dir
	}
	if target == "" {
		return nestDir, nil
	}
	dir, err := entityDirNamed(root, target)
	if err != nil {
		return "", err
	}
	if dir == "" {
		return "", fmt.Errorf("Nest handler %s: endpoint target=%s names no entity under game/entities (write the entity struct name or its package name)", handlerType, target)
	}
	if nestDir != "" && nestDir != dir {
		return "", fmt.Errorf("Nest handler %s: endpoint target=%s is entity package %s, but the handler's //roost:nest target=%s is %s; the endpoint must address the entity the handler locks",
			handlerType, target, dir, nestTarget, nestDir)
	}
	return dir, nil
}

// entityDirNamed 按 target= 的写法找 game/entities 下的实体包目录，规则与 nest 生成器的 entityNamed 相同：
// 实体结构体名（大小写不敏感），或实体包名（可带下划线）。实体包没有 //roost:entity 标记时只按包名认。对不上返回 ""。
func entityDirNamed(root, name string) (string, error) {
	plain := func(value string) string { return strings.ReplaceAll(value, "_", "") }
	entries, err := os.ReadDir(filepath.Join(root, "game", "entities"))
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() && (strings.EqualFold(entry.Name(), name) || strings.EqualFold(plain(entry.Name()), plain(name))) {
			return entry.Name(), nil
		}
	}
	entities, err := entityowner.Entities(filepath.Join(root, "game"))
	if err != nil {
		return "", err
	}
	for _, ent := range entities {
		if strings.EqualFold(ent.Entity, name) {
			return filepath.Base(ent.Dir), nil
		}
	}
	return "", nil
}

// endpointEntityOf resolves the handler's first parameter type, written as
// <alias>.I<Component>Entity, to the Entity package it imports. The kind
// constant follows the entity scaffold's naming: EntityKind<Pascal(package)>.
// A game/view type (view.I<Component>Entity, view.<Entity>Entity) is resolved
// to the entity that mounts the component, see viewEndpointEntity. targetDir
// (see endpointTargetDir) picks the entity when several mount the component;
// for any other type it must agree with the type.
func endpointEntityOf(file *ast.File, expression ast.Expr, handlerType, root, targetDir string) (endpointEntity, error) {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok {
		return endpointEntity{}, fmt.Errorf("Nest handler %s: the Entity target must be a <package>.I<Component>Entity interface", handlerType)
	}
	alias, ok := selector.X.(*ast.Ident)
	if !ok {
		return endpointEntity{}, fmt.Errorf("Nest handler %s: the Entity target must be a <package>.I<Component>Entity interface", handlerType)
	}
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := path.Base(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name != alias.Name {
			continue
		}
		if module, isView := strings.CutSuffix(importPath, "/game/view"); isView {
			return viewEndpointEntity(root, module, selector.Sel.Name, handlerType, targetDir)
		}
		if targetDir != "" && path.Base(importPath) != targetDir {
			return endpointEntity{}, fmt.Errorf("Nest handler %s: target names entity package %s, but the Entity target %s.%s is in %s", handlerType, targetDir, alias.Name, selector.Sel.Name, importPath)
		}
		return endpointEntity{alias: alias.Name, importPath: importPath, kind: "EntityKind" + toPascal(path.Base(importPath))}, nil
	}
	return endpointEntity{}, fmt.Errorf("Nest handler %s: no import provides package %q for the Entity target", handlerType, alias.Name)
}

// viewEndpointEntity 把 game/view 里的参数类型解析成实体包：view.<E>Entity 就是实体 E；组件窄接口 view.I<X>Entity 找
// game/entities/*/ 里挂了组件 X 的实体（comp:"CompType<X>" 或 comp:"<包>.CompType<X>"，不依赖已经生成的 view 文件，
// add endpoint 常在 roost generate 之前跑）。挂了它的实体不止一个时要用 target=<实体> 指定（targetDir，见
// endpointTargetDir），没指定就报错：endpoint 按 PlayerID 拼实体 ID，要知道是哪一种。指定的实体必须挂了这个组件。
func viewEndpointEntity(root, module, typeName, handlerType, targetDir string) (endpointEntity, error) {
	entityAt := func(dir string) endpointEntity {
		return endpointEntity{alias: dir, importPath: module + "/game/entities/" + dir, kind: "EntityKind" + toPascal(dir)}
	}
	name, ok := strings.CutSuffix(typeName, "Entity")
	if !ok || name == "" {
		return endpointEntity{}, fmt.Errorf("Nest handler %s: view.%s is not an entity interface (view.<Entity>Entity or view.I<Component>Entity)", handlerType, typeName)
	}
	// I 后面紧跟大写字母是组件窄接口（IProfileEntity）；ItemEntity 是实体 Item 的总接口。
	isComponent := len(name) > 1 && name[0] == 'I' && name[1] >= 'A' && name[1] <= 'Z'
	component := strings.TrimPrefix(name, "I")
	if !isComponent {
		dir := toSnake(name)
		if _, err := os.Stat(filepath.Join(root, "game", "entities", dir)); err != nil {
			return endpointEntity{}, fmt.Errorf("Nest handler %s: view.%s names no entity package game/entities/%s", handlerType, typeName, dir)
		}
		if targetDir != "" && targetDir != dir {
			return endpointEntity{}, fmt.Errorf("Nest handler %s: target names entity package %s, but view.%s is entity %s", handlerType, targetDir, typeName, dir)
		}
		return entityAt(dir), nil
	}
	dirs, err := entitiesMountingComponent(root, component)
	if err != nil {
		return endpointEntity{}, err
	}
	if targetDir != "" {
		if !slices.Contains(dirs, targetDir) {
			mounted := "no entity mounts it"
			if len(dirs) > 0 {
				mounted = "it is mounted by " + strings.Join(dirs, ", ")
			}
			return endpointEntity{}, fmt.Errorf("Nest handler %s: target entity %s does not mount component %s (view.%s); %s", handlerType, targetDir, component, typeName, mounted)
		}
		return entityAt(targetDir), nil
	}
	switch len(dirs) {
	case 1:
		return entityAt(dirs[0]), nil
	case 0:
		return endpointEntity{}, fmt.Errorf("Nest handler %s: no entity under game/entities mounts component %s (view.%s)", handlerType, component, typeName)
	default:
		return endpointEntity{}, fmt.Errorf("Nest handler %s: entities %s all mount component %s; an endpoint addresses one kind: write target=<entity> on its //roost:endpoint marker "+
			"(roost add endpoint --target <entity>), or take view.<Entity>Entity of the player's entity", handlerType, strings.Join(dirs, ", "), component)
	}
}

// entitiesMountingComponent 返回 game/entities/ 下挂了组件 component（PascalCase）的实体包目录（排序）：实体结构体上有
// comp:"CompType<component>" 或 comp:"<包>.CompType<component>"。只读业务文件（生成的 _gen 文件不挂组件字段），不依赖
// 已经生成的 view 文件：add handler / add endpoint 常在 roost generate 之前跑。
func entitiesMountingComponent(root, component string) ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(root, "game", "entities", "*", "*.go"))
	if err != nil {
		return nil, err
	}
	tag := regexp.MustCompile("\\bcomp:\"(?:[A-Za-z_][A-Za-z0-9_]*\\.)?CompType" + regexp.QuoteMeta(component) + "(?:,[^\"]*)?\"")
	var dirs []string
	for _, file := range paths {
		if strings.HasSuffix(file, "_test.go") || strings.HasSuffix(file, "_gen.go") || strings.HasSuffix(file, "_gen_wire.go") {
			continue
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		if dir := filepath.Base(filepath.Dir(file)); tag.Match(raw) && !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

func protocolFieldNames(path, typeName string) (map[string]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	for _, declaration := range file.Decls {
		generic, ok := declaration.(*ast.GenDecl)
		if !ok || generic.Tok != token.TYPE {
			continue
		}
		for _, item := range generic.Specs {
			spec, ok := item.(*ast.TypeSpec)
			if !ok || spec.Name.Name != typeName {
				continue
			}
			structure, ok := spec.Type.(*ast.StructType)
			if !ok {
				return nil, fmt.Errorf("protocol message %s is not a struct", typeName)
			}
			fields := make(map[string]string)
			for _, field := range structure.Fields.List {
				for _, name := range field.Names {
					fields[toSnake(name.Name)] = name.Name
				}
			}
			return fields, nil
		}
	}
	return nil, fmt.Errorf("protocol message %s not found in %s", typeName, path)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
