package roost

import (
	"bytes"
	"fmt"
	"github.com/tjbdwanghaibo/roost-codegen/internal/protocol"
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
	persistenceImport := "github.com/tjbdwanghaibo/roost-core/dataengine/engine"
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

	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
	{{PERSISTENCE_IMPORT}}
	"github.com/tjbdwanghaibo/roost-kit/mods"
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
		return nil, err
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

func (lifecycle *%s) GetOrCreate(ctx context.Context, uniqueID int64) (*%s.%s, bool, error) {
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
	return value, err == nil, err
}

func (lifecycle *%s) Destroy(ctx context.Context, value *%s.%s, reason entity.EntityDestroyReason, deletePersisted bool) error {
	if value == nil {
		return Err%sNotFound
	}
	return lifecycle.access.Destroy(ctx, value, reason, deletePersisted)
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
	arguments, target, err := endpointArguments(protocolPath, protocolType, handlerPath, nestType)
	if err != nil {
		return nil, err
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
	} else if !bytes.Contains(raw, []byte("NestClient()")) {
		return nil, fmt.Errorf("%s is not Nest-injected; create a new controller domain or add NestClient() before generating the endpoint", relativeSlash(root, controllerPath))
	}

	callArgs := "entityID"
	if len(arguments) > 0 {
		callArgs += ", " + strings.Join(arguments, ", ")
	}
	body := fmt.Sprintf(`package %s

import (
	"fmt"

	%s %q
	player_agent %q
	%ssyncsender %q
	"github.com/tjbdwanghaibo/roost-core/entity"
	%q
)

// Handle%s is the protocol boundary. Authentication is already represented by
// context.PlayerID; all Entity access continues through the generated Sender.
func (controller *Controller) Handle%s(context *player_agent.Context, request *pb.%sRequest) (*pb.%sResponse, error) {
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
	if err := sender.Sync_%s(context.Context(), %s); err != nil {
		return nil, err
	}
	return &pb.%sResponse{}, nil
}
`, domain,
		target.alias, target.importPath,
		manifest.Project.Module+protocol.PlayerAgentImportSuffix,
		nestSnake, manifest.Project.Module+"/game/handler/syncsender",
		manifest.Project.Module+protocol.PBImportSuffix,
		protocolType, protocolType, protocolType, protocolType, protocolSnake,
		target.alias, target.kind, protocolSnake,
		nestSnake, nestType, nestType, callArgs, protocolType)
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

func renderNestController(domain string) string {
	return fmt.Sprintf(`package %s

import (
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/app"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

type Controller struct {
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
	return &Controller{nestClient: client}, nil
}

func (controller *Controller) NestClient() corenest.Client {
	if controller == nil {
		return nil
	}
	return controller.nestClient
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

func endpointArguments(protocolPath, protocolType, handlerPath, handlerType string) ([]string, endpointEntity, error) {
	var target endpointEntity
	requestFields, err := requestFieldNames(protocolPath, protocolType+"Request")
	if err != nil {
		return nil, target, err
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, handlerPath, nil, parser.ParseComments)
	if err != nil {
		return nil, target, err
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "handler"+handlerType || function.Type.Params == nil {
			continue
		}
		if len(function.Type.Params.List) == 0 {
			return nil, target, fmt.Errorf("Nest handler %s has no Entity target", handlerType)
		}
		if len(function.Type.Params.List[0].Names) != 1 {
			return nil, target, fmt.Errorf("Nest handler %s endpoint requires exactly one named Player Entity target", handlerType)
		}
		target, err = endpointEntityOf(file, function.Type.Params.List[0].Type, handlerType)
		if err != nil {
			return nil, target, err
		}
		var arguments []string
		for index, field := range function.Type.Params.List {
			if index == 0 {
				continue
			}
			if len(field.Names) == 0 {
				return nil, target, fmt.Errorf("Nest handler %s parameter %d must be named", handlerType, index+1)
			}
			for _, name := range field.Names {
				parameter := name.Name
				requestField, exists := requestFields[toSnake(parameter)]
				if !exists {
					return nil, target, fmt.Errorf("protocol %sRequest has no field matching Nest parameter %q", protocolType, parameter)
				}
				arguments = append(arguments, "request."+requestField)
			}
		}
		return arguments, target, nil
	}
	return nil, target, fmt.Errorf("Nest handler function handler%s not found in %s", handlerType, handlerPath)
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

func requestFieldNames(path, typeName string) (map[string]string, error) {
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
				return nil, fmt.Errorf("protocol request %s is not a struct", typeName)
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
	return nil, fmt.Errorf("protocol request %s not found in %s", typeName, path)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
