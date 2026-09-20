// tool/entity generates entity factory, snapshot, and wiring code.
//
// Usage:
//
//	go run ./tool/entity -dir ./game/player
//
// Or via go:generate in the entity definition file:
//
//	//go:generate go run github.com/tjbdwanghaibo/cube/tool/entity
//
// The generator scans for struct types annotated with the marker comment:
//
//	//roost:entity entityKind=EntityKindPlayer
//	type Player struct { ... }
//
// And produces a <entity>_gen_wire.go file containing:
//   - NewXxx(param) factory function
//   - Base/Dirty and component/DAO accessor methods
//   - transactional PrepareDelete() registration for persistent DAOs
//   - Hooks wiring (onClear, onDestroy)
package entity

import (
	"flag"
	"fmt"
	"github.com/tjbdwanghaibo/roost-codegen/internal/marker"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("entity", flag.ContinueOnError)
	flags.SetOutput(stdout)
	dir := flags.String("dir", "", "directory to scan (default: GOFILE dir or cwd)")
	output := flags.String("output", "", "output file (default: <entity>_gen_wire.go)")
	force := flags.Bool("force", false, "force regeneration even if unchanged")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments %q", flags.Args())
	}

	// Determine scan directory
	scanDir := *dir
	if scanDir == "" {
		// Support go:generate mode
		if gofile := os.Getenv("GOFILE"); gofile != "" {
			scanDir = filepath.Dir(gofile)
		} else {
			scanDir = "."
		}
	}

	scanDir, err := filepath.Abs(scanDir)
	if err != nil {
		return fmt.Errorf("resolve dir: %w", err)
	}

	scanDirs := []string{scanDir}
	if *output == "" {
		scanDirs, err = findEntityDirs(scanDir)
		if err != nil {
			return fmt.Errorf("scan entities: %w", err)
		}
	}
	if len(scanDirs) == 0 {
		fmt.Fprintf(stdout, "no entity markers found in %s\n", scanDir)
		return nil
	}

	for _, dir := range scanDirs {
		// Parse all .go files in the directory
		entities, pkg, err := parseDir(dir)
		if err != nil {
			return fmt.Errorf("parse %s: %w", dir, err)
		}
		if len(entities) == 0 {
			continue
		}

		// -output names ONE file, which only makes sense for the go:generate
		// single-entity mode. Each entity generates its own <name>_gen_wire.go
		// plus a companion guard test, so pointing every entity of a package at
		// one path silently overwrote all but the last, taking the package-level
		// RegisterEntity down with whichever file lost. The tool exited 0 and
		// the consumer failed to compile (RR-20260909-06). Refused here, before
		// anything is written, so a failed run leaves no partial output.
		if *output != "" && len(entities) > 1 {
			names := make([]string, 0, len(entities))
			for _, ent := range entities {
				names = append(names, ent.Name)
			}
			sort.Strings(names)
			return fmt.Errorf("-output names one file but %s declares %d entities (%s): each entity generates its own file, so one path would overwrite all but the last; drop -output to generate them side by side",
				dir, len(entities), strings.Join(names, ", "))
		}

		// Generate for each entity. The package-level RegisterEntity goes into
		// the first entity's file (by name) and calls every sibling's own
		// registration, so several entities in one package compile together.
		siblings := make([]string, 0, len(entities))
		for _, ent := range entities {
			siblings = append(siblings, ent.Name)
		}
		sort.Strings(siblings)
		for _, ent := range entities {
			outFile := *output
			if outFile == "" {
				outFile = filepath.Join(dir, fmt.Sprintf("%s_gen_wire.go", toSnake(ent.Name)))
			}

			changed, err := generateInPackage(ent, siblings, pkg, outFile, *force)
			if err != nil {
				return fmt.Errorf("generate %s: %w", ent.Name, err)
			}
			if changed {
				fmt.Fprintf(stdout, "generated: %s\n", outFile)
			} else {
				fmt.Fprintf(stdout, "unchanged: %s\n", outFile)
			}
		}
	}
	return nil
}

func findEntityDirs(root string) ([]string, error) {
	dirs := make(map[string]bool)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root {
				switch entry.Name() {
				case "testdata", "vendor":
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") ||
			isGeneratedWireFile(entry.Name()) ||
			strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if marker.Has(string(content), "entity") {
			dirs[filepath.Dir(path)] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, len(dirs))
	for dir := range dirs {
		out = append(out, dir)
	}
	sort.Strings(out)
	return out, nil
}
