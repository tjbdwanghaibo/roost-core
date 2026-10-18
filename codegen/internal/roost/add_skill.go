package roost

import (
	"fmt"
	"go/format"
	"os"
	"path/filepath"
)

func addSkillDefinition(root string, manifest Manifest, options AddOptions) ([]string, error) {
	snake := toSnake(options.Name)
	directory := filepath.Join(root, "game", "skills")
	definitionPath := filepath.Join(directory, snake+".json")
	if _, err := os.Stat(definitionPath); err == nil {
		return nil, fmt.Errorf("%s already exists", relativeSlash(root, definitionPath))
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	definition := fmt.Sprintf(`{
  "schema": "roost.skill/v2",
  "id": "skill.%s.%s",
  "name": %q,
  "description": "TODO: describe the gameplay contract.",
  "activation": {"type": "active", "policy": {"mode": "tap"}},
  "input_schema": {"type": "none"},
  "cooldown_ticks": 0,
  "costs": [],
  "memory": {},
  "initial_phase": "cast",
  "phases": [{
    "id": "cast",
    "timeout_ticks": 0,
    "on": {"enter": {"flow": "finish", "reason": "done"}}
  }]
}
`, manifest.Project.Name, snake, toPascal(options.Name))
	paths := []string{relativeSlash(root, definitionPath)}
	changes := []syncChange{{rel: relativeSlash(root, definitionPath), path: definitionPath, body: []byte(definition)}}
	catalogPath := filepath.Join(directory, "catalog.go")
	if _, err := os.Stat(catalogPath); os.IsNotExist(err) {
		catalog, formatErr := format.Source([]byte(renderSkillCatalog()))
		if formatErr != nil {
			return nil, fmt.Errorf("format Skill catalog: %w", formatErr)
		}
		changes = append(changes, syncChange{rel: relativeSlash(root, catalogPath), path: catalogPath, body: catalog})
		paths = append(paths, relativeSlash(root, catalogPath))
	} else if err != nil {
		return nil, err
	}
	// 目录的测试与 catalog.go 一样归业务、只建一次：已有 catalog.go 而还没有测试的工程（此前生成的）在这里补上。
	testPath := filepath.Join(directory, "catalog_test.go")
	if _, err := os.Stat(testPath); os.IsNotExist(err) {
		test, formatErr := format.Source([]byte(renderSkillCatalogTest()))
		if formatErr != nil {
			return nil, fmt.Errorf("format Skill catalog test: %w", formatErr)
		}
		changes = append(changes, syncChange{rel: relativeSlash(root, testPath), path: testPath, body: test})
		paths = append(paths, relativeSlash(root, testPath))
	} else if err != nil {
		return nil, err
	}
	if err := commitSyncChanges(changes); err != nil {
		return nil, err
	}
	return paths, nil
}

func renderSkillCatalog() string {
	return `package skills

import (
	"embed"
	"fmt"
	"path/filepath"

	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
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
`
}

// renderSkillCatalogTest 是技能目录的测试：用框架的正式加载路径（CompileAll → skill.Parse / skill.Compile）编译
// game/skills 下的全部 JSON，写坏的定义在 go test 与 doctor 的 skills:catalog 项就失败。工程用自己的编译环境
// （自定义 Host 能力表、catalog）时，把这里的环境换成启动时用的那一个。
func renderSkillCatalogTest() string {
	return `package skills

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
)

// TestSkillCatalogCompiles compiles every game/skills/*.json the way startup
// does. Use the same CompileEnvironment as the service when it is not the
// default one (custom Host capabilities or catalog).
func TestSkillCatalogCompiles(t *testing.T) {
	catalog, err := CompileAll(skill.DefaultCompileEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Programs) == 0 {
		t.Fatal("game/skills has no Skill definitions")
	}
}
`
}
