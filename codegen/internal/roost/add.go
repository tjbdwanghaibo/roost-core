package roost

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"strings"
)

type AddOptions struct {
	Kind, Name, Service, Group, Entity, Component, Handler string
	Protocol, NestHandler                                  string
	Mods, Steps                                            []string
	ID                                                     int64
}

func Add(root string, options AddOptions) ([]string, error) {
	m, err := LoadManifest(root)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(options.Name)
	if !validName(toSnake(name)) {
		return nil, fmt.Errorf("invalid %s name %q", options.Kind, name)
	}
	if options.Kind == "service" {
		manifestBefore, err := os.ReadFile(filepath.Join(root, ManifestName))
		if err != nil {
			return nil, err
		}
		key := toSnake(name)
		if _, exists := m.Services[key]; exists {
			return nil, fmt.Errorf("service %s already exists", key)
		}
		if _, err := resolveMods(options.Mods); err != nil {
			return nil, err
		}
		m.Services[key] = ServiceSpec{Mods: uniqueSorted(options.Mods)}
		if err := commitManifestSync(root, manifestBefore, m); err != nil {
			return nil, err
		}
		return []string{"roost.yaml", "internal/service/" + key + "/service.go"}, nil
	}
	if options.Kind == "mod" {
		manifestBefore, err := os.ReadFile(filepath.Join(root, ManifestName))
		if err != nil {
			return nil, err
		}
		service := toSnake(options.Service)
		spec, ok := m.Services[service]
		if !ok {
			return nil, fmt.Errorf("unknown service %q", service)
		}
		if _, ok := modCatalog[toSnake(name)]; !ok {
			return nil, fmt.Errorf("unknown kit mod %q", name)
		}
		// The spec's Mods slice may share its array with the manifest's;
		// copy before appending so the "before" view stays what it was.
		previous := manifestWithService(m, service, ServiceSpec{Framework: spec.Framework, Mods: append([]string(nil), spec.Mods...), Uses: spec.Uses, Rpcs: spec.Rpcs, UsesRpcs: spec.UsesRpcs})
		spec.Mods = uniqueSorted(append(append([]string(nil), spec.Mods...), toSnake(name)))
		m.Services[service] = spec
		if err := commitManifestSync(root, manifestBefore, m); err != nil {
			return nil, err
		}
		// The service's configs were rendered at creation; give the new Mod
		// its section there too, or it runs on defaults nobody can see.
		configs, err := appendModConfigSections(root, previous, m, service)
		if err != nil {
			return []string{"roost.yaml"}, err
		}
		return append([]string{"roost.yaml"}, configs...), nil
	}
	if options.Kind == "access" {
		manifestBefore, err := os.ReadFile(filepath.Join(root, ManifestName))
		if err != nil {
			return nil, err
		}
		accessName := toSnake(name)
		if accessName != "player" {
			return nil, fmt.Errorf("unsupported access layer %q; supported: player", accessName)
		}
		service := toSnake(options.Service)
		if service == "" {
			services := sortedServiceNames(m)
			if len(services) != 1 {
				return nil, errors.New("player access requires --service when the project has multiple services")
			}
			service = services[0]
		}
		spec, exists := m.Services[service]
		if !exists {
			return nil, fmt.Errorf("unknown service %q", service)
		}
		if _, exists := m.Access[accessName]; exists {
			return nil, fmt.Errorf("access layer %s already exists", accessName)
		}
		if m.Access == nil {
			m.Access = make(map[string]AccessSpec)
		}
		m.Access[accessName] = AccessSpec{Service: service}
		m.Features = uniqueSorted(append(m.Features, "protocol", "nest"))
		spec.Mods = uniqueSorted(append(spec.Mods, "nest"))
		m.Services[service] = spec
		if err := commitManifestSync(root, manifestBefore, m); err != nil {
			return nil, err
		}
		return []string{
			"roost.yaml",
			"game/player_agent/runtime_gen.go",
			"internal/access/player/mod_gen.go",
		}, nil
	}
	if options.Kind == "transport" {
		transport := toSnake(name)
		if transport != "tcp" {
			return nil, fmt.Errorf("unsupported player transport %q; supported: tcp", transport)
		}
		access, exists := m.Access["player"]
		if !exists {
			return nil, errors.New("player transport requires an access layer; run: roost add access player --service <service>")
		}
		if service := toSnake(options.Service); service != "" && service != access.Service {
			return nil, fmt.Errorf("player access belongs to service %q, not %q", access.Service, service)
		}
		if contains(access.Transports, transport) {
			return nil, fmt.Errorf("player transport %s already exists", transport)
		}
		configPaths := []string{
			"configs/service/config." + access.Service + ".yaml",
			"configs/service/config." + access.Service + ".prod.example.yaml",
		}
		backups, backupErr := captureFiles(root, configPaths)
		if backupErr != nil {
			return nil, backupErr
		}
		manifestBefore, err := os.ReadFile(filepath.Join(root, ManifestName))
		if err != nil {
			return nil, err
		}
		access.Transports = uniqueSorted(append(access.Transports, transport))
		m.Access["player"] = access
		developmentConfig, err := ensurePlayerTCPConfig(root, access.Service, "", false)
		if err != nil {
			return nil, restoreFiles(root, backups, fmt.Errorf("configure player tcp: %w", err))
		}
		productionConfig := filepath.Join("configs", "service", "config."+access.Service+".prod.example.yaml")
		if _, err := ensurePlayerTCPConfig(root, access.Service, productionConfig, false); err != nil {
			return nil, restoreFiles(root, backups, fmt.Errorf("configure production player tcp example: %w", err))
		}
		configured, err := captureFiles(root, configPaths)
		if err != nil {
			return nil, restoreFiles(root, backups, err)
		}
		if err := commitManifestSync(root, manifestBefore, m); err != nil {
			return nil, restoreFilesIfCurrent(root, backups, configured, err)
		}
		return []string{
			"roost.yaml",
			"internal/access/player/tcp/server_gen.go",
			"internal/access/player/tcp/server_gen_test.go",
			"internal/access/player/tcp/auth.go",
			"cmd/playerprobe/main.go",
			"configs/examples/access.player.tcp.yaml",
			filepath.ToSlash(developmentConfig),
			filepath.ToSlash(productionConfig),
			"docs/PLAYER_ACCESS_TCP.zh-CN.md",
			"internal/bootstrap/generated.go",
			"Dockerfile",
			"deploy/docker/README.md",
			"deploy/k8s/base/network-policy.yaml",
			"deploy/k8s/base/" + access.Service + ".yaml",
		}, nil
	}
	if options.Kind == "saga" {
		rollbackPaths := []string{"saga/" + toSnake(options.Name) + "/definition.go"}
		backups, backupErr := captureFiles(root, rollbackPaths)
		if backupErr != nil {
			return nil, backupErr
		}
		manifestBefore, err := os.ReadFile(filepath.Join(root, ManifestName))
		if err != nil {
			return nil, err
		}
		service := toSnake(options.Service)
		if service == "" {
			services := sortedServiceNames(m)
			if len(services) != 1 {
				return nil, fmt.Errorf("saga requires -service when the project has multiple services")
			}
			service = services[0]
		}
		spec, ok := m.Services[service]
		if !ok {
			return nil, fmt.Errorf("unknown service %q", service)
		}
		paths, addErr := addArtifact(root, m, options)
		if addErr != nil {
			return nil, addErr
		}
		created, captureErr := captureFiles(root, rollbackPaths)
		if captureErr != nil {
			return paths, restoreFiles(root, backups, captureErr)
		}
		previous := manifestWithService(m, service, ServiceSpec{Framework: spec.Framework, Mods: append([]string(nil), spec.Mods...), Uses: spec.Uses, Rpcs: spec.Rpcs, UsesRpcs: spec.UsesRpcs})
		spec.Mods = uniqueSorted(append(append([]string(nil), spec.Mods...), "saga"))
		m.Services[service] = spec
		m.Features = uniqueSorted(append(m.Features, "saga"))
		m.Sagas = uniqueSorted(append(m.Sagas, toSnake(options.Name)))
		if err := commitManifestSync(root, manifestBefore, m); err != nil {
			return paths, restoreFilesIfCurrent(root, backups, created, err)
		}
		paths = append(paths, "roost.yaml", "internal/bootstrap/generated.go")
		// The saga Mod (and whatever it pulled in) gets its config section in
		// the service's configs, like a Mod chosen at project creation.
		configs, err := appendModConfigSections(root, previous, m, service)
		if err != nil {
			return paths, err
		}
		return append(paths, configs...), nil
	}
	if options.Kind == "skill" {
		return addSkillDefinition(root, m, options)
	}
	if options.Kind == "rpc" {
		return addRPC(root, m, options)
	}
	return addArtifact(root, m, options)
}

type fileBackup struct {
	body    []byte
	existed bool
}

func captureFiles(root string, paths []string) (map[string]fileBackup, error) {
	backups := make(map[string]fileBackup, len(paths))
	for _, rel := range paths {
		path := filepath.Join(root, filepath.FromSlash(rel))
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			backups[rel] = fileBackup{body: raw, existed: true}
		case os.IsNotExist(err):
			backups[rel] = fileBackup{}
		default:
			return nil, err
		}
	}
	return backups, nil
}

func restoreFiles(root string, backups map[string]fileBackup, cause error) error {
	joined := cause
	for rel, backup := range backups {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if backup.existed {
			if err := writeAtomic(path, backup.body, 0o644); err != nil {
				joined = errors.Join(joined, fmt.Errorf("rollback %s: %w", rel, err))
			}
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			joined = errors.Join(joined, fmt.Errorf("rollback new %s: %w", rel, err))
		}
	}
	return joined
}

func restoreFilesIfCurrent(root string, backups, expected map[string]fileBackup, cause error) error {
	joined := cause
	for rel, backup := range backups {
		current, err := captureFiles(root, []string{rel})
		if err != nil {
			joined = errors.Join(joined, fmt.Errorf("inspect %s before rollback: %w", rel, err))
			continue
		}
		want, ok := expected[rel]
		got := current[rel]
		if !ok || got.existed != want.existed || !bytes.Equal(got.body, want.body) {
			joined = errors.Join(joined, fmt.Errorf("rollback skipped concurrently changed file %s", rel))
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(rel))
		if backup.existed {
			if err := writeAtomic(path, backup.body, 0o644); err != nil {
				joined = errors.Join(joined, fmt.Errorf("rollback %s: %w", rel, err))
			}
		} else if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			joined = errors.Join(joined, fmt.Errorf("rollback new %s: %w", rel, err))
		}
	}
	return joined
}

func commitManifestSync(root string, manifestBefore []byte, manifest Manifest) error {
	_, err := commitManifestSyncResult(root, manifestBefore, manifest)
	return err
}

func commitManifestSyncResult(root string, manifestBefore []byte, manifest Manifest) (SyncResult, error) {
	manifestPath := filepath.Join(root, ManifestName)
	manifestRaw, err := manifest.Marshal()
	if err != nil {
		return SyncResult{}, err
	}
	manifestChange := syncChange{rel: ManifestName, path: manifestPath, before: manifestBefore, body: manifestRaw, existed: true}
	if err := verifySyncChangeCurrent(manifestChange); err != nil {
		return SyncResult{}, err
	}
	if err := writeAtomic(manifestPath, manifestRaw, 0o644); err != nil {
		return SyncResult{}, err
	}
	result, err := SyncProject(root)
	if err != nil {
		rollbackErr := rollbackSync([]syncChange{manifestChange})
		if rollbackErr != nil {
			return SyncResult{}, errors.Join(err, fmt.Errorf("rollback %s: %w", ManifestName, rollbackErr))
		}
		return SyncResult{}, err
	}
	return result, nil
}

func addArtifact(root string, m Manifest, o AddOptions) ([]string, error) {
	snake := toSnake(o.Name)
	pascal := toPascal(o.Name)
	if o.Kind == "protocol" || o.Kind == "entity" || o.Kind == "component" || o.Kind == "errcode" {
		o.Group = toSnake(o.Group)
		if o.Group != "" && !validName(o.Group) {
			return nil, fmt.Errorf("invalid %s group %q", o.Kind, o.Group)
		}
		if o.ID == 0 {
			id, err := NextID(root, m, o.Kind, o.Group)
			if err != nil {
				return nil, err
			}
			o.ID = id
		} else {
			if err := validateExplicitID(m, o.Kind, o.Group, o.ID); err != nil {
				return nil, err
			}
			uses, err := ScanIDs(root)
			if err != nil {
				return nil, err
			}
			for _, use := range uses {
				if use.Kind == o.Kind && use.ID == o.ID {
					return nil, fmt.Errorf("%s id %d is already used by %s", o.Kind, o.ID, use.File)
				}
			}
		}
	}
	var path, body string
	switch o.Kind {
	case "protocol":
		handler := toSnake(o.Handler)
		if handler != "" && !validName(handler) {
			return nil, fmt.Errorf("invalid protocol handler %q", o.Handler)
		}
		protocolMarker := "//roost:protocol"
		if group := o.Group; group != "" {
			protocolMarker += " group=" + group
		}
		if handler != "" {
			protocolMarker += " handler=" + handler
		}
		path = "protocol/def/" + snake + ".go"
		body = fmt.Sprintf("//go:build protocoldef\n\npackage protocoldef\n\ntype %sRequest struct{}\ntype %sResponse struct {\n\tCode int32 `pb:\"1\"`\n\tReason string `pb:\"2\"`\n}\n\n%s\ntype %sProtocol interface {\n\t//roost:msg id=%d\n\t%s(%sRequest) %sResponse\n}\n", pascal, pascal, protocolMarker, pascal, o.ID, pascal, pascal, pascal)
	case "entity":
		path = "game/entities/" + snake + "/entity.go"
		// The category goes on the marker, which makes it the single place the
		// kind's category is written: the generated wiring pastes it in, so the
		// hand-written MustRegisterEntityKindCategory line is gone and with it
		// the implicit "that line must run before any RegisterEntity" ordering.
		// Other is the safe default; a scaffold must never mint a per-entity
		// "= 1", which is the rank reserved for remote-managed kinds.
		body = fmt.Sprintf("package %s\n\nimport \"github.com/tjbdwanghaibo/roost-core/entity\"\n\nconst EntityKind%s entity.EntityKind = %d\n\n// I%sEntity is the lock-safe business view used by Nest handlers.\ntype I%sEntity interface { entity.IThreadSafeEntity }\n\n// category is this kind's lock rank, acquired lowest first. Other is the safe\n// default: holding it permits acquiring nothing further. Move the kind to\n// EntityCategoryWorld / PlayerScoped / Player once its ordering is known, and\n// to EntityCategoryRemote if it becomes remote=managed.\n//roost:entity id=%d entityKind=EntityKind%s category=entity.EntityCategoryOther\ntype %s struct {\n\t*entity.EntityBase\n\tentity.ComponentManager\n\tentity.DaoManager\n}\n", snake, pascal, o.ID, pascal, pascal, o.ID, pascal, pascal)
	case "component":
		return addEntityComponent(root, m, o)
	case "handler":
		if !hasFeature(m, "nest") {
			return nil, errors.New("handler requires the nest feature in roost.yaml")
		}
		if !contains(allProjectMods(m), "nest") {
			services := sortedServiceNames(m)
			if len(services) == 1 {
				return nil, fmt.Errorf("handler requires the nest runtime mod; run: roost add mod nest -service %s", services[0])
			}
			return nil, errors.New("handler requires the nest runtime mod; choose its owning service and run: roost add mod nest -service <service>")
		}
		entityName, _, err := resolveEntitySource(root, o.Entity)
		if err != nil {
			return nil, err
		}
		componentName := toSnake(o.Component)
		if componentName == "" {
			return nil, errors.New("handler requires --component <name>")
		}
		componentPath := filepath.Join(root, "game", "entities", entityName, componentName+"_component.go")
		if _, err := os.Stat(componentPath); err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("component %q is not attached to Entity %q; run: roost add component %s --entity %s", componentName, entityName, componentName, entityName)
			}
			return nil, err
		}
		path = "game/handler/" + snake + ".go"
		body = fmt.Sprintf("package handler\n\nimport %s %q\n\n//roost:nest rollback=undo durability=strict\nfunc handler%s(target %s.I%sEntity) error {\n\t// TODO: validate input and call target.%sComp().<BusinessMethod>().\n\treturn nil\n}\n", entityName, m.Project.Module+"/game/entities/"+entityName, pascal, entityName, toPascal(componentName), toPascal(componentName))
	case "lifecycle":
		return addEntityLifecycle(root, m, o)
	case "endpoint":
		return addProtocolEndpoint(root, m, o)
	case "event":
		path = "event/def/" + snake + ".go"
		body = fmt.Sprintf("package eventdef\n\ntype Event%s struct{}\n", pascal)
	case "table":
		path = "configs/schema/" + snake + ".go"
		body = fmt.Sprintf("package schema\n\n//roost:table name=%s key=ID\ntype %s struct {\n\tID int64 `csv:\"id\" json:\"id\" title:\"ID\" required:\"true\" unique:\"true\"`\n}\n", snake, pascal)
	case "dao":
		if strings.TrimSpace(o.Entity) != "" {
			return addEntityDAO(root, m, o)
		}
		path = "db/def/" + snake + ".go"
		body = renderDAODefinition(snake, pascal+"Dao")
	case "webroute":
		path = "service/web/" + snake + ".go"
		body = fmt.Sprintf("package web\n\nimport (\n\t\"context\"\n\t\"github.com/tjbdwanghaibo/roost-core/webroute\"\n)\n\ntype %sResponse struct{}\n\n//roost:web method=GET path=/%s body=raw\nfunc %s(context.Context, *Service, webroute.RawRequest) (%sResponse, error) { return %sResponse{}, nil }\n", pascal, snake, pascal, pascal, pascal)
	case "errcode":
		path = "internal/errors/" + snake + ".go"
		body = fmt.Sprintf("package errors\n\nimport \"github.com/tjbdwanghaibo/roost-core/errcode\"\n\nvar Err%s = errcode.Define(%d, %q, %q)\n", pascal, o.ID, snake, "TODO")
	case "module":
		path = "game/controllers/" + snake + "/controller.go"
		body = fmt.Sprintf("package %s\n\nimport \"github.com/tjbdwanghaibo/roost-core/app\"\n\ntype Controller struct{}\nfunc FromRegistry(*app.Registry) (Controller, error) { return Controller{}, nil }\n", snake)
	case "saga":
		if len(o.Steps) == 0 {
			return nil, fmt.Errorf("saga %s requires -steps", o.Name)
		}
		path = "saga/" + snake + "/definition.go"
		var steps strings.Builder
		var topics strings.Builder
		var subscribers strings.Builder
		for _, stepName := range o.Steps {
			stepSnake, stepPascal := toSnake(stepName), toPascal(stepName)
			if !validName(stepSnake) {
				return nil, fmt.Errorf("invalid saga step %q", stepName)
			}
			fmt.Fprintf(&steps, "\t\t{Name: %q, ForwardTopic: Topic%s, CompensateTopic: Topic%sCompensation, Timeout: 5 * time.Second, MaxAttempts: 5, BackoffMin: 100 * time.Millisecond, BackoffMax: 5 * time.Second},\n", stepSnake, stepPascal, stepPascal)
			// The topics as constants, so a project can address them without
			// repeating the strings: the Subscribe* helpers below take a
			// Mongo inbox, and a step whose business is a Nest transaction
			// subscribes with saga.SubscribeDataEngineStep and these topics
			// instead (roost-core/saga's native path).
			fmt.Fprintf(&topics, "\t// Topic%s is the forward topic of step %s.\n", stepPascal, stepSnake)
			fmt.Fprintf(&topics, "\tTopic%s = %q\n", stepPascal, snake+"."+stepSnake)
			fmt.Fprintf(&topics, "\t// Topic%sCompensation is its compensation topic.\n", stepPascal)
			fmt.Fprintf(&topics, "\tTopic%sCompensation = %q\n", stepPascal, snake+"."+stepSnake+".compensate")
			fmt.Fprintf(&subscribers, "\nfunc Subscribe%s(ctx context.Context, client fnats.IJetStream, transport *saga.JetStreamPublisher, inbox *saga.MongoCommandInbox, stream, durable string, handler saga.StepHandler) (fnats.IJetStreamSubscription, error) {\n\treturn saga.SubscribeMongoStep(ctx, client, transport, inbox, saga.StepConsumerConfig{Stream: stream, Durable: durable, Topic: %q}, handler)\n}\n\nfunc Subscribe%sCompensation(ctx context.Context, client fnats.IJetStream, transport *saga.JetStreamPublisher, inbox *saga.MongoCommandInbox, stream, durable string, handler saga.StepHandler) (fnats.IJetStreamSubscription, error) {\n\treturn saga.SubscribeMongoStep(ctx, client, transport, inbox, saga.StepConsumerConfig{Stream: stream, Durable: durable, Topic: %q}, handler)\n}\n", stepPascal, snake+"."+stepSnake, stepPascal, snake+"."+stepSnake+".compensate")
		}
		body = fmt.Sprintf(`package %s

import (
	"context"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	"github.com/tjbdwanghaibo/roost-core/saga"
)

const (
	Type = %q
	Version uint32 = 1
)

// The step topics. A step whose business is a Nest transaction subscribes
// with saga.SubscribeDataEngineStep and binds its receipt inside that
// transaction; one whose business is a call into another service uses the
// Mongo inbox helpers below. Both address the same topics.
const (
%s)

func Definition() saga.Definition {
	return saga.Definition{Type: Type, Version: Version, Steps: []saga.Step{
%s	}}
}

// Definitions must retain every version which still has non-terminal records.
// When changing step order or compensation semantics, keep the old definition
// here and make Definition return the new version.
func Definitions() []saga.Definition { return []saga.Definition{Definition()} }

func Register(engine *saga.Engine) error {
	for _, definition := range Definitions() {
		if err := engine.Register(definition); err != nil {
			return err
		}
	}
	return nil
}

// EmitStart is the production entry point from a Nest handler: the Saga start
// intent and current Entity mutations are committed in the same Nest WAL record.
func EmitStart(businessKey string, state []byte, deadline time.Time) error {
	return saga.EmitStart(saga.StartRequest{Type: Type, DefinitionVersion: Version, BusinessKey: businessKey, Data: state, DeadlineAt: deadline})
}

// Start is for durable consumers, administration and recovery paths which are
// already outside a Nest transaction. Calls are idempotent for one intent.
func Start(ctx context.Context, engine *saga.Engine, businessKey string, state []byte, deadline time.Time) (saga.Record, error) {
	return engine.StartSaga(ctx, saga.StartRequest{Type: Type, DefinitionVersion: Version, BusinessKey: businessKey, Data: state, DeadlineAt: deadline})
}
%s`, snake, snake, topics.String(), steps.String(), subscribers.String())
	default:
		return nil, fmt.Errorf("unsupported artifact kind %q", o.Kind)
	}
	formatted, err := format.Source([]byte(body))
	if err != nil {
		return nil, err
	}
	abs := filepath.Join(root, filepath.FromSlash(path))
	if _, err := os.Stat(abs); err == nil {
		return nil, fmt.Errorf("%s already exists", path)
	}
	if err := commitSyncChanges([]syncChange{{rel: path, path: abs, body: formatted}}); err != nil {
		return nil, err
	}
	return []string{path}, nil
}

func validateExplicitID(manifest Manifest, kind, group string, id int64) error {
	space, exists := manifest.IDs[kind]
	if !exists {
		return fmt.Errorf("id space %q is not configured", kind)
	}
	selected := IDRange{Min: space.Min, Max: space.Max}
	if group != "" {
		var ok bool
		selected, ok = space.Groups[group]
		if !ok {
			return fmt.Errorf("id group %q is not configured for %s", group, kind)
		}
	}
	if selected.Min <= 0 || selected.Max < selected.Min {
		return fmt.Errorf("id space %s has no usable range", kind)
	}
	if id < selected.Min || id > selected.Max {
		return fmt.Errorf("%s id %d is outside %d-%d", kind, id, selected.Min, selected.Max)
	}
	return nil
}

func saveManifest(root string, m Manifest) error {
	raw, err := m.Marshal()
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(root, ManifestName), raw, 0o644)
}

func toSnake(value string) string { return strings.ToLower(strings.Join(splitWords(value), "_")) }
func toPascal(value string) string {
	parts := splitWords(value)
	for i := range parts {
		parts[i] = strings.ToUpper(parts[i][:1]) + strings.ToLower(parts[i][1:])
	}
	return strings.Join(parts, "")
}
func splitWords(value string) []string {
	value = strings.TrimSpace(value)
	var parts []string
	var current []rune
	for i, r := range []rune(value) {
		if r == '-' || r == '_' || r == ' ' {
			if len(current) > 0 {
				parts = append(parts, string(current))
				current = nil
			}
			continue
		}
		if i > 0 && r >= 'A' && r <= 'Z' && len(current) > 0 {
			parts = append(parts, string(current))
			current = nil
		}
		current = append(current, r)
	}
	if len(current) > 0 {
		parts = append(parts, string(current))
	}
	return parts
}
