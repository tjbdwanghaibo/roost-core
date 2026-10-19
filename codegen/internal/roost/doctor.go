package roost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/marker"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type CheckStatus string

const (
	StatusOK   CheckStatus = "ok"
	StatusWarn CheckStatus = "warn"
	StatusFail CheckStatus = "fail"
)

type CheckItem struct {
	Name   string      `json:"name"`
	Status CheckStatus `json:"status"`
	Detail string      `json:"detail"`
}

type DoctorReport struct {
	Items []CheckItem `json:"items"`
}

type DoctorOptions struct {
	Strict     bool
	JSONOutput bool
	Workflow   string
}

// DoctorWithOptions checks the project and prints the report. When ctx ends
// (roost was interrupted), it returns the interruption instead of a report
// whose go checks were cut short.
func DoctorWithOptions(ctx context.Context, root string, options DoctorOptions, stdout io.Writer) error {
	m, err := LoadManifest(root)
	if err != nil {
		return err
	}
	report := DoctorReport{Items: []CheckItem{{Name: "manifest", Status: StatusOK, Detail: ManifestName}}}
	goAvailable := false
	for _, command := range []string{"go", "git"} {
		if _, err := exec.LookPath(command); err != nil {
			report.Items = append(report.Items, CheckItem{Name: command, Status: StatusFail, Detail: err.Error()})
		} else {
			report.Items = append(report.Items, CheckItem{Name: command, Status: StatusOK, Detail: "available"})
			if command == "go" {
				goAvailable = true
			}
		}
	}
	// 业务声明只有编译后的进程知道：工程能编译时，config-schema 检查编译一次进程、读回每个服务的
	// 全部声明；编译不过（compile:go-list 已 FAIL）时只按框架声明检查。
	var processSchemas serviceSchemaSource
	var process *projectProcess
	if goAvailable {
		report.Items = append(report.Items, runDoctorGoCommand(ctx, root, "dependencies:go-mod-verify", 2*time.Minute, "go mod verify passed", "run GOWORK=off go mod download, then retry", "mod", "verify"))
		list := runDoctorGoCommand(ctx, root, "compile:go-list", 2*time.Minute, "all packages loaded with -mod=readonly", "fix package/import errors without changing go.mod, then retry; after a failed or interrupted project upgrade run roost project deps first", "list", "-buildvcs=false", "-mod=readonly", "./...")
		report.Items = append(report.Items, list)
		if list.Status == StatusOK {
			if item, ok := checkSkillCatalog(ctx, root); ok {
				report.Items = append(report.Items, item)
			}
			built, cleanup := newProjectProcess(ctx, root)
			defer cleanup()
			process, processSchemas = built, built.schema
		}
		if options.Strict {
			report.Items = append(report.Items, runDoctorGoCommand(ctx, root, "compile:go-test", 5*time.Minute, "all packages and tests compiled", "run GOWORK=off go test -buildvcs=false -mod=readonly -run=^$ ./... and fix the reported compiler error", "test", "-buildvcs=false", "-mod=readonly", "-run=^$", "./..."))
		}
	}
	for _, service := range sortedServiceNames(m) {
		path := filepath.Join(root, "configs", "service", "config."+service+".yaml")
		if err := CheckConfig(path, false); err != nil {
			report.Items = append(report.Items, CheckItem{Name: "config:" + service, Status: StatusFail, Detail: err.Error()})
		} else {
			report.Items = append(report.Items, CheckItem{Name: "config:" + service, Status: StatusOK, Detail: filepath.ToSlash(path)})
		}
	}
	report.Items = append(report.Items, checkConfigDeclarations(root, m, processSchemas)...)
	report.Items = append(report.Items, checkConfigRules(root, m, process)...)
	report.Items = append(report.Items, checkConfigSections(root, m)...)
	report.Items = append(report.Items, checkConfigAcrossServicesItem(root, m))
	report.Items = append(report.Items, checkConfigReads(root))
	report.Items = append(report.Items, checkShutdownBudgets(root, m)...)
	report.Items = append(report.Items, checkLogicOffsets(root, m))
	if err := CheckIDs(root, m); err != nil {
		report.Items = append(report.Items, CheckItem{Name: "ids", Status: StatusFail, Detail: err.Error()})
	} else {
		report.Items = append(report.Items, CheckItem{Name: "ids", Status: StatusOK, Detail: "no conflicts"})
	}
	report.Items = append(report.Items, checkCICDTemplates(root)...)
	report.Items = append(report.Items, checkFrameworkCollaborators(root, m)...)
	if item, ok := checkRemoteRuntime(root, m); ok {
		report.Items = append(report.Items, item)
	}
	report.Items = append(report.Items, checkStepAndEffectWiring(root, m)...)
	report.Items = append(report.Items, checkWebRouteServing(root, m)...)
	if selfSyncEnabled(m) {
		if status, err := checkSelfSync(root, m); err != nil {
			report.Items = append(report.Items, CheckItem{Name: "self-sync", Status: StatusFail, Detail: err.Error()})
		} else if status.warning != "" {
			report.Items = append(report.Items, CheckItem{Name: "self-sync", Status: StatusWarn, Detail: status.warning})
		} else {
			report.Items = append(report.Items, CheckItem{Name: "self-sync", Status: StatusOK, Detail: "player Entity syncs through " + status.packer + "; push id in " + selfSyncProtocolPath})
		}
	}
	if workflow := strings.TrimSpace(options.Workflow); workflow != "" {
		items, workflowErr := checkWorkflow(root, m, workflow)
		if workflowErr != nil {
			return workflowErr
		}
		report.Items = append(report.Items, items...)
	}
	if options.Strict {
		if err := generate(ctx, root, GenerateOptions{Check: true, Stdout: io.Discard}); err != nil {
			report.Items = append(report.Items, CheckItem{Name: "generated", Status: StatusFail, Detail: err.Error()})
		} else {
			report.Items = append(report.Items, CheckItem{Name: "generated", Status: StatusOK, Detail: "up to date"})
		}
		var diff bytes.Buffer
		if err := DiffProject(root, &diff); err != nil {
			report.Items = append(report.Items, CheckItem{Name: "project-templates", Status: StatusFail, Detail: err.Error()})
		} else if !strings.Contains(diff.String(), "summary: 0 file(s) would change") {
			report.Items = append(report.Items, CheckItem{Name: "project-templates", Status: StatusFail, Detail: "managed templates are stale; run make sync"})
		} else {
			report.Items = append(report.Items, CheckItem{Name: "project-templates", Status: StatusOK, Detail: "up to date"})
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("doctor interrupted, no report: %w", err)
	}
	if options.JSONOutput {
		raw, _ := json.MarshalIndent(report, "", "  ")
		_, _ = fmt.Fprintln(stdout, string(raw))
	} else {
		for _, item := range report.Items {
			fmt.Fprintf(stdout, "%-5s %-20s %s\n", strings.ToUpper(string(item.Status)), item.Name, item.Detail)
		}
	}
	var failed []error
	for _, item := range report.Items {
		if item.Status == StatusFail {
			failed = append(failed, errors.New(item.Name+": "+item.Detail))
		}
	}
	return errors.Join(failed...)
}

func runDoctorGoCommand(ctx context.Context, root, name string, timeout time.Duration, success, fix string, args ...string) CheckItem {
	return runDoctorCommand(ctx, "go", root, name, timeout, success, fix, args...)
}

// runDoctorCommand is runDoctorGoCommand with the binary as a parameter, so
// tests can stand in for the go tool.
func runDoctorCommand(ctx context.Context, binary, root, name string, timeout time.Duration, success, fix string, args ...string) CheckItem {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return runDoctorCommandUntil(ctx, binary, root, name, timeout, success, fix, args...)
}

// runDoctorCommandUntil runs the command until ctx ends; a ctx that ends with
// context.DeadlineExceeded is reported as having timed out after timeout. It
// lets tests decide when the deadline passes instead of racing a timer.
func runDoctorCommandUntil(ctx context.Context, binary, root, name string, timeout time.Duration, success, fix string, args ...string) CheckItem {
	// a tree, so the timeout also kills go's compile / link
	// children and their copy of the output pipe cannot hold the call open.
	var output bytes.Buffer
	err := runCommandTree(ctx, root, append(os.Environ(), "GOWORK=off"), &output, &output, binary, args...)
	if err == nil {
		return CheckItem{Name: name, Status: StatusOK, Detail: success}
	}
	detail := strings.TrimSpace(output.String())
	if len(detail) > 2048 {
		detail = detail[len(detail)-2048:]
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		detail = "timed out after " + timeout.String()
	}
	if detail == "" {
		detail = err.Error()
	}
	return CheckItem{Name: name, Status: StatusFail, Detail: detail + "; fix: " + fix}
}

func checkCICDTemplates(root string) []CheckItem {
	paths := []string{
		".github/workflows/ci.yml",
		".github/workflows/dependency-update.yml",
		".github/workflows/release.yml",
		".github/workflows/deploy-shell.yml",
		".github/workflows/deploy-docker.yml",
		".github/workflows/deploy-k8s.yml",
		"deploy/shell/rollback.sh",
		"deploy/docker/docker-compose.prod.yaml",
		"deploy/k8s/overlays/staging/kustomization.yaml",
		"deploy/k8s/overlays/production/kustomization.yaml",
	}
	items := make([]CheckItem, 0, len(paths)+1)
	for _, rel := range paths {
		if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil || !info.Mode().IsRegular() {
			detail := "missing; run make sync"
			if err != nil && !os.IsNotExist(err) {
				detail = err.Error()
			}
			items = append(items, CheckItem{Name: "cicd:" + rel, Status: StatusFail, Detail: detail})
		} else {
			items = append(items, CheckItem{Name: "cicd:" + rel, Status: StatusOK, Detail: "present"})
		}
	}
	ci, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err == nil && (bytes.Contains(ci, []byte("make deps-update")) || bytes.Contains(ci, []byte("go get -u"))) {
		items = append(items, CheckItem{Name: "cicd:reproducible", Status: StatusFail, Detail: "ordinary CI must not resolve latest dependencies; run make sync"})
	} else if err == nil {
		items = append(items, CheckItem{Name: "cicd:reproducible", Status: StatusOK, Detail: "ordinary CI uses committed go.mod/go.sum"})
	}
	return items
}

// checkSkillCatalog 是 doctor 的 skills:catalog 项：工程有技能目录（game/skills/catalog.go）时运行它的测试，
// 测试用框架的正式加载路径编译全部 JSON（renderSkillCatalogTest），写坏的定义在这里 FAIL。没有测试文件时 WARN：
// 那样没有任何地方在启动前编译这些定义。没有技能目录时不报这一项。
func checkSkillCatalog(ctx context.Context, root string) (CheckItem, bool) {
	if _, err := os.Stat(filepath.Join(root, "game", "skills", "catalog.go")); err != nil {
		return CheckItem{}, false
	}
	if _, err := os.Stat(filepath.Join(root, "game", "skills", "catalog_test.go")); err != nil {
		return CheckItem{Name: "skills:catalog", Status: StatusWarn, Detail: "game/skills has no catalog_test.go, so nothing compiles the Skill definitions before startup; " +
			"the next roost add skill creates one (TestSkillCatalogCompiles calls CompileAll), or write it by hand"}, true
	}
	return runDoctorGoCommand(ctx, root, "skills:catalog", 3*time.Minute, "every game/skills/*.json compiles (go test ./game/skills/)",
		"fix the Skill definition named above, then run GOWORK=off go test ./game/skills/", "test", "-buildvcs=false", "-mod=readonly", "-count=1", "./game/skills/"), true
}

// checkRemoteRuntime 是 doctor 的 remote-runtime 项：工程里有 remote=managed 实体时，带 Nest 的服务要有
// remote_entity Mod 才写得了它（规则与 generate 相同，见 remoteRuntimeHosts）。没有这类实体时不报这一项。
func checkRemoteRuntime(root string, m Manifest) (CheckItem, bool) {
	missing, partial, err := remoteRuntimeHosts(root, m)
	switch {
	case err != nil:
		return CheckItem{Name: "remote-runtime", Status: StatusFail, Detail: "scan entity markers: " + err.Error()}, true
	case missing != "":
		return CheckItem{Name: "remote-runtime", Status: StatusFail, Detail: missing}, true
	case partial != "":
		return CheckItem{Name: "remote-runtime", Status: StatusWarn, Detail: partial}, true
	}
	kinds, err := projectEntityKinds(root)
	if err != nil {
		return CheckItem{}, false
	}
	for _, kind := range kinds {
		if kind.managed {
			return CheckItem{Name: "remote-runtime", Status: StatusOK, Detail: "every service running Nest has the remote_entity Mod for the remote=managed entities"}, true
		}
	}
	return CheckItem{}, false
}

// collaboratorUnconfiguredMarker is the phrase every generated fail-closed
// collaborator stub carries. Its presence in a hosted service's
// collaborators.go means that service refuses everything until the project
// implements the collaborator — worth saying before the first deploy.
const collaboratorUnconfiguredMarker = "is not configured"

// checkFrameworkCollaborators reports, per hosted framework service, whether
// the project has replaced the generated fail-closed collaborators.
func checkFrameworkCollaborators(root string, manifest Manifest) []CheckItem {
	var items []CheckItem
	for _, name := range sortedServiceNames(manifest) {
		if !manifest.isFrameworkService(name) {
			continue
		}
		rel := "internal/service/" + name + "/collaborators.go"
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			items = append(items, CheckItem{Name: "collaborators:" + name, Status: StatusFail, Detail: rel + " is missing; run make sync"})
			continue
		}
		if strings.Contains(string(raw), collaboratorUnconfiguredMarker) {
			items = append(items, CheckItem{Name: "collaborators:" + name, Status: StatusFail,
				Detail: fmt.Sprintf("%s still has generated fail-closed stubs (%q); the %s service refuses every request until they are implemented; see docs/SERVICES.zh-CN.md", rel, collaboratorUnconfiguredMarker, name)})
			continue
		}
		items = append(items, CheckItem{Name: "collaborators:" + name, Status: StatusOK, Detail: rel + " has no generated fail-closed stubs left"})
	}
	return items
}

func checkWorkflow(root string, manifest Manifest, workflow string) ([]CheckItem, error) {
	switch workflow {
	case "first-business":
		return checkFirstBusinessWorkflow(root, manifest)
	case "player-tcp":
		return checkPlayerTCPWorkflow(root, manifest)
	default:
		return nil, fmt.Errorf("unknown doctor workflow %q; supported: first-business, player-tcp", workflow)
	}
}

// PrintNextStep turns the full doctor checklist into one safe action. It is
// intentionally non-interactive and returns success while a workflow is
// incomplete, so beginners can run it after every edit without interpreting a
// wall of failures or accidentally automating application-owned decisions.
func PrintNextStep(root, requestedWorkflow string, stdout io.Writer) error {
	manifest, err := LoadManifest(root)
	if err != nil {
		return err
	}
	workflow := strings.TrimSpace(requestedWorkflow)
	if workflow == "" {
		workflow = "first-business"
		items, checkErr := checkWorkflow(root, manifest, workflow)
		if checkErr != nil {
			return checkErr
		}
		if workflowComplete(items) && contains(manifest.Access["player"].Transports, "tcp") {
			workflow = "player-tcp"
		}
	}
	items, err := checkWorkflow(root, manifest, workflow)
	if err != nil {
		return err
	}
	done := 0
	for _, item := range items {
		if item.Status == StatusOK {
			done++
		}
	}
	fmt.Fprintf(stdout, "workflow: %s\n", workflow)
	fmt.Fprintf(stdout, "progress: %d/%d\n", done, len(items))
	for _, item := range items {
		if item.Status != StatusFail {
			continue
		}
		detail, fix := splitWorkflowFix(item.Detail)
		fmt.Fprintf(stdout, "next: %s\n", fix)
		fmt.Fprintf(stdout, "why: missing %s\n", detail)
		fmt.Fprintf(stdout, "check again: roost project next --workflow %s\n", workflow)
		fmt.Fprintf(stdout, "full check: roost project doctor --workflow %s\n", workflow)
		fmt.Fprintf(stdout, "guide: %s\n", workflowGuide(workflow))
		return nil
	}
	fmt.Fprintln(stdout, "status: complete")
	for _, item := range checkFrameworkCollaborators(root, manifest) {
		if item.Status == StatusFail {
			service := strings.TrimPrefix(item.Name, "collaborators:")
			fmt.Fprintf(stdout, "next: implement the collaborators of %s in internal/service/%s/collaborators.go\n", service, service)
			fmt.Fprintf(stdout, "why: %s\n", item.Detail)
		}
	}
	if workflow == "first-business" {
		fmt.Fprintln(stdout, "next: roost project next --workflow player-tcp")
		fmt.Fprintln(stdout, "note: player-tcp is optional when another gateway owns the network connection")
	} else {
		fmt.Fprintln(stdout, "next: make ci")
		fmt.Fprintln(stdout, "next: follow docs/DEPLOYMENT.zh-CN.md for release")
		// Past the required chain, the framework has more the project has not
		// used; list it as suggestions, most valuable first.
		for _, hint := range optionalNextHints(root, manifest) {
			fmt.Fprintf(stdout, "optional: %s\n", hint.Command)
			fmt.Fprintf(stdout, "why: %s\n", hint.Why)
		}
	}
	fmt.Fprintf(stdout, "guide: %s\n", workflowGuide(workflow))
	return nil
}

func workflowComplete(items []CheckItem) bool {
	for _, item := range items {
		if item.Status == StatusFail {
			return false
		}
	}
	return true
}

func splitWorkflowFix(detail string) (string, string) {
	const marker = "; fix: "
	parts := strings.SplitN(detail, marker, 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return detail, "review the workflow guide"
}

func workflowGuide(workflow string) string {
	if workflow == "player-tcp" {
		return "docs/PLAYER_ACCESS_TCP.zh-CN.md"
	}
	return "docs/BEGINNER_WORKBOOK.zh-CN.md"
}

func checkFirstBusinessWorkflow(root string, manifest Manifest) ([]CheckItem, error) {
	items := make([]CheckItem, 0, 9)
	add := func(name string, ok bool, detail, fix string) {
		status := StatusOK
		if !ok {
			status = StatusFail
			detail += "; fix: " + fix
		}
		items = append(items, CheckItem{Name: "workflow:" + name, Status: status, Detail: detail})
	}

	services := sortedServiceNames(manifest)
	service := "game"
	if len(services) > 0 {
		service = services[0]
	}
	access, accessOK := manifest.Access["player"]
	add("player-access", accessOK, "access.player is configured", "roost add access player --service "+service)
	nestOK := false
	if accessOK {
		if service, exists := manifest.Services[access.Service]; exists {
			resolved, err := resolveMods(service.Mods)
			nestOK = err == nil && contains(resolved, "nest")
		}
	}
	add("nest-runtime", nestOK, "the player access service owns Nest", "roost add mod nest --service "+service)

	names, err := firstBusinessNames(root, manifest)
	if err != nil {
		return nil, err
	}
	checks := []struct {
		name, pattern, needle, detail, fix string
	}{
		{"entity", "game/entities/*/entity.go", "//roost:entity", "an Entity aggregate exists", "roost add entity " + names.entity},
		{"component", "game/entities/*/*_component.go", "//roost:component", "an Entity Component exists", "roost add component Profile --entity " + names.entity + exampleNameNote},
		{"dao", "db/def/*.go", "//roost:dao", "a DAO definition exists", "roost add dao " + names.entity + " --entity " + names.entity},
		{"nest-handler", "game/handler/*.go", "//roost:nest", "a Nest business handler exists", "roost add handler RenamePlayer --entity " + names.entity + " --component " + names.component + exampleNameNote},
		{"protocol", "protocol/def/*.go", "handler=", "a protocol is assigned to a controller", "roost add protocol " + names.protocol + " --group " + names.group + " --handler " + names.domain + names.protocolNote},
		{"endpoint", "game/controllers/*/*.go", "generated Sender", "a protocol endpoint calls a generated Sender", "roost add endpoint " + names.endpoint + " --handler " + names.endpointDomain + names.endpointNote},
		{"lifecycle", "game/lifecycle/*.go", "FromRegistry", "an explicit Entity lifecycle boundary exists", "roost add lifecycle " + names.entity},
	}
	for _, check := range checks {
		found, err := anyFileContains(root, check.pattern, check.needle)
		if check.name == "endpoint" {
			found, err = anyConnectedEndpoint(root)
		}
		if err != nil {
			return nil, err
		}
		add(check.name, found, check.detail, check.fix)
		if check.name == "dao" {
			implemented, fieldErr := anyDAOHasBusinessField(root)
			if fieldErr != nil {
				return nil, fieldErr
			}
			add("dao-business-field", implemented, "a DAO declares application state", "add a persisted field in db/def/<entity>.go; see docs/BEGINNER_WORKBOOK.zh-CN.md section 3.2, then run roost generate")

			implemented, methodErr := anyComponentBusinessMethod(root)
			if methodErr != nil {
				return nil, methodErr
			}
			add("component-business", implemented, "a Component contains an application business method", "implement a method in game/entities/<entity>/<component>_component.go; see docs/BEGINNER_WORKBOOK.zh-CN.md section 3.3")
		}
		if check.name == "nest-handler" {
			implemented, handlerErr := anyNestHandlerImplemented(root)
			if handlerErr != nil {
				return nil, handlerErr
			}
			add("nest-handler-business", implemented, "a Nest handler calls application business logic", "replace the TODO handler body and add named request parameters; see docs/BEGINNER_WORKBOOK.zh-CN.md section 3.4")
		}
		if check.name == "protocol" {
			implemented, protocolErr := anyPlayerProtocolRequestField(root)
			if protocolErr != nil {
				return nil, protocolErr
			}
			add("protocol-request", implemented, "a player Request declares typed input fields", "add Request fields matching the Nest handler parameters; see docs/BEGINNER_WORKBOOK.zh-CN.md section 3.5")
		}
	}
	// 首登时 Entity 还不存在：没有登录边界先 GetOrCreate，第一条玩法请求就会收到 entity_not_found。
	loginOK, err := anyLoginEndpoint(root, manifest)
	if err != nil {
		return nil, err
	}
	add("login", loginOK, "a login endpoint creates or loads the player's Entity (lifecycle GetOrCreate)",
		"roost add protocol EnterGame --group "+names.group+" --handler "+names.domain+", then roost add endpoint EnterGame --handler "+names.domain+" --lifecycle "+names.lifecycle)
	bootstrapOK, err := anyFileContains(root, "game/protocol_bootstrap/*.go", "RegisterPlayerProtocols")
	if err != nil {
		return nil, err
	}
	add("protocol-bootstrap", bootstrapOK, "protocol registration is generated", "roost generate")
	return items, nil
}

// exampleNameNote 标出修复命令里只是示例的名字：照抄能用，但新手应换成自己业务的名字。
const exampleNameNote = "  # example name, use your own"

// workflowNames 是 first-business 修复命令里的名字。按工程现状取：已有的 Entity、Component、controller 域、
// 缺协议的 Nest handler、缺 endpoint 的协议；工程里还没有时才用示例名（Player / Profile / RenamePlayer / player），
// 示例名后面带 exampleNameNote。照抄示例会多出与业务无关的协议或 controller 域（新用户试用第三轮）。
type workflowNames struct {
	entity, component, group, domain, lifecycle string
	protocol, protocolNote                      string
	endpoint, endpointDomain, endpointNote      string
}

func firstBusinessNames(root string, manifest Manifest) (workflowNames, error) {
	names := workflowNames{entity: "Player", component: "Profile", group: "game", domain: "player",
		protocol: "RenamePlayer", protocolNote: exampleNameNote, endpoint: "RenamePlayer", endpointNote: exampleNameNote}
	if groups := manifest.IDs["protocol"].Groups; len(groups) > 0 {
		if _, ok := groups["game"]; !ok {
			names.group = slices.Sorted(maps.Keys(groups))[0]
		}
	}
	// Entity 与它的第一个 Component：优先 player（玩家接入与 self_sync 认它），否则按目录名第一个。
	entityDirs, err := filepath.Glob(filepath.Join(root, "game", "entities", "*", "entity.go"))
	if err != nil {
		return names, err
	}
	entityDir := ""
	for _, path := range entityDirs {
		dir := filepath.Base(filepath.Dir(path))
		if entityDir == "" || dir == "player" {
			entityDir = dir
		}
	}
	if entityDir != "" {
		names.entity = toPascal(entityDir)
		components, err := filepath.Glob(filepath.Join(root, "game", "entities", entityDir, "*_component.go"))
		if err != nil {
			return names, err
		}
		if len(components) > 0 {
			names.component = toPascal(strings.TrimSuffix(filepath.Base(components[0]), "_component.go"))
		}
	}
	// 登录用的 lifecycle：有这个 Entity 的就用它，否则用已有的第一个，都没有时与 Entity 同名（还要先 add lifecycle）。
	names.lifecycle = names.entity
	lifecycles, err := filepath.Glob(filepath.Join(root, "game", "lifecycle", "*.go"))
	if err != nil {
		return names, err
	}
	var existing []string
	for _, path := range lifecycles {
		base := strings.TrimSuffix(filepath.Base(path), ".go")
		if !strings.HasSuffix(base, "_gen") && !strings.HasSuffix(base, "_test") && !strings.HasSuffix(base, "_singleton") {
			existing = append(existing, base)
		}
	}
	if len(existing) > 0 && !slices.Contains(existing, toSnake(names.entity)) {
		names.lifecycle = toPascal(existing[0])
	}
	// 协议文件（protocol/def/<snake>.go）与它声明的 controller 域。
	protocolPaths, err := filepath.Glob(filepath.Join(root, "protocol", "def", "*.go"))
	if err != nil {
		return names, err
	}
	protocolDomain := map[string]string{}
	var protocols []string
	for _, path := range protocolPaths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return names, err
		}
		match := protocolHandlerMarker.FindSubmatch(raw)
		if match == nil {
			continue
		}
		snake := strings.TrimSuffix(filepath.Base(path), ".go")
		protocolDomain[snake] = string(match[1])
		protocols = append(protocols, snake)
	}
	if len(protocols) > 0 {
		names.domain = protocolDomain[protocols[0]]
	} else if controllers, err := filepath.Glob(filepath.Join(root, "game", "controllers", "*")); err != nil {
		return names, err
	} else {
		for _, path := range controllers {
			if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
				names.domain = filepath.Base(path)
				break
			}
		}
	}
	names.endpointDomain = names.domain
	// Nest handler（game/handler/<snake>.go 带 //roost:nest）：协议提示用第一个还没有同名协议的 handler。
	handlerPaths, err := filepath.Glob(filepath.Join(root, "game", "handler", "*.go"))
	if err != nil {
		return names, err
	}
	handlers := map[string]bool{}
	for _, path := range handlerPaths {
		base := strings.TrimSuffix(filepath.Base(path), ".go")
		if strings.HasSuffix(base, "_gen") || strings.HasSuffix(base, "_test") {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return names, err
		}
		if !bytes.Contains(raw, []byte("//roost:nest")) {
			continue
		}
		handlers[base] = true
		if _, hasProtocol := protocolDomain[base]; !hasProtocol && names.protocolNote != "" {
			names.protocol, names.protocolNote = toPascal(base), ""
		}
	}
	// endpoint 提示：第一个与 Nest handler 同名、还没有 controller 文件的协议，沿用它声明的域。
	for _, snake := range protocols {
		if !handlers[snake] {
			continue
		}
		domain := protocolDomain[snake]
		if _, err := os.Stat(filepath.Join(root, "game", "controllers", domain, snake+".go")); os.IsNotExist(err) {
			names.endpoint, names.endpointDomain, names.endpointNote = toPascal(snake), domain, ""
			break
		} else if err != nil {
			return names, err
		}
	}
	return names, nil
}

// protocolHandlerMarker 取 //roost:protocol 标记的 handler=（controller 域）。
var protocolHandlerMarker = regexp.MustCompile(`//roost:protocol\b[^\n]*\bhandler=([A-Za-z0-9_]+)`)

func anyComponentBusinessMethod(root string) (bool, error) {
	paths, err := filepath.Glob(filepath.Join(root, "game", "entities", "*", "*_component.go"))
	if err != nil {
		return false, err
	}
	frameworkMethods := map[string]bool{"Type": true, "Name": true, "Init": true, "Destroy": true, "Owner": true}
	for _, path := range paths {
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			return false, parseErr
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv == nil || frameworkMethods[function.Name.Name] {
				continue
			}
			if len(function.Recv.List) != 1 {
				continue
			}
			receiver := function.Recv.List[0].Type
			if pointer, ok := receiver.(*ast.StarExpr); ok {
				receiver = pointer.X
			}
			identifier, ok := receiver.(*ast.Ident)
			if ok && strings.HasSuffix(identifier.Name, "Component") {
				return true, nil
			}
		}
	}
	return false, nil
}

func anyDAOHasBusinessField(root string) (bool, error) {
	paths, err := filepath.Glob(filepath.Join(root, "db", "def", "*.go"))
	if err != nil {
		return false, err
	}
	for _, path := range paths {
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return false, readErr
		}
		if !marker.Has(string(raw), "dao") {
			continue
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, raw, 0)
		if parseErr != nil {
			return false, parseErr
		}
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.TYPE {
				continue
			}
			for _, specification := range general.Specs {
				typeSpec, ok := specification.(*ast.TypeSpec)
				if !ok {
					continue
				}
				structure, ok := typeSpec.Type.(*ast.StructType)
				if !ok || structure.Fields == nil {
					continue
				}
				for _, field := range structure.Fields.List {
					if len(field.Names) > 0 && field.Tag != nil && strings.Contains(field.Tag.Value, "dao:") {
						return true, nil
					}
				}
			}
		}
	}
	return false, nil
}

func anyNestHandlerImplemented(root string) (bool, error) {
	paths, err := filepath.Glob(filepath.Join(root, "game", "handler", "*.go"))
	if err != nil {
		return false, err
	}
	for _, path := range paths {
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return false, readErr
		}
		if !marker.Has(string(raw), "nest") {
			continue
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, raw, 0)
		if parseErr != nil {
			return false, parseErr
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || !strings.HasPrefix(function.Name.Name, "handler") || function.Body == nil {
				continue
			}
			if handlerCallsOwnedComponent(function) {
				return true, nil
			}
		}
	}
	return false, nil
}

func handlerCallsOwnedComponent(function *ast.FuncDecl) bool {
	if function.Type.Params == nil || len(function.Type.Params.List) == 0 || len(function.Type.Params.List[0].Names) != 1 {
		return false
	}
	target := function.Type.Params.List[0].Names[0].Name
	found := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		method, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		componentCall, ok := method.X.(*ast.CallExpr)
		if !ok {
			return true
		}
		componentGetter, ok := componentCall.Fun.(*ast.SelectorExpr)
		if !ok || !strings.HasSuffix(componentGetter.Sel.Name, "Comp") {
			return true
		}
		owner, ok := componentGetter.X.(*ast.Ident)
		if ok && owner.Name == target {
			found = true
		}
		return !found
	})
	return found
}

func anyPlayerProtocolRequestField(root string) (bool, error) {
	handlerPaths, err := filepath.Glob(filepath.Join(root, "game", "handler", "*.go"))
	if err != nil {
		return false, err
	}
	for _, handlerPath := range handlerPaths {
		if strings.HasSuffix(handlerPath, "_gen.go") {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(handlerPath), ".go")
		matched, matchErr := workflowHandlerProtocolMatch(root, name)
		if matchErr != nil {
			return false, matchErr
		}
		if matched {
			return true, nil
		}
	}
	return false, nil
}

func workflowHandlerProtocolMatch(root, name string) (bool, error) {
	handlerPath := filepath.Join(root, "game", "handler", name+".go")
	file, err := parser.ParseFile(token.NewFileSet(), handlerPath, nil, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	functionName := "handler" + toPascal(name)
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != functionName || !handlerCallsOwnedComponent(function) || function.Type.Params == nil {
			continue
		}
		parameters := make([]string, 0)
		for index, parameter := range function.Type.Params.List {
			if index == 0 {
				continue
			}
			for _, parameterName := range parameter.Names {
				parameters = append(parameters, toSnake(parameterName.Name))
			}
		}
		if len(parameters) == 0 {
			return false, nil
		}
		fields, fieldErr := protocolFieldNames(filepath.Join(root, "protocol", "def", name+".go"), toPascal(name)+"Request")
		if fieldErr != nil {
			if os.IsNotExist(fieldErr) {
				return false, nil
			}
			return false, fieldErr
		}
		for _, parameter := range parameters {
			if _, exists := fields[parameter]; !exists {
				return false, nil
			}
		}
		return true, nil
	}
	return false, nil
}

// anyLoginEndpoint 报告是否有 controller 文件导入工程的 game/lifecycle 并调用某个 GetOrCreate：
// 这就是“登录时保证 Entity 存在”的那一步（roost add endpoint --lifecycle 生成的，或业务手写的）。
func anyLoginEndpoint(root string, manifest Manifest) (bool, error) {
	paths, err := filepath.Glob(filepath.Join(root, "game", "controllers", "*", "*.go"))
	if err != nil {
		return false, err
	}
	lifecycleImport := strconv.Quote(manifest.Project.Module + "/game/lifecycle")
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			return false, parseErr
		}
		imported := false
		for _, spec := range file.Imports {
			imported = imported || spec.Path.Value == lifecycleImport
		}
		if !imported {
			continue
		}
		found := false
		ast.Inspect(file, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "GetOrCreate" {
					found = true
				}
			}
			return !found
		})
		if found {
			return true, nil
		}
	}
	return false, nil
}

func anyConnectedEndpoint(root string) (bool, error) {
	paths, err := filepath.Glob(filepath.Join(root, "game", "controllers", "*", "*.go"))
	if err != nil {
		return false, err
	}
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".go")
		if name == "controller" {
			continue
		}
		matched, matchErr := workflowHandlerProtocolMatch(root, name)
		if matchErr != nil {
			return false, matchErr
		}
		if !matched {
			continue
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return false, readErr
		}
		if bytes.Contains(raw, []byte("New"+toPascal(name)+"Sender")) {
			return true, nil
		}
	}
	return false, nil
}

func checkPlayerTCPWorkflow(root string, manifest Manifest) ([]CheckItem, error) {
	items := make([]CheckItem, 0, 5)
	add := func(name string, ok bool, detail, fix string) {
		status := StatusOK
		if !ok {
			status = StatusFail
			detail += "; fix: " + fix
		}
		items = append(items, CheckItem{Name: "workflow:" + name, Status: status, Detail: detail})
	}
	services := sortedServiceNames(manifest)
	service := "game"
	if len(services) > 0 {
		service = services[0]
	}
	access, accessOK := manifest.Access["player"]
	add("player-access", accessOK, "access.player is configured", "roost add access player --service "+service)
	transportOK := accessOK && contains(access.Transports, "tcp")
	add("tcp-transport", transportOK, "the TCP transport is declared", "roost add transport tcp")
	serverOK, err := anyFileContains(root, "internal/access/player/tcp/server_gen.go", "gateway.NewTCPServer(")
	if err != nil {
		return nil, err
	}
	add("tcp-runtime", serverOK, "the generated TCP adapter connects the shared runtime", "roost project sync")

	authPath := filepath.Join(root, "internal", "access", "player", "tcp", "auth.go")
	authRaw, authErr := os.ReadFile(authPath)
	authOK := authErr == nil && playerTCPAuthenticatorImplemented(authRaw)
	if authErr != nil && !os.IsNotExist(authErr) {
		return nil, authErr
	}
	add("tcp-auth", authOK, "the application authenticator consumes the presented token", "implement and security-review internal/access/player/tcp/auth.go")

	configOK := false
	if accessOK {
		configPath := filepath.Join(root, "configs", "service", "config."+access.Service+".yaml")
		raw, readErr := os.ReadFile(configPath)
		if readErr != nil && !os.IsNotExist(readErr) {
			return nil, readErr
		}
		if readErr == nil {
			var config map[string]any
			if yaml.Unmarshal(raw, &config) == nil {
				playerAccess, _ := config["player_access"].(map[string]any)
				tcpConfig, _ := playerAccess["tcp"].(map[string]any)
				enabled, _ := tcpConfig["enabled"].(bool)
				configOK = enabled && strings.TrimSpace(fmt.Sprint(tcpConfig["addr"])) != "" &&
					positiveConfigNumber(tcpConfig["max_connections"]) &&
					positiveConfigNumber(tcpConfig["max_connections_per_ip"]) &&
					positiveConfigNumber(tcpConfig["max_handshakes"]) &&
					positiveConfigNumber(tcpConfig["max_handshake_bytes"]) &&
					positiveConfigNumber(tcpConfig["max_payload_bytes"])
			}
		}
	}
	add("tcp-config", configOK, "the owning service enables bounded TCP access", "roost config enable player-tcp")
	return items, nil
}

func playerTCPAuthenticatorImplemented(raw []byte) bool {
	file, err := parser.ParseFile(token.NewFileSet(), "auth.go", raw, 0)
	if err != nil {
		return false
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "Authenticate" || function.Recv == nil || function.Body == nil {
			continue
		}
		tokenName := ""
		if function.Type.Params != nil {
			for _, parameter := range function.Type.Params.List {
				identifier, isString := parameter.Type.(*ast.Ident)
				if !isString || identifier.Name != "string" || len(parameter.Names) != 1 || parameter.Names[0].Name == "_" {
					continue
				}
				tokenName = parameter.Names[0].Name
			}
		}
		if tokenName == "" {
			return false
		}
		usesToken := false
		ast.Inspect(function.Body, func(node ast.Node) bool {
			identifier, ok := node.(*ast.Ident)
			if ok && identifier.Name == tokenName {
				usesToken = true
			}
			return true
		})
		if !usesToken {
			return false
		}
		if len(function.Body.List) != 1 {
			return true
		}
		result, ok := function.Body.List[0].(*ast.ReturnStmt)
		if !ok || len(result.Results) != 2 {
			return true
		}
		selector, ok := result.Results[1].(*ast.SelectorExpr)
		if ok && selector.Sel.Name == "ErrUnauthenticated" {
			return false
		}
		return true
	}
	return false
}

func positiveConfigNumber(value any) bool {
	switch number := value.(type) {
	case int:
		return number > 0
	case int64:
		return number > 0
	case uint64:
		return number > 0
	case float64:
		return number > 0
	default:
		return false
	}
}

func anyFileContains(root, pattern, needle string) (bool, error) {
	paths, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
	if err != nil {
		return false, err
	}
	for _, path := range paths {
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return false, readErr
		}
		if bytes.Contains(raw, []byte(needle)) {
			return true, nil
		}
	}
	return false, nil
}

func CheckConfig(path string, production bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var value map[string]any
	if err := yaml.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("invalid yaml: %w", err)
	}
	if value["sid"] == nil {
		return errors.New("sid is required")
	}
	if production {
		var document yaml.Node
		if err := yaml.Unmarshal(raw, &document); err != nil {
			return fmt.Errorf("invalid yaml: %w", err)
		}
		if forbidden, value := findForbiddenProductionScalar(&document); forbidden != "" {
			return fmt.Errorf("production config contains forbidden value %q in scalar %q", forbidden, value)
		}
	}
	return nil
}

func findForbiddenProductionScalar(node *yaml.Node) (string, string) {
	return findForbiddenProductionAt(node, "")
}
func findForbiddenProductionAt(node *yaml.Node, path string) (string, string) {
	if node == nil {
		return "", ""
	}
	if node.Kind == yaml.ScalarNode && (node.Tag == "!!str" || node.Tag == "") {
		value := strings.ToLower(strings.TrimSpace(node.Value))
		for _, forbidden := range []string{"change_me", "127.0.0.1", "localhost", "dev-"} {
			if path == "ops.addr" && (forbidden == "127.0.0.1" || forbidden == "localhost") {
				continue
			}
			if strings.Contains(value, forbidden) {
				return forbidden, node.Value
			}
		}
	}
	for index, child := range node.Content {
		if node.Kind == yaml.MappingNode && index%2 == 0 {
			continue
		}
		next := path
		if node.Kind == yaml.MappingNode {
			if next != "" {
				next += "."
			}
			next += node.Content[index-1].Value
		}
		if forbidden, value := findForbiddenProductionAt(child, next); forbidden != "" {
			return forbidden, value
		}
	}
	return "", ""
}

func DiffProject(root string, stdout io.Writer) error {
	m, err := LoadManifest(root)
	if err != nil {
		return err
	}
	return diffManifest(root, m, false, stdout)
}

// diffManifest previews what SyncProject would write for manifest m: the
// codegen-owned templates, and the generated shutdown: blocks in the
// application-owned configs that sync refreshes before it renders
// Generator outputs are not previewed; roost generate
// --check reports those.
//
// the preview used to render from the configs on disk and
// skip every existing application-owned file, so a Mod change (or an upgrade
// across a stop-budget formula change) left out the three configs sync was
// about to rewrite. The refresh now runs on copies of just those configs in a
// throwaway directory, and the templates render from the refreshed totals,
// exactly as SyncProject's staging tree does.
func diffManifest(root string, m Manifest, includeManifest bool, stdout io.Writer) error {
	shutdownStage, err := os.MkdirTemp("", "roost-diff-shutdown-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(shutdownStage)
	for _, service := range sortedServiceNames(m) {
		for _, target := range shutdownConfigTargets(service) {
			raw, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(target.rel)))
			if os.IsNotExist(readErr) {
				continue
			}
			if readErr != nil {
				return readErr
			}
			if err := writeAtomic(filepath.Join(shutdownStage, filepath.FromSlash(target.rel)), raw, 0o644); err != nil {
				return err
			}
		}
	}
	refreshed, err := refreshGeneratedShutdownConfigs(shutdownStage, m)
	if err != nil {
		return fmt.Errorf("preview shutdown.total_timeout refresh: %w", err)
	}
	plan, err := renderProject(m.withConfiguredShutdown(shutdownStage).withServicePackages(root))
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(plan))
	for path := range plan {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	manifestChanged := false
	changed := make([]string, 0, len(paths)+len(refreshed))
	if includeManifest {
		prospective, marshalErr := m.Marshal()
		if marshalErr != nil {
			return marshalErr
		}
		current, readErr := os.ReadFile(filepath.Join(root, ManifestName))
		if readErr != nil && !os.IsNotExist(readErr) {
			return readErr
		}
		manifestChanged = readErr != nil || !bytes.Equal(current, prospective)
	}
	for _, path := range paths {
		file := plan[path]
		current, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if readErr == nil && !file.Owned {
			continue
		}
		if readErr == nil && file.Owned && !canOverwriteGenerated(path, current) {
			return fmt.Errorf("refusing to overwrite non-generated file %s", path)
		}
		if readErr != nil && !os.IsNotExist(readErr) {
			return readErr
		}
		if readErr != nil || string(current) != string(file.Body) {
			changed = append(changed, path)
		}
	}
	for _, rel := range refreshed {
		if !slices.Contains(changed, rel) {
			changed = append(changed, rel)
		}
	}
	sort.Strings(changed)
	if manifestChanged {
		changed = append([]string{ManifestName}, changed...)
	}
	for _, path := range changed {
		fmt.Fprintln(stdout, path)
	}
	fmt.Fprintf(stdout, "summary: %d file(s) would change\n", len(changed))
	return nil
}
