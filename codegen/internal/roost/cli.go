package roost

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
)

var buildVersion string

func Run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printHelpOverview(stdout)
		return nil
	}
	if args[0] == "help" {
		return runHelp(args[1:], stdout)
	}
	if isHelpToken(args[0]) {
		printHelpOverview(stdout)
		return nil
	}
	if len(args) >= 2 && isHelpToken(args[len(args)-1]) {
		return runContextHelp(args[:len(args)-1], stdout)
	}
	switch args[0] {
	case "project":
		return runProject(args[1:], stdout, stderr)
	case "generate":
		return runGenerate(args[1:], stdout, stderr)
	case "add":
		return runAdd(args[1:], stdout, stderr)
	case "config":
		return runConfig(args[1:], stdout, stderr)
	case "id":
		return runID(args[1:], stdout, stderr)
	case "version":
		return runVersion(args[1:], stdout)
	case "env":
		return runEnvironment(args[1:], stdout)
	case "format":
		if len(args) != 2 || args[1] != "check" {
			return errors.New("usage: roost format check")
		}
		root, err := projectRoot(".")
		if err != nil {
			return err
		}
		if err := CheckFormat(root); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "Go files are formatted")
		return nil
	case "framework":
		return runFramework(args[1:], stdout, stderr)
	case "help-make":
		printMakeHelp(stdout)
		return nil
	default:
		return fmt.Errorf("unknown command %q; run roost help", args[0])
	}
}

func runFramework(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "verify" {
		return errors.New("usage: roost framework verify [--manifest path] [--expected-codegen version] [--lock path] [--github-output path]")
	}
	fs := flag.NewFlagSet("framework verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	manifest := fs.String("manifest", "ci/framework-release.yaml", "framework release manifest")
	expected := fs.String("expected-codegen", "", "required codegen version, normally the release tag")
	lock := fs.String("lock", "framework-lock.json", "output lock file; empty disables it")
	githubOutput := fs.String("github-output", "", "optional GitHub Actions output file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	return VerifyFrameworkRelease(*manifest, *expected, *lock, *githubOutput, stdout)
}

func runProject(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: roost project <new|sync|diff|doctor|next|upgrade|deps>")
	}
	switch args[0] {
	case "new":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return errors.New("usage: roost project new <name> -module <go-module> [flags]")
		}
		fs := flag.NewFlagSet("project new", flag.ContinueOnError)
		fs.SetOutput(stderr)
		module := fs.String("module", "", "target Go module")
		out := fs.String("out", "", "target directory")
		services := fs.String("services", "game", "comma-separated services")
		mods := fs.String("mods", "", "comma-separated service Kit mods")
		features := fs.String("features", "", "comma-separated features")
		core := fs.String("roost-core-version", "", "roost-core version")
		kit := fs.String("roost-kit-version", "", "roost-kit version")
		skill := fs.String("roost-skill-version", "", "roost-skill version")
		serviceVersion := fs.String("roost-service-version", "", "roost-service version")
		codegen := fs.String("codegen-version", "", "roost-codegen version")
		template := fs.String("template", "", "opt-in starting shape: game (hosts account, chat, mail, match, session and wires the first service to them), or game-demo (game plus a working Player write path: Profile/Bag components, a DAO, one Nest transaction and a TCP endpoint)")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected arguments %q; usage: roost project new <name> -module <go-module> [flags]", fs.Args())
		}
		if strings.TrimSpace(*module) == "" {
			return fmt.Errorf("-module is required so generated imports do not use someone else's repository; example: roost project new %s -module github.com/<your-account>/%s", args[1], toSnake(args[1]))
		}
		result, target, err := NewProject(NewOptions{Name: toSnake(args[1]), Module: *module, Out: *out, Services: splitList(*services), Mods: splitList(*mods), Features: splitList(*features), Versions: VersionSpec{Core: *core, Kit: *kit, Skill: *skill, Service: *serviceVersion, Codegen: *codegen}, Template: *template})
		if err != nil {
			return err
		}
		manifest, err := LoadManifest(target)
		if err != nil {
			return err
		}
		printSyncResult(stdout, result)
		fmt.Fprintf(stdout, "project files ready: %s\n", target)
		if err := UpdateFrameworkDependencies(target, manifest, stdout, stderr); err != nil {
			return fmt.Errorf("project files created at %s but framework resolution failed; after connectivity recovers run roost project deps --root %s: %w", target, target, err)
		}
		fmt.Fprintf(stdout, "project ready: %s\n", target)
		return nil
	case "sync", "diff", "doctor", "next", "upgrade", "deps":
		fs := flag.NewFlagSet("project "+args[0], flag.ContinueOnError)
		fs.SetOutput(stderr)
		rootFlag := fs.String("root", ".", "project directory")
		strict := fs.Bool("strict", true, "include generated freshness checks")
		jsonOutput := fs.Bool("json", false, "JSON output")
		workflow := fs.String("workflow", "", "validate a complete workflow (first-business or player-tcp)")
		dryRun := fs.Bool("dry-run", false, "preview an upgrade without writing files or resolving dependencies")
		core := fs.String("core", "", "new core version")
		kit := fs.String("kit", "", "new kit version")
		skill := fs.String("skill", "", "removed: skill ships inside roost-core since v1.14.0")
		serviceVersion := fs.String("service", "", "removed: the services ship inside roost-kit since v1.13.0")
		codegen := fs.String("codegen", "", "new codegen version")
		consolidate := fs.Bool("consolidate", false, "rewrite imports from roost-skill / roost-service / roost-kit implementation packages to their consolidated locations (core v1.14.0 / kit v1.13.0)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected arguments %q", fs.Args())
		}
		allowed := map[string][]string{
			"sync": {"root"}, "diff": {"root"}, "doctor": {"root", "strict", "json", "workflow"},
			"next": {"root", "workflow"}, "deps": {"root"},
			"upgrade": {"root", "dry-run", "core", "kit", "skill", "service", "codegen", "consolidate"},
		}
		if err := rejectUnsupportedFlags(fs, allowed[args[0]]...); err != nil {
			return err
		}
		root, err := projectRoot(*rootFlag)
		if err != nil {
			return err
		}
		switch args[0] {
		case "sync":
			result, err := SyncProject(root)
			if err != nil {
				return err
			}
			manifest, err := LoadManifest(root)
			if err != nil {
				return err
			}
			if err := UpdateFrameworkDependencies(root, manifest, stdout, stderr); err != nil {
				return fmt.Errorf("project files synchronized but framework resolution failed: %w", err)
			}
			printSyncResult(stdout, result)
			return nil
		case "diff":
			return DiffProject(root, stdout)
		case "doctor":
			return DoctorWithOptions(root, DoctorOptions{Strict: *strict, JSONOutput: *jsonOutput, Workflow: *workflow}, stdout)
		case "next":
			return PrintNextStep(root, *workflow, stdout)
		case "deps":
			manifest, err := LoadManifest(root)
			if err != nil {
				return err
			}
			return UpdateFrameworkDependencies(root, manifest, stdout, stderr)
		case "upgrade":
			if *skill != "" || *serviceVersion != "" {
				return errors.New("-skill and -service no longer exist: skill ships inside roost-core (v1.14.0+) and the services inside roost-kit (v1.13.0+); run roost project upgrade --consolidate to rewrite the project's imports")
			}
			if *consolidate {
				if _, err := ConsolidateProject(root, *dryRun, stdout); err != nil {
					return err
				}
				if *dryRun {
					// The manifest diff below would show the pre-consolidation
					// manifest; the rewrite report above is the preview.
					return nil
				}
			} else if needsConsolidation(root) {
				return errors.New("this project still requires roost-skill / roost-service; run roost project upgrade --consolidate first (add --dry-run to preview the import rewrite)")
			}
			m, err := loadManifestForUpgrade(root)
			if err != nil {
				return err
			}
			mergeVersions(&m.Versions, VersionSpec{Core: *core, Kit: *kit, Codegen: *codegen})
			if err := m.Validate(); err != nil {
				return fmt.Errorf("validate upgraded manifest: %w", err)
			}
			if *dryRun {
				return diffManifest(root, m, true, stdout)
			}
			if _, _, err := planManifestSync(root, m); err != nil {
				return fmt.Errorf("preflight project upgrade: %w", err)
			}
			manifestBefore, err := os.ReadFile(filepath.Join(root, ManifestName))
			if err != nil {
				return err
			}
			result, err := commitManifestSyncResult(root, manifestBefore, m)
			if err != nil {
				return err
			}
			manifest, err := LoadManifest(root)
			if err != nil {
				return err
			}
			if err := UpdateFrameworkDependencies(root, manifest, stdout, stderr); err != nil {
				return fmt.Errorf("project upgraded but framework resolution failed: %w", err)
			}
			printSyncResult(stdout, result)
			return nil
		}
	}
	return fmt.Errorf("unknown project command %q", args[0])
}

func runGenerate(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rootFlag := fs.String("root", ".", "project directory")
	changed := fs.Bool("changed", false, "only generators affected by git changes")
	check := fs.Bool("check", false, "verify in a temporary copy")
	dry := fs.Bool("dry-run", false, "print generation plan")
	force := fs.Bool("force", false, "rewrite outputs even when content is unchanged")
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
	return GenerateTransactional(root, GenerateOptions{Changed: *changed, Check: *check, DryRun: *dry, Force: *force, Stdout: stdout}, stderr)
}

func runAdd(args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 {
		return errors.New("usage: roost add <service|mod|access|transport|module|protocol|entity|component|handler|lifecycle|endpoint|skill|event|table|dao|webroute|errcode|saga> <name> [flags]")
	}
	fs := flag.NewFlagSet("add "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	rootFlag := fs.String("root", ".", "project directory")
	service := fs.String("service", "", "target service")
	mods := fs.String("mods", "", "comma-separated Kit mods")
	steps := fs.String("steps", "", "comma-separated Saga steps")
	group := fs.String("group", "", "ID allocation group")
	entityName := fs.String("entity", "", "owning Entity for component/DAO wiring")
	componentName := fs.String("component", "", "Component capability for a Nest handler")
	handlerDomain := fs.String("handler", "", "protocol controller domain")
	protocolName := fs.String("protocol", "", "protocol name for an endpoint")
	nestHandler := fs.String("nest-handler", "", "Nest handler name for an endpoint")
	id := fs.Int64("id", 0, "explicit numeric ID")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	allowed := map[string][]string{
		"service": {"root", "mods"}, "mod": {"root", "service"}, "access": {"root", "service"},
		"transport": {"root", "service"}, "module": {"root"},
		"protocol": {"root", "group", "handler", "id"}, "entity": {"root", "id"},
		"component": {"root", "entity", "id"}, "handler": {"root", "entity", "component"},
		"lifecycle": {"root", "entity", "service"}, "endpoint": {"root", "handler", "protocol", "nest-handler"},
		"skill": {"root"}, "event": {"root"}, "table": {"root"}, "dao": {"root", "entity"},
		"webroute": {"root"}, "errcode": {"root", "id"}, "saga": {"root", "service", "steps"},
		"rpc": {"root", "service"},
	}
	if err := rejectUnsupportedFlags(fs, allowed[args[0]]...); err != nil {
		return err
	}
	root, err := projectRoot(*rootFlag)
	if err != nil {
		return err
	}
	paths, err := Add(root, AddOptions{Kind: args[0], Name: args[1], Service: *service, Mods: splitList(*mods), Steps: splitList(*steps), Group: *group, Entity: *entityName, Component: *componentName, Handler: *handlerDomain, Protocol: *protocolName, NestHandler: *nestHandler, ID: *id})
	if err != nil {
		return err
	}
	for _, path := range paths {
		fmt.Fprintf(stdout, "created/updated: %s\n", path)
	}
	if args[0] == "entity" || args[0] == "component" || args[0] == "dao" || args[0] == "handler" || args[0] == "lifecycle" || args[0] == "endpoint" {
		fmt.Fprintln(stdout, "next: roost project next")
	}
	if args[0] == "access" || args[0] == "transport" || args[0] == "protocol" {
		fmt.Fprintln(stdout, "next: roost project next")
	}
	if args[0] == "skill" {
		fmt.Fprintln(stdout, "next: edit game/skills/<name>.json, then compile it through game/skills.CompileAll")
		fmt.Fprintln(stdout, "guide: docs/SKILL.zh-CN.md")
	}
	return nil
}

func runConfig(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: roost config <check|enable|disable> [player-tcp] [flags]")
	}
	if args[0] == "enable" || args[0] == "disable" {
		if len(args) < 2 || args[1] != "player-tcp" {
			return fmt.Errorf("usage: roost config %s player-tcp [--service <name>] [--file <path>]", args[0])
		}
		fs := flag.NewFlagSet("config "+args[0]+" player-tcp", flag.ContinueOnError)
		fs.SetOutput(stderr)
		rootFlag := fs.String("root", ".", "project directory")
		service := fs.String("service", "", "service name; defaults to access.player.service")
		file := fs.String("file", "", "explicit service config path")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected arguments %q", fs.Args())
		}
		root, err := projectRoot(*rootFlag)
		if err != nil {
			return err
		}
		manifest, err := LoadManifest(root)
		if err != nil {
			return err
		}
		access, exists := manifest.Access["player"]
		if !exists || !contains(access.Transports, "tcp") {
			return errors.New("player TCP is not generated; run: roost add access player --service <service>, then roost add transport tcp")
		}
		targetService := toSnake(*service)
		if targetService == "" {
			targetService = access.Service
		}
		if targetService != access.Service {
			return fmt.Errorf("player access belongs to service %q, not %q", access.Service, targetService)
		}
		enabled := args[0] == "enable"
		if enabled {
			authPath := filepath.Join(root, "internal", "access", "player", "tcp", "auth.go")
			auth, readErr := os.ReadFile(authPath)
			if readErr != nil {
				return fmt.Errorf("read player TCP authenticator: %w", readErr)
			}
			if !playerTCPAuthenticatorImplemented(auth) {
				return errors.New("refusing to enable player TCP: auth.go is still the fail-closed skeleton or does not consume a named token parameter; implement and review internal/access/player/tcp/auth.go first")
			}
		}
		path, err := ensurePlayerTCPConfig(root, targetService, *file, enabled)
		if err != nil {
			return err
		}
		state := "disabled"
		if enabled {
			state = "enabled"
		}
		fmt.Fprintf(stdout, "player TCP %s: %s\n", state, filepath.ToSlash(path))
		if enabled {
			fmt.Fprintln(stdout, "next: roost project doctor --workflow player-tcp")
		} else {
			fmt.Fprintln(stdout, "listener remains fail-closed until explicitly enabled again")
		}
		return nil
	}
	if args[0] != "check" {
		return errors.New("usage: roost config <check|enable|disable> [player-tcp] [flags]")
	}
	fs := flag.NewFlagSet("config check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rootFlag := fs.String("root", ".", "project directory")
	service := fs.String("service", "", "service name")
	all := fs.Bool("all", false, "check every service config declared in roost.yaml")
	production := fs.Bool("production", false, "enforce production-safe values")
	file := fs.String("file", "", "explicit config path")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	root, err := projectRoot(*rootFlag)
	if err != nil {
		return err
	}
	if *all {
		if *service != "" || *file != "" {
			return errors.New("--all cannot be combined with --service or --file")
		}
		manifest, loadErr := LoadManifest(root)
		if loadErr != nil {
			return loadErr
		}
		var joined error
		for _, name := range sortedServiceNames(manifest) {
			path := filepath.Join(root, "configs", "service", "config."+name+".yaml")
			if checkErr := CheckConfig(path, *production); checkErr != nil {
				joined = errors.Join(joined, fmt.Errorf("service %s: %w", name, checkErr))
				continue
			}
			fmt.Fprintf(stdout, "config ok: %s\n", filepath.ToSlash(path))
		}
		return joined
	}
	path := *file
	if path == "" {
		if *service == "" {
			return errors.New("--service or --file is required")
		}
		path = filepath.Join(root, "configs", "service", "config."+*service+".yaml")
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	if err := CheckConfig(path, *production); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "config is valid: %s\n", path)
	return nil
}

func runID(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: roost id <next|check>")
	}
	fs := flag.NewFlagSet("id "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	rootFlag := fs.String("root", ".", "project directory")
	group := fs.String("group", "", "ID group")
	parseArgs := args[1:]
	if args[0] == "next" {
		parseArgs = normalizeNextIDArgs(parseArgs)
	}
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}
	if args[0] == "check" {
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected arguments %q", fs.Args())
		}
		if err := rejectUnsupportedFlags(fs, "root"); err != nil {
			return err
		}
	}
	root, err := projectRoot(*rootFlag)
	if err != nil {
		return err
	}
	m, err := LoadManifest(root)
	if err != nil {
		return err
	}
	switch args[0] {
	case "check":
		if err := CheckIDs(root, m); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "IDs are valid")
		return nil
	case "next":
		if fs.NArg() != 1 {
			return errors.New("usage: roost id next <kind> [--group <group>]")
		}
		id, err := NextID(root, m, fs.Arg(0), *group)
		if err == nil {
			fmt.Fprintln(stdout, id)
		}
		return err
	default:
		return fmt.Errorf("unknown id command %q", args[0])
	}
}

func normalizeNextIDArgs(args []string) []string {
	if len(args) <= 1 || strings.HasPrefix(args[0], "-") {
		return args
	}
	normalized := make([]string, 0, len(args))
	normalized = append(normalized, args[1:]...)
	normalized = append(normalized, args[0])
	return normalized
}

func printMakeHelp(w io.Writer) {
	fmt.Fprint(w, `Targets: next sync project-upgrade deps-update roost-up codegen-up doctor generate generate-changed check-generated config-check config-check-all player-tcp-enable player-tcp-disable id-check build run loadtest test test-race ci cicd-check release-check image-build compose-check k8s-render k8s-check deploy-shell rollback-shell deploy-docker rollback-docker deploy-k8s rollback-k8s dev-up dev-down dev-logs
Scaffolds: new-entity NAME=Player | new-component NAME=Profile ENTITY=Player | new-handler NAME=RenamePlayer ENTITY=Player COMPONENT=Profile | new-dao NAME=Player ENTITY=Player
`)
}

func rejectUnsupportedFlags(fs *flag.FlagSet, allowed ...string) error {
	set := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		set[name] = true
	}
	var unsupported []string
	fs.Visit(func(current *flag.Flag) {
		if !set[current.Name] {
			unsupported = append(unsupported, "-"+current.Name)
		}
	})
	if len(unsupported) > 0 {
		return fmt.Errorf("unsupported flags for %s: %s", fs.Name(), strings.Join(unsupported, ", "))
	}
	return nil
}

func runVersion(args []string, stdout io.Writer) error {
	if len(args) != 0 {
		return errors.New("usage: roost version")
	}
	version := "development"
	if buildVersion != "" {
		version = buildVersion
	} else if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
	fmt.Fprintf(stdout, "roost-codegen %s\n", version)
	return nil
}

func runEnvironment(args []string, stdout io.Writer) error {
	if len(args) != 1 || args[0] != "doctor" {
		return errors.New("usage: roost env doctor")
	}
	var missingRequired []string
	for _, item := range []struct {
		name     string
		required bool
	}{{"go", true}, {"git", true}, {"make", false}, {"docker", false}} {
		path, err := exec.LookPath(item.name)
		status := "OK"
		detail := path
		if err != nil {
			if item.required {
				status = "FAIL"
				missingRequired = append(missingRequired, item.name)
			} else {
				status = "WARN"
			}
			detail = "not found in PATH"
		}
		fmt.Fprintf(stdout, "%-4s %-8s %s\n", status, item.name, detail)
	}
	if raw, err := exec.Command("go", "env", "GOPROXY", "GOBIN", "GOPATH").Output(); err == nil {
		values := strings.Split(strings.TrimSpace(string(raw)), "\n")
		labels := []string{"GOPROXY", "GOBIN", "GOPATH"}
		for index, label := range labels {
			value := ""
			if index < len(values) {
				value = strings.TrimSpace(values[index])
			}
			if label == "GOBIN" && value == "" {
				value = "<empty; go install uses GOPATH/bin>"
			}
			fmt.Fprintf(stdout, "INFO %-8s %s\n", label, value)
		}
	}
	if len(missingRequired) > 0 {
		return fmt.Errorf("required tools missing from PATH: %s", strings.Join(missingRequired, ", "))
	}
	return nil
}
