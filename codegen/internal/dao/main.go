// tool/dao generates DAO accessor code (setters, marshal/unmarshal, DaoInterface).
//
// Usage:
//
//	go run ./tool/dao -def ./db/def -out ./db -pkg db
//
// Scans def directory for //roost:dao and //roost:redisdao markers, generates:
//   - gen_<name>_dao.go   — private storage, typed accessors/mutators, Init, Marshal, Unmarshal
//   - gen_<name>_nested.go — private nested storage and typed accessors/mutators
//   - gen_<name>_redis_dao.go — typed Redis DAO wrapper
package dao

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tjbdwanghaibo/roost-codegen/internal/project"
)

// Default locations of the DAO definitions and the generated package inside a
// business project. Exported so `roost generate` refers to the same values
// instead of repeating them (U-0118).
const (
	DefaultDefDir = "./db/def"
	DefaultOutDir = "./db"
)

func Run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("dao", flag.ContinueOnError)
	flags.SetOutput(stdout)
	defDir := flags.String("def", DefaultDefDir, "directory containing DB definitions")
	outDir := flags.String("out", DefaultOutDir, "output directory for generated code")
	pkg := flags.String("pkg", "", "generated package name (default: detect from output directory)")
	force := flags.Bool("force", false, "force regeneration")
	if err := flags.Parse(args); err != nil {
		return err
	}

	absDefDir, err := filepath.Abs(*defDir)
	if err != nil {
		return fmt.Errorf("resolve definition directory: %w", err)
	}
	absOutDir, err := filepath.Abs(*outDir)
	if err != nil {
		return fmt.Errorf("resolve output directory: %w", err)
	}

	// Ensure output directory exists
	if err := os.MkdirAll(absOutDir, 0755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	if *pkg == "" {
		info, err := project.Discover(absOutDir)
		if err != nil {
			return fmt.Errorf("discover output project: %w", err)
		}
		*pkg, err = info.PackageName(absOutDir)
		if err != nil {
			return fmt.Errorf("detect output package: %w", err)
		}
	}

	// Parse definitions
	defs, err := parseDefDir(absDefDir)
	if err != nil {
		return fmt.Errorf("parse definitions: %w", err)
	}

	if len(defs.Daos) == 0 && len(defs.RedisDaos) == 0 && len(defs.Nested) == 0 {
		// Deleting the last definition must still retire its outputs: an
		// orphan compiles and registers, silently diverging from an empty
		// definition set. Generated files are always reproducible, and the
		// header check keeps hand-written files safe.
		removed, err := removeOrphanGenerated(absOutDir, nil)
		if err != nil {
			return err
		}
		for _, name := range removed {
			_, _ = fmt.Fprintf(stdout, "removed orphan: %s\n", filepath.Join(absOutDir, name))
		}
		_, _ = fmt.Fprintf(stdout, "no markers found in %s\n", absDefDir)
		return nil
	}

	expected := make(map[string]bool, len(defs.Daos)+len(defs.RedisDaos)+len(defs.Nested))

	// Generate DAO files
	for _, dao := range defs.Daos {
		outFile := filepath.Join(absOutDir, daoFileName(dao.Name))
		expected[filepath.Base(outFile)] = true
		changed, err := generateDao(dao, defs, *pkg, outFile, *force)
		if err != nil {
			return fmt.Errorf("generate dao %s: %w", dao.Name, err)
		}
		if changed {
			_, _ = fmt.Fprintf(stdout, "generated: %s\n", outFile)
		} else {
			_, _ = fmt.Fprintf(stdout, "unchanged: %s\n", outFile)
		}
	}

	for _, dao := range defs.RedisDaos {
		outFile := filepath.Join(absOutDir, redisDaoFileName(dao.Name))
		expected[filepath.Base(outFile)] = true
		changed, err := generateRedisDao(dao, *pkg, outFile, *force)
		if err != nil {
			return fmt.Errorf("generate redis dao %s: %w", dao.Name, err)
		}
		if changed {
			_, _ = fmt.Fprintf(stdout, "generated: %s\n", outFile)
		} else {
			_, _ = fmt.Fprintf(stdout, "unchanged: %s\n", outFile)
		}
	}

	// Generate nested struct files
	for _, nested := range defs.Nested {
		outFile := filepath.Join(absOutDir, fmt.Sprintf("gen_%s_nested.go", toSnake(nested.Name)))
		expected[filepath.Base(outFile)] = true
		changed, err := generateNested(nested, defs, *pkg, outFile, *force)
		if err != nil {
			return fmt.Errorf("generate nested %s: %w", nested.Name, err)
		}
		if changed {
			_, _ = fmt.Fprintf(stdout, "generated: %s\n", outFile)
		} else {
			_, _ = fmt.Fprintf(stdout, "unchanged: %s\n", outFile)
		}
	}
	if len(defs.Nested) > 0 {
		outFile := filepath.Join(absOutDir, bsonHelpersFileName)
		expected[filepath.Base(outFile)] = true
		changed, err := generateBSONHelpers(*pkg, outFile, *force)
		if err != nil {
			return fmt.Errorf("generate bson helpers: %w", err)
		}
		if changed {
			_, _ = fmt.Fprintf(stdout, "generated: %s\n", outFile)
		} else {
			_, _ = fmt.Fprintf(stdout, "unchanged: %s\n", outFile)
		}
	}

	// Renamed or deleted definitions must not leave stale generated files
	// behind: an orphan still compiles, still registers, and silently
	// diverges from the definitions.
	removed, err := removeOrphanGenerated(absOutDir, expected)
	if err != nil {
		return err
	}
	for _, name := range removed {
		_, _ = fmt.Fprintf(stdout, "removed orphan: %s\n", filepath.Join(absOutDir, name))
	}
	return nil
}

// daoFileName derives gen_<name>_dao.go, collapsing a Dao-suffixed struct
// name so HeroDao maps to gen_hero_dao.go rather than gen_hero_dao_dao.go.
func daoFileName(structName string) string {
	base := strings.TrimSuffix(toSnake(structName), "_dao")
	return fmt.Sprintf("gen_%s_dao.go", base)
}

// redisDaoFileName mirrors daoFileName for gen_<name>_redis_dao.go.
func redisDaoFileName(structName string) string {
	base := strings.TrimSuffix(toSnake(structName), "_dao")
	base = strings.TrimSuffix(base, "_redis")
	return fmt.Sprintf("gen_%s_redis_dao.go", base)
}

const generatedHeader = "// Code generated by tool/dao. DO NOT EDIT."

// removeOrphanGenerated deletes generated files in dir that no current
// definition produced. Only files matching this generator's naming pattern
// AND carrying its generated header are candidates, so a hand-written file
// can never be deleted.
func removeOrphanGenerated(dir string, expected map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("scan output directory: %w", err)
	}
	var removed []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || expected[name] || !strings.HasPrefix(name, "gen_") {
			continue
		}
		if !strings.HasSuffix(name, "_dao.go") && !strings.HasSuffix(name, "_nested.go") {
			continue
		}
		path := filepath.Join(dir, name)
		content, err := os.ReadFile(path)
		if err != nil {
			return removed, fmt.Errorf("read %s: %w", name, err)
		}
		if !strings.HasPrefix(string(content), generatedHeader) {
			continue
		}
		if err := os.Remove(path); err != nil {
			return removed, fmt.Errorf("remove orphan %s: %w", name, err)
		}
		removed = append(removed, name)
	}
	return removed, nil
}
