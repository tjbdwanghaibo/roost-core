// tool/nest generates invoke wrappers, sender functions, and explicit registration
// for the nest dispatch framework.
//
// Usage:
//
//	go run ./tool/nest -dir ./game
//
// Scans -dir recursively for functions marked with //roost:nest,
// and generates:
//   - <source>_nest_gen.go — invoke wrapper + RegisterNestHandlers()
//   - sender/<source>_nest_gen.go — typed async sender functions (optional, with -sender)
//   - syncsender/<source>_nest_gen.go — typed sync sender functions (optional, with -sender)
//   - internal/registry/nest_gen.go — aggregate RegisterNestHandlers() for game scans
package nest

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("nest", flag.ContinueOnError)
	flags.SetOutput(stdout)
	dir := flags.String("dir", ".", "directory to scan for //roost:nest markers")
	force := flags.Bool("force", false, "force regeneration even if unchanged")
	sender := flags.Bool("sender", true, "generate sender package (default: true)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments %q", flags.Args())
	}

	absDir, err := filepath.Abs(*dir)
	if err != nil {
		return fmt.Errorf("resolve dir: %w", err)
	}
	generateBootstrap := filepath.Base(absDir) == "game"
	var moduleRoot string
	var modulePath string
	if generateBootstrap {
		moduleRoot, modulePath, err = findModuleInfo(absDir)
		if err != nil {
			return fmt.Errorf("find module: %w", err)
		}
	}

	// Walk directory and find all files with supported Nest markers.
	var totalGenerated int
	var bootstrapRegs []bootstrapRegistration
	err = filepath.Walk(absDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == "vendor" || base == "testdata" || strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_nest_gen.go") || strings.HasPrefix(filepath.Base(path), "gen_") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}

		funcs, pkg, err := parseFile(path)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		if len(funcs) == 0 {
			return nil
		}

		// Generate handler file (same package)
		outDir := filepath.Dir(path)
		baseName := strings.TrimSuffix(filepath.Base(path), ".go")
		outFile := filepath.Join(outDir, baseName+"_nest_gen.go")

		registerFunc := registerFuncName(baseName)
		if generateBootstrap && !hasReceiverHandler(funcs) {
			importPath, err := importPathForDir(moduleRoot, modulePath, outDir)
			if err != nil {
				return fmt.Errorf("resolve import path %s: %w", outDir, err)
			}
			bootstrapRegs = append(bootstrapRegs, bootstrapRegistration{
				ImportPath:   importPath,
				RegisterFunc: registerFunc,
			})
		}
		changed, err := generate(funcs, pkg, outFile, *force, false, registerFunc)
		if err != nil {
			return fmt.Errorf("generate %s: %w", outFile, err)
		}
		if changed {
			fmt.Fprintf(stdout, "generated: %s\n", outFile)
			totalGenerated++
		}

		// Generate sender package
		if *sender {
			senderDir := filepath.Join(outDir, "sender")
			if err := os.MkdirAll(senderDir, 0755); err != nil {
				return fmt.Errorf("mkdir sender: %w", err)
			}
			senderFile := filepath.Join(senderDir, baseName+"_nest_gen.go")
			changed, err := generate(funcs, pkg+"_sender", senderFile, *force, true, "")
			if err != nil {
				return fmt.Errorf("generate sender %s: %w", senderFile, err)
			}
			if changed {
				fmt.Fprintf(stdout, "generated: %s\n", senderFile)
				totalGenerated++
			}
			testChanged, err := generateSenderGuardTest(pkg+"_sender", senderFile, *force)
			if err != nil {
				return fmt.Errorf("generate sender guard test %s: %w", senderFile, err)
			}
			if testChanged {
				fmt.Fprintf(stdout, "generated: %s\n", guardTestFileFor(senderFile))
				totalGenerated++
			}
			syncSenderDir := filepath.Join(outDir, "syncsender")
			if err := os.MkdirAll(syncSenderDir, 0755); err != nil {
				return fmt.Errorf("mkdir syncsender: %w", err)
			}
			syncSenderFile := filepath.Join(syncSenderDir, baseName+"_nest_gen.go")
			changed, err = generateSyncSender(funcs, pkg+"_syncsender", syncSenderFile, *force)
			if err != nil {
				return fmt.Errorf("generate syncsender %s: %w", syncSenderFile, err)
			}
			if changed {
				fmt.Fprintf(stdout, "generated: %s\n", syncSenderFile)
				totalGenerated++
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("walk: %w", err)
	}
	if generateBootstrap {
		// The aggregate lives with the other static registrations so the
		// project has one registration package, not a game/bootstrap and an
		// internal/registry that both mean "wire things up at startup".
		registryDir := filepath.Join(moduleRoot, "internal", "registry")
		if err := os.MkdirAll(registryDir, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", registryDir, err)
		}
		outFile := filepath.Join(registryDir, "nest_gen.go")
		changed, err := generateBootstrapNest(bootstrapRegs, outFile, *force)
		if err != nil {
			return fmt.Errorf("generate bootstrap %s: %w", outFile, err)
		}
		if changed {
			fmt.Fprintf(stdout, "generated: %s\n", outFile)
			totalGenerated++
		}
	}

	if totalGenerated == 0 {
		fmt.Fprintln(stdout, "all files up to date")
	}
	return nil
}

func hasReceiverHandler(funcs []*FuncInfo) bool {
	for _, fn := range funcs {
		if fn != nil && fn.ReceiverType != "" {
			return true
		}
	}
	return false
}

func registerFuncName(baseName string) string {
	parts := strings.FieldsFunc(baseName, func(r rune) bool {
		return r == '_' || r == '-' || r == '.'
	})
	var b strings.Builder
	b.WriteString("Register")
	for _, p := range parts {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]))
		if len(p) > 1 {
			b.WriteString(p[1:])
		}
	}
	b.WriteString("NestHandlers")
	return b.String()
}
