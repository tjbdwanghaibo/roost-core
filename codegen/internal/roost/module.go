package roost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
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
	"strings"
)

// runModule 是 `roost module <list|plan|add|verify>`。
func runModule(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	const usage = "usage: roost module <list|plan|add|verify> [module ...] [--json] [--service name] [--no-self-sync] [--root dir]"
	if len(args) == 0 {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet("module "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	rootFlag := fs.String("root", ".", "project directory")
	service := fs.String("service", "", "business service to install into (when several run Nest)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	noSelfSync := fs.Bool("no-self-sync", false, "do not turn on access.player.self_sync for modules that ask for it (the project runs its own entitysync Manager)")
	names, flags := splitModuleArgs(args[1:])
	if err := fs.Parse(flags); err != nil {
		return err
	}
	names = append(names, fs.Args()...)
	switch args[0] {
	case "list":
		return listModules(*rootFlag, *asJSON, stdout)
	case "plan", "add":
		root, err := projectRoot(*rootFlag)
		if err != nil {
			return err
		}
		options := ModuleOptions{Service: *service, Stderr: stderr, SkipSelfSync: *noSelfSync}
		var plan ModulePlan
		if args[0] == "plan" {
			plan, err = PlanModules(root, names, options)
		} else {
			// 生成器的逐文件输出对这条命令是噪声：成功时只报模块层面的结果，失败时错误里带原因。
			plan, err = AddModules(ctx, root, names, options)
		}
		if err != nil {
			return err
		}
		if *asJSON {
			return writeJSON(stdout, plan)
		}
		printModulePlan(stdout, plan)
		return nil
	case "verify":
		root, err := projectRoot(*rootFlag)
		if err != nil {
			return err
		}
		report, err := VerifyModules(ctx, root, names, stderr)
		if *asJSON {
			if writeErr := writeJSON(stdout, report); writeErr != nil {
				return writeErr
			}
		} else {
			printModuleVerify(stdout, report)
		}
		return err
	default:
		return errors.New(usage)
	}
}

// splitModuleArgs 把模块名与旗标分开：flag 包遇到第一个非旗标参数就停，而模块名习惯写在前面。
func splitModuleArgs(args []string) (names, flags []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			names = append(names, arg)
			continue
		}
		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if (name == "root" || name == "service") && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return names, flags
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// moduleListing 是 `roost module list --json` 的一项。
type moduleListing struct {
	ModuleSpec
	// Installed 在工程目录里执行时给出：true 表示 roost.yaml 里有这个模块的记录。
	Installed bool `json:"installed"`
}

// listModules 列出内置模块；在工程目录里执行时标出已安装的。不在工程里也能用（agent 先看目录再建工程）。
func listModules(rootFlag string, asJSON bool, stdout io.Writer) error {
	catalog, err := loadModuleCatalog()
	if err != nil {
		return err
	}
	installed := map[string]InstalledModule{}
	if root, err := projectRoot(rootFlag); err == nil {
		if manifest, err := LoadManifest(root); err == nil {
			installed = manifest.Modules
		}
	}
	listings := make([]moduleListing, 0, len(catalog))
	for _, name := range sortedModuleNames(catalog) {
		_, has := installed[name]
		listings = append(listings, moduleListing{ModuleSpec: catalog[name], Installed: has})
	}
	if asJSON {
		return writeJSON(stdout, listings)
	}
	for _, listing := range listings {
		mark := " "
		if listing.Installed {
			mark = "*"
		}
		fmt.Fprintf(stdout, "%s %-14s %s\n", mark, listing.Name, listing.Summary)
		if len(listing.Requires) > 0 {
			fmt.Fprintf(stdout, "    requires: %s\n", strings.Join(listing.Requires, ", "))
		}
		if len(listing.State) > 0 {
			var fields []string
			for _, field := range listing.State {
				fields = append(fields, fmt.Sprintf("%sDao.%s (%s)", listing.Entity, field.Field, field.Dao))
			}
			fmt.Fprintf(stdout, "    state: %s\n", strings.Join(fields, ", "))
		}
		for _, dao := range moduleDAONames(listing.ModuleSpec) {
			fmt.Fprintf(stdout, "    dao: %s\n", dao)
		}
		if names := listing.withNames(); len(names) > 0 {
			fmt.Fprintf(stdout, "    with: %s (extra files when that module is installed too)\n", strings.Join(names, ", "))
		}
	}
	fmt.Fprintln(stdout, "\n* = installed in this project.  Preview: roost module plan <name>   Install: roost module add <name>")
	return nil
}

func printModulePlan(stdout io.Writer, plan ModulePlan) {
	verb := "will"
	if plan.Applied {
		verb = "did"
	}
	fmt.Fprintf(stdout, "service: %s\n", plan.Service)
	for _, command := range plan.Prerequisites {
		fmt.Fprintf(stdout, "prerequisite (%s run): %s\n", verb, command)
	}
	declareVerb := "will declare"
	if plan.Applied {
		declareVerb = "declared"
	}
	for _, name := range plan.FrameworkServices {
		fmt.Fprintf(stdout, "framework service %s: %s (services.%s.framework: %s, added to services.%s.uses; its config, deployment and collaborators come with the install)\n", name, declareVerb, name, name, plan.Service)
	}
	for _, item := range plan.Modules {
		switch item.Status {
		case moduleStatusInstalled:
			fmt.Fprintf(stdout, "module %s: already installed, nothing to do\n", item.Name)
			continue
		case moduleStatusInstalledOther:
			fmt.Fprintf(stdout, "module %s: already installed from another version of the module; the installer leaves installed modules alone\n", item.Name)
			continue
		}
		if plan.Applied {
			fmt.Fprintf(stdout, "module %s: installed\n", item.Name)
		} else {
			fmt.Fprintf(stdout, "module %s: will install\n", item.Name)
		}
		if item.Runtime != nil && item.Runtime.Framework != "" {
			fmt.Fprintf(stdout, "  runtime:   %s.%s in the process of the hosted %s service (after its Mods start, before service Init; stopped in reverse)\n", item.Runtime.Package, item.Runtime.Start, item.Runtime.Framework)
		} else if item.Runtime != nil {
			fmt.Fprintf(stdout, "  runtime:   %s.%s (before service Init; stop after service Shutdown)\n", item.Runtime.Package, item.Runtime.Start)
		}
		for _, manager := range item.Managers {
			fmt.Fprintf(stdout, "  manager:   %s() in %s (started and stopped by the manager mod)\n", manager, gameManagersPath)
		}
		for _, host := range slices.Sorted(maps.Keys(item.Services)) {
			for _, manager := range item.Services[host].Managers {
				fmt.Fprintf(stdout, "  manager:   gamemanagers.%s() in internal/service/%s/managers.go (service %s; started and stopped by its manager mod)\n", manager, host, host)
			}
		}
		for _, state := range item.State {
			fmt.Fprintf(stdout, "  state:     %s\n", state)
		}
		for _, dao := range item.DAOs {
			fmt.Fprintf(stdout, "  dao:       %s\n", dao)
		}
		if item.Component != "" {
			fmt.Fprintf(stdout, "  component: %s\n", item.Component)
		}
		if item.Timer != nil {
			action := "reuse"
			if item.Timer.Create {
				action = "create"
			}
			fmt.Fprintf(stdout, "  timer:     %s %s.%s (component id %d)\n", action, item.Timer.Entity, item.Timer.Spec.Component, item.Timer.Spec.ID)
		}
		for _, kind := range moduleIDKinds {
			names := make([]string, 0, len(item.IDs[kind]))
			for name := range item.IDs[kind] {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				fmt.Fprintf(stdout, "  id:        %s %s = %d\n", kind, name, item.IDs[kind][name])
			}
		}
		for _, file := range item.Files {
			if contains(item.Replaces, file) {
				fmt.Fprintf(stdout, "  file:      %s (replaces the untouched generated stub)\n", file)
				continue
			}
			fmt.Fprintf(stdout, "  file:      %s\n", file)
		}
		// 安装后提示：安装器不代办、要工程自己做的事（清单的 next）。plan 与 add 都列出，add 之后它们仍然没做。
		for _, hint := range item.Next {
			fmt.Fprintf(stdout, "  todo:      %s\n", hint)
		}
	}
	for _, glue := range plan.Glue {
		if plan.Applied {
			fmt.Fprintf(stdout, "glue %s with %s: written (%s)\n", glue.Module, glue.With, glue.Summary)
		} else {
			fmt.Fprintf(stdout, "glue %s with %s: will write (%s)\n", glue.Module, glue.With, glue.Summary)
		}
		for _, file := range glue.Files {
			fmt.Fprintf(stdout, "  file:      %s\n", file)
		}
	}
	if plan.Applied {
		fmt.Fprintln(stdout, "next: roost module verify")
	}
}

// ModuleVerifyReport 是 `roost module verify` 的结果。
type ModuleVerifyReport struct {
	Modules []ModuleVerifyResult `json:"modules"`
	// Checks 是工程级检查：build、vet、id check、generate --check。
	Checks []ModuleCheck `json:"checks"`
	OK     bool          `json:"ok"`
}

// ModuleVerifyResult 是一个模块自带场景的结果。
type ModuleVerifyResult struct {
	Name    string `json:"name"`
	Want    int    `json:"want"`
	Passed  int    `json:"passed"`
	Skipped int    `json:"skipped"`
	OK      bool   `json:"ok"`
	Detail  string `json:"detail,omitempty"`
}

// ModuleCheck 是一条工程级检查。
type ModuleCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// 模块的 verify.pass 是场景数：一个顶层测试是一个场景，t.Run 的子测试是同一场景的几种情形、不另算；
// 跳过在任何一层都算（零跳过的要求覆盖子测试）。go test -v 的顶层结果行从第一列开始，子测试逐层缩进。
var (
	modulePassLine = regexp.MustCompile(`(?m)^--- PASS: `)
	moduleSkipLine = regexp.MustCompile(`(?m)^\s*--- SKIP: `)
)

// countModuleScenarios 数 go test -v 输出里通过的顶层场景与任意一层的跳过。
func countModuleScenarios(output []byte) (passed, skipped int) {
	return len(modulePassLine.FindAllIndex(output, -1)), len(moduleSkipLine.FindAllIndex(output, -1))
}

// VerifyModules 跑已装模块（names 为空 = 全部）自带的场景，要求通过数恰好等于清单里的数、零跳过，
// 再跑工程级检查。通过数与跳过数都核对：-run 写错或用例被跳过时 go test 照样报 ok。
// 有任何一项不通过时返回错误，报告里逐项写明。
//
// 只运行、只统计模块装进工程的那些测试文件里的用例（维护者 2026-11-17）：用例名从 roost.yaml 记下的
// 模块文件（联动是 with.<另一个模块> 记下的文件）里读出，再与清单的 run 取交集，拼成精确的 -run '^(A|B|…)$'。
// 工程自己的测试即使与模块场景同前缀（回合 PvP 的 TestTurnPVPRealTimerRecovery 之于 ^TestTurn）也不会被运行或计数。
func VerifyModules(ctx context.Context, root string, names []string, stderr io.Writer) (ModuleVerifyReport, error) {
	catalog, err := loadModuleCatalog()
	if err != nil {
		return ModuleVerifyReport{}, err
	}
	manifest, err := LoadManifest(root)
	if err != nil {
		return ModuleVerifyReport{}, err
	}
	if len(names) == 0 {
		for name := range manifest.Modules {
			names = append(names, name)
		}
		sort.Strings(names)
	}
	if len(names) == 0 {
		return ModuleVerifyReport{}, errors.New("no module is installed in this project; run: roost module add <name>")
	}
	report := ModuleVerifyReport{OK: true, Modules: []ModuleVerifyResult{}, Checks: []ModuleCheck{}}
	for _, name := range names {
		name = toSnake(name)
		spec, known := catalog[name]
		installed, isInstalled := manifest.Modules[name]
		if !isInstalled || !known {
			return ModuleVerifyReport{}, fmt.Errorf("module %q is not installed in this project", name)
		}
		scenarios := []moduleScenario{{name: name, verify: spec.Verify, files: installed.Files}}
		// 已经写进工程的联动各自带场景时，跟在所属模块后面，名字是 <模块>+<另一个模块>。
		others := make([]string, 0, len(installed.With))
		for other := range installed.With {
			others = append(others, other)
		}
		sort.Strings(others)
		for _, other := range others {
			if with, ok := spec.With[other]; ok && with.Verify != nil {
				scenarios = append(scenarios, moduleScenario{name: name + "+" + other, verify: *with.Verify, files: installed.With[other]})
			}
		}
		for _, scenario := range scenarios {
			result := ModuleVerifyResult{Name: scenario.name, Want: scenario.verify.Pass}
			tests, err := moduleScenarioTests(root, scenario.files, scenario.verify.Run)
			if err == nil && len(tests) == 0 {
				err = fmt.Errorf("no test matching %s in the module's installed test files", scenario.verify.Run)
			}
			if err != nil {
				result.Detail = err.Error()
				report.OK = false
				report.Modules = append(report.Modules, result)
				continue
			}
			args := append([]string{"test", "-mod=mod", "-race", "-count=1", "-v", "-run", moduleExactRun(tests)}, scenario.verify.Packages...)
			output, runErr := runModuleTool(ctx, root, "go", args...)
			result.Passed, result.Skipped = countModuleScenarios(output)
			result.OK = runErr == nil && result.Passed == result.Want && result.Skipped == 0
			if !result.OK {
				report.OK = false
				result.Detail = tailLines(output, 30)
			}
			report.Modules = append(report.Modules, result)
		}
	}
	checks := []struct {
		name string
		run  func() ([]byte, error)
	}{
		{"go build ./...", func() ([]byte, error) { return runModuleTool(ctx, root, "go", "build", "-mod=mod", "./...") }},
		{"go vet ./...", func() ([]byte, error) { return runModuleTool(ctx, root, "go", "vet", "-mod=mod", "./...") }},
		{"roost id check", func() ([]byte, error) { return nil, CheckIDs(root, manifest) }},
		{"roost generate --check", func() ([]byte, error) {
			var out bytes.Buffer
			return out.Bytes(), generate(ctx, root, GenerateOptions{Check: true, Stdout: &out})
		}},
	}
	for _, check := range checks {
		output, runErr := check.run()
		item := ModuleCheck{Name: check.name, OK: runErr == nil}
		if runErr != nil {
			report.OK = false
			item.Detail = strings.TrimSpace(tailLines(output, 30) + "\n" + runErr.Error())
		}
		report.Checks = append(report.Checks, item)
	}
	_ = stderr
	if !report.OK {
		return report, errors.New("module verification failed")
	}
	return report, nil
}

// moduleScenario 是 verify 的一项：模块自己的场景，或一组联动的场景。files 是它装进工程的文件（roost.yaml 的记录）。
type moduleScenario struct {
	name   string
	verify ModuleVerify
	files  []string
}

// moduleScenarioTests 读出 files 里的测试文件（_test.go，按当前 GOOS / GOARCH 与构建约束会编译的那些；
// 带 integration 一类标签的真实依赖用例不在其中）中名字匹配 run 的顶层测试函数，按名字排序。
// 文件不在工程里（被删了）是错误：模块场景少了，条数也就对不上。
func moduleScenarioTests(root string, files []string, run string) ([]string, error) {
	pattern, err := regexp.Compile(run)
	if err != nil {
		return nil, fmt.Errorf("verify.run %q: %w", run, err)
	}
	seen := map[string]bool{}
	for _, rel := range files {
		if !strings.HasSuffix(rel, "_test.go") {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(rel))
		match, err := build.Default.MatchFile(filepath.Dir(path), filepath.Base(path))
		if err != nil {
			return nil, fmt.Errorf("module test file %s: %w", rel, err)
		}
		if !match {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("module test file %s: %w", rel, err)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && isGoTestFunc(fn) && pattern.MatchString(fn.Name.Name) {
				seen[fn.Name.Name] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(seen)), nil
}

// isGoTestFunc 与 go test 认测试函数的规则相同：顶层函数 TestXxx(t *testing.T)，Test 之后不是小写字母。
func isGoTestFunc(fn *ast.FuncDecl) bool {
	name := fn.Name.Name
	if fn.Recv != nil || !strings.HasPrefix(name, "Test") || fn.Type.Params == nil || len(fn.Type.Params.List) != 1 {
		return false
	}
	if rest := name[len("Test"):]; rest != "" && rest[0] >= 'a' && rest[0] <= 'z' {
		return false
	}
	param := fn.Type.Params.List[0]
	star, ok := param.Type.(*ast.StarExpr)
	if !ok || len(param.Names) > 1 {
		return false
	}
	selector, ok := star.X.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == "T"
}

// moduleExactRun 把用例名拼成只匹配这些名字的 -run：^(A|B|…)$。
func moduleExactRun(tests []string) string {
	quoted := make([]string, len(tests))
	for i, test := range tests {
		quoted[i] = regexp.QuoteMeta(test)
	}
	return "^(" + strings.Join(quoted, "|") + ")$"
}

// runModuleTool 在工程根目录里执行一条工具命令；GOWORK=off 让结果不受外层 go.work 影响。
func runModuleTool(ctx context.Context, root, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = root
	command.Env = append(os.Environ(), "GOWORK=off")
	return command.CombinedOutput()
}

func tailLines(output []byte, count int) string {
	lines := strings.Split(strings.TrimRight(string(output), "\n"), "\n")
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	return strings.Join(lines, "\n")
}

func printModuleVerify(stdout io.Writer, report ModuleVerifyReport) {
	for _, result := range report.Modules {
		status := "ok"
		if !result.OK {
			status = "FAIL"
		}
		fmt.Fprintf(stdout, "%-4s module %s: %d passed (want %d), %d skipped\n", status, result.Name, result.Passed, result.Want, result.Skipped)
		if result.Detail != "" {
			fmt.Fprintln(stdout, result.Detail)
		}
	}
	for _, check := range report.Checks {
		status := "ok"
		if !check.OK {
			status = "FAIL"
		}
		fmt.Fprintf(stdout, "%-4s %s\n", status, check.Name)
		if check.Detail != "" {
			fmt.Fprintln(stdout, check.Detail)
		}
	}
}
