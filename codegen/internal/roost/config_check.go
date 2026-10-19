package roost

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// roost config check 的退出码（维护者 2026-10-19 批准；写在 roost help config 与 CODEGEN_REFERENCE）。几类问题同时出现时
// 取最前面的一类：键错了，跨 Mod 规则的结论没有意义；跨 Mod 规则不过，进程起不来，跨服务比较排在其后。
//
//	0  全部检查都跑了并通过（--keys-only 时：键检查与 --all 的跨服务检查通过，跨 Mod 规则按要求没查）
//	1  命令本身没跑成：参数错误、roost.yaml 读不出来（与其他 roost 命令相同）
//	2  键错误：YAML / sid / 生产禁用值、值不合声明、没有任何声明的键（多半拼错）
//	3  跨 Mod 规则错误：编译出的进程 --check-config 拒绝（ValidateConfig 与 app.ModConfigCrossChecker）
//	4  跨服务不一致：共享资源名不同、开发配置集里监听端口冲突（只在 --all 时比较）
//	5  检查没有跑全、已跑的都通过：工程编译不过所以跨 Mod 规则没查，或文件不是 config.<service>[.<变体>].yaml、键没按声明查
const (
	configExitKeyErrors          = 2
	configExitCrossModErrors     = 3
	configExitCrossServiceErrors = 4
	configExitNotFullyChecked    = 5
)

// ExitError 让命令以指定退出码结束（codegen/cmd/roost 的 main 读 ExitCode）。Error 是完整说明（调用方与测试用）；
// Brief 非空时 main 只在 stderr 打印它——细节已经写在 stdout 上，不再重复一遍。
type ExitError struct {
	Code  int
	Err   error
	Brief string
}

func (e *ExitError) Error() string       { return e.Err.Error() }
func (e *ExitError) Unwrap() error       { return e.Err }
func (e *ExitError) ExitCode() int       { return e.Code }
func (e *ExitError) ExitSummary() string { return e.Brief }

// 每份文件三步检查各自的结论。
const (
	checkOK         = "ok"
	checkFailed     = "failed"
	checkNotChecked = "not_checked"
)

// configCheckReport 是 --json 的输出，也是文本输出与退出码的唯一来源。
type configCheckReport struct {
	Files        []configFileResult `json:"files"`
	CrossService crossServiceResult `json:"cross_service"`
	Status       string             `json:"status"` // ok | key_errors | cross_mod_errors | cross_service_errors | not_fully_checked
	ExitCode     int                `json:"exit_code"`
}

type configFileResult struct {
	Path    string `json:"path"`
	Service string `json:"service,omitempty"`
	// Keys：键检查（YAML / sid / 生产禁用值与按声明检查）。not_checked 表示认不出服务、没按声明查。
	Keys       string   `json:"keys"`
	KeyErrors  []string `json:"key_errors,omitempty"`
	KeysReason string   `json:"keys_reason,omitempty"`
	// CrossMod：编译出的进程 --check-config（跨键与跨 Mod 规则）。没查时 CrossModReason 说明原因：
	// keys_only（--keys-only）、key_errors（键已经错了）、not_a_service_config、project_does_not_compile: …
	CrossMod       string   `json:"cross_mod"`
	CrossModReason string   `json:"cross_mod_reason,omitempty"`
	CrossModErrors []string `json:"cross_mod_errors,omitempty"`
}

type crossServiceResult struct {
	Checked  bool     `json:"checked"`
	Reason   string   `json:"reason,omitempty"`
	Services int      `json:"services,omitempty"`
	Errors   []string `json:"errors,omitempty"`
	// Warnings 不影响退出码：开发配置里的按服务端口与 roost.yaml 端口块号派生的值不一致。
	Warnings []string `json:"warnings,omitempty"`
}

func runConfigCheck(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("config check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rootFlag := fs.String("root", ".", "project directory")
	service := fs.String("service", "", "service name")
	all := fs.Bool("all", false, "check every service config declared in roost.yaml, and compare them across services")
	production := fs.Bool("production", false, "enforce production-safe values")
	file := fs.String("file", "", "explicit config path")
	keysOnly := fs.Bool("keys-only", false, "check keys only: do not compile the project or run the cross-mod rules")
	jsonOutput := fs.Bool("json", false, "print a machine-readable report on stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	root, err := projectRoot(*rootFlag)
	if err != nil {
		return err
	}
	// 键的声明检查要按 roost.yaml 知道每个服务装配了哪些 Mod；读不出来就失败，不能跳过声明检查后报通过。
	manifest, err := LoadManifest(root)
	if err != nil {
		return fmt.Errorf("config check reads %s for each service's declarations: %w", ManifestName, err)
	}
	type target struct{ path, service string }
	var targets []target
	if *all {
		if *service != "" || *file != "" {
			return errors.New("--all cannot be combined with --service or --file")
		}
		for _, name := range sortedServiceNames(manifest) {
			targets = append(targets, target{filepath.Join(root, "configs", "service", "config."+name+".yaml"), name})
		}
	} else {
		path, name := *file, *service
		if path == "" {
			if name == "" {
				return errors.New("--service or --file is required")
			}
			path = filepath.Join(root, "configs", "service", "config."+name+".yaml")
		} else {
			if !filepath.IsAbs(path) {
				path = filepath.Join(root, path)
			}
			if name == "" {
				name = configFileService(manifest, path)
			}
		}
		targets = append(targets, target{path, name})
	}

	// 与 doctor 共用一个编译后的进程：键检查发现可疑键时读回业务声明（--print-config-schema）；键检查通过后再交给
	// 进程的 --check-config 跑跨键与跨 Mod 规则（app.ModConfigCrossChecker）。规则只在进程里一份，config check、
	// --check-config 与 doctor config-check 对同一份配置的结论相同。--keys-only 不编译。
	var process *projectProcess
	if !*keysOnly {
		built, cleanup := newProjectProcess(context.Background(), root)
		defer cleanup()
		process = built
	}
	var report configCheckReport
	for _, t := range targets {
		report.Files = append(report.Files, checkOneConfigFile(root, manifest, t.path, t.service, *production, process))
	}
	if *all {
		problems, warnings, compared := checkConfigAcrossServices(root, manifest, devConfigSet(manifest))
		report.CrossService = crossServiceResult{Checked: true, Services: compared, Errors: problems, Warnings: warnings}
	} else {
		report.CrossService = crossServiceResult{Reason: "only with --all"}
	}
	report.Status, report.ExitCode = report.verdict()

	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return err
		}
	} else {
		report.writeText(stdout, *all)
	}
	if report.ExitCode == 0 {
		return nil
	}
	return &ExitError{Code: report.ExitCode, Err: report.summary(), Brief: fmt.Sprintf("config check: %s (exit %d); details above", report.Status, report.ExitCode)}
}

// checkOneConfigFile 跑一份文件的键检查与（键通过后）进程的 --check-config。process 为 nil 表示 --keys-only。
func checkOneConfigFile(root string, m Manifest, path, service string, production bool, process *projectProcess) configFileResult {
	result := configFileResult{Path: displayPath(root, path), Service: service, Keys: checkOK, CrossMod: checkNotChecked}
	if err := CheckConfig(path, production); err != nil {
		result.Keys, result.KeyErrors, result.CrossModReason = checkFailed, splitLines(err.Error()), "key_errors"
		return result
	}
	if service == "" {
		result.Keys = checkNotChecked
		result.KeysReason = fmt.Sprintf("not named config.<service>[.<variant>].yaml for a service in %s; pass --service", ManifestName)
		result.CrossModReason = "not_a_service_config"
		return result
	}
	var schemas serviceSchemaSource
	if process != nil {
		schemas = process.schema
	}
	problems, err := checkConfigFileDeclarations(path, production, m, service, schemas)
	if err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		if process == nil {
			problems = append(problems, "--keys-only: the service's own declarations were not read, so a key the service itself declares may be reported above")
		}
		result.Keys, result.KeyErrors, result.CrossModReason = checkFailed, problems, "key_errors"
		return result
	}
	if process == nil {
		result.CrossModReason = "keys_only"
		return result
	}
	// 工程编译不过时与 doctor 一样不判这一半（进程本身也起不来），但明确说没查，不报“valid”。
	if err := process.build(); err != nil {
		result.CrossModReason = "project_does_not_compile: " + lastBytes(err.Error(), 300)
		return result
	}
	if err := process.checkConfig(service, path); err != nil {
		result.CrossMod, result.CrossModErrors = checkFailed, splitLines(err.Error())
		return result
	}
	result.CrossMod = checkOK
	return result
}

func (r configCheckReport) verdict() (string, int) {
	keyErrors, crossModErrors, notFully := false, false, false
	for _, f := range r.Files {
		keyErrors = keyErrors || f.Keys == checkFailed
		crossModErrors = crossModErrors || f.CrossMod == checkFailed
		notFully = notFully || f.Keys == checkNotChecked || strings.HasPrefix(f.CrossModReason, "project_does_not_compile")
	}
	switch {
	case keyErrors:
		return "key_errors", configExitKeyErrors
	case crossModErrors:
		return "cross_mod_errors", configExitCrossModErrors
	case len(r.CrossService.Errors) > 0:
		return "cross_service_errors", configExitCrossServiceErrors
	case notFully:
		return "not_fully_checked", configExitNotFullyChecked
	}
	return "ok", 0
}

// writeText 是给人看的输出：每份文件一行结论（有问题时逐条列出），--all 时再加跨服务一行。
func (r configCheckReport) writeText(out io.Writer, all bool) {
	for _, f := range r.Files {
		switch {
		case f.Keys == checkFailed:
			fmt.Fprintf(out, "config invalid: %s\n", f.Path)
			writeItems(out, f.KeyErrors)
		case f.Keys == checkNotChecked:
			fmt.Fprintf(out, "keys not checked against declarations: %s is %s\n", f.Path, f.KeysReason)
		case f.CrossMod == checkFailed:
			fmt.Fprintf(out, "config rejected by the process (cross-key / cross-mod rules, same check as: go run . %s --check-config --config <file>): %s\n", f.Service, f.Path)
			writeItems(out, f.CrossModErrors)
		case f.CrossModReason == "keys_only":
			fmt.Fprintf(out, "config keys ok, cross-mod rules not checked (--keys-only): %s\n", f.Path)
		case strings.HasPrefix(f.CrossModReason, "project_does_not_compile"):
			fmt.Fprintf(out, "cross-mod rules NOT checked for %s: the project does not compile (%s); rerun after go build passes\n",
				f.Service, strings.TrimPrefix(f.CrossModReason, "project_does_not_compile: "))
			if all {
				fmt.Fprintf(out, "config keys ok (cross-mod rules not checked): %s\n", f.Path)
			} else {
				fmt.Fprintf(out, "config keys are valid (cross-mod rules not checked): %s\n", f.Path)
			}
		case all:
			fmt.Fprintf(out, "config ok: %s\n", f.Path)
		default:
			fmt.Fprintf(out, "config is valid: %s\n", f.Path)
		}
	}
	if !all {
		return
	}
	if len(r.CrossService.Warnings) > 0 {
		fmt.Fprintln(out, "cross-service warnings (exit code unaffected):")
		writeItems(out, r.CrossService.Warnings)
	}
	if len(r.CrossService.Errors) == 0 {
		fmt.Fprintf(out, "cross-service consistency ok: %d service(s) (shared resource names; development listen ports)\n", r.CrossService.Services)
		return
	}
	fmt.Fprintln(out, "cross-service inconsistencies:")
	writeItems(out, r.CrossService.Errors)
}

// summary 是非零退出时的错误说明（stderr）：点名每份出问题的文件 / 服务与问题本身。
func (r configCheckReport) summary() error {
	var parts []string
	for _, f := range r.Files {
		owner := f.Path
		if f.Service != "" {
			owner = "service " + f.Service
		}
		switch {
		case f.Keys == checkFailed:
			parts = append(parts, fmt.Sprintf("%s: %s", owner, strings.Join(f.KeyErrors, "; ")))
		case f.CrossMod == checkFailed:
			parts = append(parts, fmt.Sprintf("%s: the process refuses this config (cross-key / cross-mod rules, same check as: go run . %s --check-config --config <file>): %s", owner, f.Service, strings.Join(f.CrossModErrors, "; ")))
		}
	}
	for _, problem := range r.CrossService.Errors {
		parts = append(parts, "cross-service: "+problem)
	}
	if len(parts) == 0 && r.Status == "not_fully_checked" {
		parts = append(parts, "the keys passed, but not every check ran (see above): cross-mod rules need a project that compiles; keys are checked against declarations only for config.<service>[.<variant>].yaml or with --service")
	}
	return fmt.Errorf("config check: %s (exit %d): %s", r.Status, r.ExitCode, strings.Join(parts, "\n"))
}

func writeItems(out io.Writer, items []string) {
	for _, item := range items {
		fmt.Fprintf(out, "  - %s\n", strings.ReplaceAll(item, "\n", "\n    "))
	}
}

func splitLines(text string) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func displayPath(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(path)
}
