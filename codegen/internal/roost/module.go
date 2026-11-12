package roost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
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
		if item.Runtime != nil {
			fmt.Fprintf(stdout, "  runtime:   %s.%s (before service Init; stop after service Shutdown)\n", item.Runtime.Package, item.Runtime.Start)
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

var (
	modulePassLine = regexp.MustCompile(`(?m)^\s*--- PASS: `)
	moduleSkipLine = regexp.MustCompile(`(?m)^\s*--- SKIP: `)
)

// VerifyModules 跑已装模块（names 为空 = 全部）自带的场景，要求通过数恰好等于清单里的数、零跳过，
// 再跑工程级检查。通过数与跳过数都核对：-run 写错或用例被跳过时 go test 照样报 ok。
// 有任何一项不通过时返回错误，报告里逐项写明。
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
		if _, installed := manifest.Modules[name]; !installed || !known {
			return ModuleVerifyReport{}, fmt.Errorf("module %q is not installed in this project", name)
		}
		scenarios := []struct {
			name   string
			verify ModuleVerify
		}{{name, spec.Verify}}
		// 已经写进工程的联动各自带场景时，跟在所属模块后面，名字是 <模块>+<另一个模块>。
		others := make([]string, 0, len(manifest.Modules[name].With))
		for other := range manifest.Modules[name].With {
			others = append(others, other)
		}
		sort.Strings(others)
		for _, other := range others {
			if with, ok := spec.With[other]; ok && with.Verify != nil {
				scenarios = append(scenarios, struct {
					name   string
					verify ModuleVerify
				}{name + "+" + other, *with.Verify})
			}
		}
		for _, scenario := range scenarios {
			args := append([]string{"test", "-mod=mod", "-race", "-count=1", "-v", "-run", scenario.verify.Run}, scenario.verify.Packages...)
			output, runErr := runModuleTool(ctx, root, "go", args...)
			result := ModuleVerifyResult{
				Name: scenario.name, Want: scenario.verify.Pass,
				Passed:  len(modulePassLine.FindAllIndex(output, -1)),
				Skipped: len(moduleSkipLine.FindAllIndex(output, -1)),
			}
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
