package skills

import (
	"embed"
	"fmt"
	"path/filepath"

	"github.com/tjbdwanghaibo/roost-core/skill"
)

// definitions are application-owned Skill wire documents. They are parsed and
// compiled during startup; invalid content fails closed before traffic enters.
//
//go:embed *.json
var definitions embed.FS

type Catalog struct {
	Programs    map[string]*skill.Program
	Diagnostics map[string][]skill.Diagnostic
}

func CompileAll(environment skill.CompileEnvironment) (*Catalog, error) {
	entries, err := definitions.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("read Skill catalog: %w", err)
	}
	result := &Catalog{
		Programs:    make(map[string]*skill.Program, len(entries)),
		Diagnostics: make(map[string][]skill.Diagnostic, len(entries)),
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		raw, err := definitions.ReadFile(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read Skill %s: %w", entry.Name(), err)
		}
		definition, err := skill.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("parse Skill %s: %w", entry.Name(), err)
		}
		program, diagnostics := skill.Compile(definition, environment)
		result.Diagnostics[entry.Name()] = append([]skill.Diagnostic(nil), diagnostics...)
		for _, diagnostic := range diagnostics {
			if diagnostic.Severity == skill.DiagnosticError {
				return nil, fmt.Errorf("compile Skill %s: %s at %s: %s", entry.Name(), diagnostic.Code, diagnostic.Path, diagnostic.Message)
			}
		}
		if program == nil {
			return nil, fmt.Errorf("compile Skill %s: compiler returned no Program", entry.Name())
		}
		id := skill.Inspect(program).ID
		if _, exists := result.Programs[id]; exists {
			return nil, fmt.Errorf("duplicate Skill id %q", id)
		}
		result.Programs[id] = program
	}
	return result, nil
}
