package roost

import (
	"bytes"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/protocol"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

func addEntityLifecycle(root string, manifest Manifest, options AddOptions) ([]string, error) {
	entityName, _, err := resolveEntitySource(root, firstNonEmpty(options.Entity, options.Name))
	if err != nil {
		return nil, err
	}
	service := lifecycleService(manifest, options.Service)
	serviceSpec, serviceExists := manifest.Services[service]
	if !serviceExists {
		if strings.TrimSpace(options.Service) != "" {
			return nil, fmt.Errorf("unknown service %q", toSnake(options.Service))
		}
		return nil, fmt.Errorf("Entity lifecycle requires --service when no owning Service can be inferred")
	}
	resolvedMods, resolveErr := resolveMods(serviceSpec.Mods)
	if resolveErr != nil {
		return nil, resolveErr
	}
	if !contains(resolvedMods, "nest") {
		return nil, fmt.Errorf("Entity lifecycle requires the instance Entity runtime published by Nest; run: roost add mod nest -service %s", service)
	}
	persistenceAlias := "engine"
	persistenceImport := "github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	path := filepath.Join(root, "game", "lifecycle", entityName+".go")
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("%s already exists", relativeSlash(root, path))
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	body := fmt.Sprintf(`package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	{{PERSISTENCE_IMPORT}}
	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	%s %q
)

var Err%sNotFound = errors.New("%s lifecycle: Entity not found")

// %s owns the explicit create/load/destroy boundary for %s.
// Request handlers should use generated Nest senders for ordinary reads and
// writes; this type is for authentication/role creation and administration.
type %s struct {
	access *entity.ManagerAccess
}

func New%s(access *entity.ManagerAccess) (*%s, error) {
	if access == nil || access.Manager() == nil {
		return nil, fmt.Errorf("%s lifecycle: Entity access is required")
	}
	return &%s{access: access}, nil
}

// FromRegistry is the normal application entry point. Nest publishes the
// instance-scoped ManagerAccess as entity.runtime, so lifecycle code never
// depends on package globals or reaches into a Service implementation.
func FromRegistry(registry *app.Registry) (*%s, error) {
	if registry == nil {
		return nil, fmt.Errorf("%s lifecycle: app registry is required")
	}
	access, ok := app.Lookup[*entity.ManagerAccess](registry, mods.ModEntityRuntime)
	if !ok || access == nil {
		return nil, fmt.Errorf("%s lifecycle: Entity runtime is unavailable")
	}
	return New%s(access)
}

func (lifecycle *%s) Get(ctx context.Context, uniqueID int64) (*%s.%s, error) {
	fullID, err := entity.BuildEntityID(uniqueID, %s.EntityKind%s)
	if err != nil {
		return nil, err
	}
	value, err := lifecycle.access.Get(ctx, fullID, entity.MustEntityCategoryOfKind(%s.EntityKind%s))
	if err != nil {
		return nil, deletedError(err)
	}
	if value == nil {
		return nil, Err%sNotFound
	}
	typed, ok := value.(*%s.%s)
	if !ok {
		return nil, fmt.Errorf("%s lifecycle: Entity %%d has type %%T", fullID, value)
	}
	return typed, nil
}

func (lifecycle *%s) Create(ctx context.Context, uniqueID int64) (*%s.%s, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Create does not read the store: uniqueID must be a fresh id from the id
	// generator, never the id of a deleted Entity (its tombstone makes the
	// first write a projection conflict that fences the process). Use
	// GetOrCreate when the id may already have a record.
	value, err := lifecycle.access.Create(&entity.EntityCreateParam{
		IsCreate: true,
		UniqueID: uniqueID,
		Kind:     %s.EntityKind%s,
		Category: entity.MustEntityCategoryOfKind(%s.EntityKind%s),
	})
	if err != nil {
		return nil, err
	}
	typed, ok := value.(*%s.%s)
	if !ok {
		return nil, fmt.Errorf("%s lifecycle: created Entity has type %%T", value)
	}
	return typed, nil
}

// GetOrCreate returns the existing Entity or creates it when persistence has
// no record. An Entity that was deleted (only tombstones left) is NOT
// created again: its id cannot be used again, and GetOrCreate returns an
// error carrying errcode.ErrEntityDeleted (client code entity_deleted).
// entity.ErrEntityRemoved means the previous instance with this ID
// is still finishing its removal (a revoked in-transaction create or a
// Destroy); it is transient, so the lookup is retried a bounded number of
// times before the error is returned. Inside a Nest handler the framework
// reports that window as nest.ErrCreatedEntityLockConflict instead and
// decides itself whether the whole message is rolled back and re-admitted.
func (lifecycle *%s) GetOrCreate(ctx context.Context, uniqueID int64) (*%s.%s, bool, error) {
	for attempt := 1; ; attempt++ {
		value, err := lifecycle.Get(ctx, uniqueID)
		if err == nil {
			return value, false, nil
		}
		if !errors.Is(err, {{PERSISTENCE_NOT_FOUND}}) && !errors.Is(err, Err%sNotFound) {
			return nil, false, err
		}
		value, err = lifecycle.Create(ctx, uniqueID)
		if errors.Is(err, entity.ErrEntityExists) {
			value, err = lifecycle.Get(ctx, uniqueID)
			return value, false, err
		}
		if errors.Is(err, entity.ErrEntityRemoved) && attempt < 3 {
			continue
		}
		return value, err == nil, err
	}
}

func (lifecycle *%s) Destroy(ctx context.Context, value *%s.%s, reason entity.EntityDestroyReason, deletePersisted bool) error {
	if value == nil {
		return Err%sNotFound
	}
	return lifecycle.access.Destroy(ctx, value, reason, deletePersisted)
}

// deletedError gives a deleted Entity its stable client code
// (errcode.ErrEntityDeleted); the cause stays on the chain for errors.Is.
func deletedError(err error) error {
	if errors.Is(err, entity.ErrAuthorityEntityDeleted) {
		var coded *errcode.IntError
		if !errors.As(err, &coded) {
			return errcode.Wrap(errcode.ErrEntityDeleted, err)
		}
	}
	return err
}
`, entityName, manifest.Project.Module+"/game/entities/"+entityName,
		toPascal(entityName), entityName, toPascal(entityName)+"Lifecycle", toPascal(entityName), toPascal(entityName)+"Lifecycle",
		toPascal(entityName)+"Lifecycle", toPascal(entityName)+"Lifecycle", entityName, toPascal(entityName)+"Lifecycle",
		toPascal(entityName)+"Lifecycle", entityName, entityName, toPascal(entityName)+"Lifecycle",
		toPascal(entityName)+"Lifecycle", entityName, toPascal(entityName), entityName, toPascal(entityName), entityName, toPascal(entityName),
		toPascal(entityName), entityName, toPascal(entityName), entityName,
		toPascal(entityName)+"Lifecycle", entityName, toPascal(entityName), entityName, toPascal(entityName), entityName, toPascal(entityName), entityName, toPascal(entityName), entityName,
		toPascal(entityName)+"Lifecycle", entityName, toPascal(entityName), toPascal(entityName),
		toPascal(entityName)+"Lifecycle", entityName, toPascal(entityName), toPascal(entityName))
	body = strings.ReplaceAll(body, "{{PERSISTENCE_IMPORT}}", persistenceAlias+" \""+persistenceImport+"\"")
	// One lifecycle file per Entity, all in package lifecycle: the registry
	// entry point carries the Entity's name, or the second Entity's file
	// redeclares FromRegistry and the package stops compiling (found when the
	// game template added World next to Player).
	body = strings.Replace(body, "// FromRegistry is the normal application entry point.", "// "+toPascal(entityName)+"FromRegistry is the normal application entry point.", 1)
	body = strings.Replace(body, "func FromRegistry(registry *app.Registry)", "func "+toPascal(entityName)+"FromRegistry(registry *app.Registry)", 1)
	// 同理，删档错误的辅助函数也带 Entity 名（deletedPlayerError），同包多个 lifecycle 文件不重名。
	body = strings.ReplaceAll(body, "deletedError", "deleted"+toPascal(entityName)+"Error")
	body = strings.ReplaceAll(body, "{{PERSISTENCE_NOT_FOUND}}", persistenceAlias+".ErrEntityAggregateNotFound")
	formatted, err := format.Source([]byte(body))
	if err != nil {
		return nil, fmt.Errorf("format lifecycle scaffold: %w\n%s", err, body)
	}
	if err := commitSyncChanges([]syncChange{{rel: relativeSlash(root, path), path: path, body: formatted}}); err != nil {
		return nil, err
	}
	return []string{relativeSlash(root, path)}, nil
}

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
		return nil, fmt.Errorf("endpoint requires player access; run: roost add access player --service %s", lifecycleService(manifest, options.Service))
	}
	domain := toSnake(options.Handler)
	if !validName(domain) {
		return nil, fmt.Errorf("endpoint requires --handler <protocol-controller-domain>")
	}
	if strings.TrimSpace(options.Lifecycle) != "" {
		if strings.TrimSpace(options.NestHandler) != "" {
			return nil, fmt.Errorf("endpoint --lifecycle calls the Entity lifecycle, not a Nest handler; drop --nest-handler")
		}
		return addLifecycleEndpoint(root, manifest, options, domain)
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
	signature, err := endpointArguments(protocolPath, protocolType, handlerPath, nestType)
	if err != nil {
		return nil, err
	}
	arguments, target := signature.arguments, signature.entity
	controllerDir := filepath.Join(root, "game", "controllers", domain)
	endpointPath := filepath.Join(controllerDir, protocolSnake+".go")
	if _, err := os.Stat(endpointPath); err == nil {
		return nil, fmt.Errorf("%s already exists", relativeSlash(root, endpointPath))
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	controllerPath := filepath.Join(controllerDir, "controller.go")
	var controllerChange *syncChange
	if raw, err := os.ReadFile(controllerPath); os.IsNotExist(err) {
		body, formatErr := format.Source([]byte(renderNestController(domain)))
		if formatErr != nil {
			return nil, formatErr
		}
		change := syncChange{rel: relativeSlash(root, controllerPath), path: controllerPath, body: body}
		controllerChange = &change
	} else if err != nil {
		return nil, err
	} else if !bytes.Contains(raw, []byte("NestClient()")) {
		return nil, fmt.Errorf("%s is not Nest-injected; create a new controller domain or add NestClient() before generating the endpoint", relativeSlash(root, controllerPath))
	}

	callArgs := "entityID"
	if len(arguments) > 0 {
		callArgs += ", " + strings.Join(arguments, ", ")
	}
	// Sync_ 返回 handler 的全部非 error 结果再加 err；骨架必须全部接住才能编译，结果留给业务填进响应。
	call := fmt.Sprintf("err = sender.Sync_%s(context.Context(), %s)", nestType, callArgs)
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
	body := fmt.Sprintf(`package %s

import (
	"fmt"
	"log/slog"

	%s %q
	playeraccess %q
	%ssyncsender %q
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	%q
)

// Handle%s is the protocol boundary. Authentication is already represented by
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
%s	return &pb.%sResponse{}, nil
}
`, domain,
		target.alias, target.importPath,
		manifest.Project.Module+protocol.PlayerAccessImportSuffix,
		nestSnake, manifest.Project.Module+"/game/handler/syncsender",
		manifest.Project.Module+protocol.PBImportSuffix,
		protocolType, protocolType, protocolType, protocolType, protocolSnake,
		target.alias, target.kind, protocolSnake,
		nestSnake, nestType, call,
		protocolSnake, protocolType,
		useResults, protocolType)
	formatted, err := format.Source([]byte(body))
	if err != nil {
		return nil, fmt.Errorf("format endpoint scaffold: %w\n%s", err, body)
	}
	changes := make([]syncChange, 0, 2)
	if controllerChange != nil {
		changes = append(changes, *controllerChange)
	}
	changes = append(changes, syncChange{rel: relativeSlash(root, endpointPath), path: endpointPath, body: formatted})
	if err := commitSyncChanges(changes); err != nil {
		return nil, err
	}
	paths := []string{relativeSlash(root, endpointPath)}
	if controllerChange != nil {
		paths = append(paths, relativeSlash(root, controllerPath))
	}
	return paths, nil
}

// addLifecycleEndpoint 生成登录边界：endpoint 经 <Entity>FromRegistry 取得实例级 lifecycle，对
// context.PlayerID 调 GetOrCreate，保证之后的玩法请求打到的 Entity 已存在。它不走 Nest Sender：
// 首登时 Entity 还不存在，Nest 冷加载只会回 errcode.CodeEntityNotFound（DESIGN-TODO D7）。
// 业务失败与 Sender 型 endpoint 一样变成 (Code, Reason) 响应，只给 CodeInternal 记日志。
func addLifecycleEndpoint(root string, manifest Manifest, options AddOptions, domain string) ([]string, error) {
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
			return nil, fmt.Errorf("protocol %q does not exist; run: roost add protocol %s --group game --handler %s", protocolSnake, protocolType, domain)
		}
		return nil, err
	}
	lifecyclePath := filepath.Join(root, "game", "lifecycle", entitySnake+".go")
	if _, err := os.Stat(lifecyclePath); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("lifecycle %q does not exist; run: roost add lifecycle %s", entitySnake, toPascal(entitySnake))
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
	controllerDir := filepath.Join(root, "game", "controllers", domain)
	endpointPath := filepath.Join(controllerDir, protocolSnake+".go")
	if _, err := os.Stat(endpointPath); err == nil {
		return nil, fmt.Errorf("%s already exists", relativeSlash(root, endpointPath))
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	controllerPath := filepath.Join(controllerDir, "controller.go")
	var controllerChange *syncChange
	if raw, err := os.ReadFile(controllerPath); os.IsNotExist(err) {
		body, formatErr := format.Source([]byte(renderNestController(domain)))
		if formatErr != nil {
			return nil, formatErr
		}
		change := syncChange{rel: relativeSlash(root, controllerPath), path: controllerPath, body: body}
		controllerChange = &change
	} else if err != nil {
		return nil, err
	} else if !bytes.Contains(raw, []byte("Registry() *app.Registry")) {
		// controller.go 归业务，生成一次后不改写；早于本选项生成的 controller 没有 Registry()。
		return nil, fmt.Errorf("%s has no Registry() accessor; keep the registry passed to FromRegistry and add `func (controller *Controller) Registry() *app.Registry`, then run this command again", relativeSlash(root, controllerPath))
	}
	entityType := toPascal(entitySnake)
	body := fmt.Sprintf(`package %s

import (
	"fmt"
	"log/slog"

	lifecycle %q
	playeraccess %q
	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	%q
)

// Handle%s is the login boundary. It makes sure the authenticated player has
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
	entities, err := lifecycle.%sFromRegistry(controller.Registry())
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
`, domain,
		manifest.Project.Module+"/game/lifecycle",
		manifest.Project.Module+protocol.PlayerAccessImportSuffix,
		manifest.Project.Module+protocol.PBImportSuffix,
		protocolType, entityType,
		protocolType, protocolType, protocolType, protocolSnake,
		entityType, protocolSnake, protocolType, protocolType)
	formatted, err := format.Source([]byte(body))
	if err != nil {
		return nil, fmt.Errorf("format login endpoint scaffold: %w\n%s", err, body)
	}
	changes := make([]syncChange, 0, 2)
	if controllerChange != nil {
		changes = append(changes, *controllerChange)
	}
	changes = append(changes, syncChange{rel: relativeSlash(root, endpointPath), path: endpointPath, body: formatted})
	if err := commitSyncChanges(changes); err != nil {
		return nil, err
	}
	paths := []string{relativeSlash(root, endpointPath)}
	if controllerChange != nil {
		paths = append(paths, relativeSlash(root, controllerPath))
	}
	return paths, nil
}

func renderNestController(domain string) string {
	return fmt.Sprintf(`package %s

import (
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
)

type Controller struct {
	registry   *app.Registry
	nestClient corenest.Client
}

func FromRegistry(registry *app.Registry) (*Controller, error) {
	if registry == nil {
		return nil, fmt.Errorf("%s controller: app registry is required")
	}
	client, ok := app.Lookup[corenest.Client](registry, app.ModName("nest"))
	if !ok || client == nil {
		return nil, fmt.Errorf("%s controller: nest client is unavailable")
	}
	return &Controller{registry: registry, nestClient: client}, nil
}

func (controller *Controller) NestClient() corenest.Client {
	if controller == nil {
		return nil
	}
	return controller.nestClient
}

// Registry is the application registry, for boundaries that are not a Nest
// call: a login endpoint resolves the Entity lifecycle from it.
func (controller *Controller) Registry() *app.Registry {
	if controller == nil {
		return nil
	}
	return controller.registry
}
`, domain, domain, domain)
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
	entity    endpointEntity
}

func endpointArguments(protocolPath, protocolType, handlerPath, handlerType string) (endpointSignature, error) {
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
		signature.entity, err = endpointEntityOf(file, function.Type.Params.List[0].Type, handlerType)
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
				requestField, exists := requestFields[toSnake(parameter)]
				if !exists {
					return signature, fmt.Errorf("protocol %sRequest has no field matching Nest parameter %q", protocolType, parameter)
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
		return signature, nil
	}
	return signature, fmt.Errorf("Nest handler function handler%s not found in %s", handlerType, handlerPath)
}

// endpointEntityOf resolves the handler's first parameter type, written as
// <alias>.I<Component>Entity, to the Entity package it imports. The kind
// constant follows the entity scaffold's naming: EntityKind<Pascal(package)>.
func endpointEntityOf(file *ast.File, expression ast.Expr, handlerType string) (endpointEntity, error) {
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
		return endpointEntity{alias: alias.Name, importPath: importPath, kind: "EntityKind" + toPascal(path.Base(importPath))}, nil
	}
	return endpointEntity{}, fmt.Errorf("Nest handler %s: no import provides package %q for the Entity target", handlerType, alias.Name)
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
