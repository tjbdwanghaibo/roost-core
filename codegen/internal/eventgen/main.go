// tool/eventgen generates event type constants, Type() methods, and handler dispatch code.
//
// Usage:
//
//	go run ./tool/eventgen -def ./event/def -out ./event -pkg event -game ./game
//
// Phase 1: Scans -def for structs prefixed with "Event", generates into -out:
//   - event_def_gen.go   — copied runtime event structs
//   - event_type_gen.go  — EventType constants
//   - event_type_impl_gen.go  — Type() method implementations
//
// Phase 2: Scans -game for DealEventXXX methods, generates:
//   - <receiver>_event_gen.go — InitSub() + SyncHandleEvent()
package eventgen

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tjbdwanghaibo/roost-codegen/internal/project"
)

// Default locations of the event definitions and the generated package inside a
// business project; `roost generate` refers to these (U-0118).
const (
	DefaultDefDir = "./event/def"
	DefaultOutDir = "./event"
)

func run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("eventgen", flag.ContinueOnError)
	flags.SetOutput(stdout)
	defDir := flags.String("def", DefaultDefDir, "directory containing event definitions")
	outDir := flags.String("out", DefaultOutDir, "output directory for generated event code")
	pkg := flags.String("pkg", "event", "generated package name")
	gameDir := flags.String("game", "", "game directory to scan for DealEventXXX handlers")
	eventPkg := flags.String("eventpkg", "", "import path for event package (default: derive from -out)")
	force := flags.Bool("force", false, "force regeneration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments %q", flags.Args())
	}

	absDefDir, err := filepath.Abs(*defDir)
	if err != nil {
		return fmt.Errorf("resolve def dir: %w", err)
	}
	absOutDir, err := filepath.Abs(*outDir)
	if err != nil {
		return fmt.Errorf("resolve out dir: %w", err)
	}
	if err := os.MkdirAll(absOutDir, 0755); err != nil {
		return fmt.Errorf("create out dir: %w", err)
	}
	if *eventPkg == "" {
		info, err := project.Discover(absOutDir)
		if err != nil {
			return fmt.Errorf("discover event module: %w", err)
		}
		*eventPkg, err = info.ImportPath(absOutDir)
		if err != nil {
			return fmt.Errorf("derive event import: %w", err)
		}
	}

	// Phase 1: generate event types
	events, err := parseEventDir(absDefDir)
	if err != nil {
		return fmt.Errorf("parse events: %w", err)
	}

	if len(events) == 0 {
		fmt.Fprintf(stdout, "no event structs found in %s\n", absDefDir)
		return nil
	}

	defFile := filepath.Join(absOutDir, "event_def_gen.go")
	changed, err := generateDefs(events, *pkg, defFile, *force)
	if err != nil {
		return fmt.Errorf("generate defs: %w", err)
	}
	if changed {
		fmt.Fprintf(stdout, "generated: %s\n", defFile)
	} else {
		fmt.Fprintf(stdout, "unchanged: %s\n", defFile)
	}

	typeFile := filepath.Join(absOutDir, "event_type_gen.go")
	changed, err = generateTypes(events, *pkg, typeFile, *force)
	if err != nil {
		return fmt.Errorf("generate types: %w", err)
	}
	if changed {
		fmt.Fprintf(stdout, "generated: %s\n", typeFile)
	} else {
		fmt.Fprintf(stdout, "unchanged: %s\n", typeFile)
	}

	typeImplFile := filepath.Join(absOutDir, "event_type_impl_gen.go")
	changed, err = generateTypeImpl(events, *pkg, typeImplFile, *force)
	if err != nil {
		return fmt.Errorf("generate type impl: %w", err)
	}
	if changed {
		fmt.Fprintf(stdout, "generated: %s\n", typeImplFile)
	} else {
		fmt.Fprintf(stdout, "unchanged: %s\n", typeImplFile)
	}

	// Phase 2: generate handler dispatch code
	if *gameDir != "" {
		absGameDir, err := filepath.Abs(*gameDir)
		if err != nil {
			return fmt.Errorf("resolve game dir: %w", err)
		}
		if err := scanGameDirTo(absGameDir, *eventPkg, *force, declaredEventsOf(events), stdout); err != nil {
			return fmt.Errorf("scan game dir: %w", err)
		}
	}
	return nil
}
