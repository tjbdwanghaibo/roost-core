package roost

import (
	"fmt"
	"go/format"
	"os"
	"path/filepath"
)

// ModuleSaga 声明编排顺序、确切路由步骤和保存原生回执的实体。
// 模块提供业务 handler 与 Steps 注册；这里不生成无人接线的第二套步骤桩。
type ModuleSaga struct {
	Name     string   `yaml:"name" json:"name"`
	Steps    []string `yaml:"steps" json:"steps"`
	Routed   []string `yaml:"routed" json:"routed,omitempty"`
	Receipts []string `yaml:"receipts" json:"receipts,omitempty"`
}

func moduleSagaDefinition(name string) string { return "saga/" + name + "/definition.go" }

func validateModuleSagas(spec ModuleSpec) error {
	if len(spec.Sagas) == 0 {
		return nil
	}
	if !contains(spec.Needs.Mods, "saga") || spec.Runtime == nil {
		return fmt.Errorf("module %s: sagas require the saga mod and a runtime entrypoint", spec.Name)
	}
	seen := map[string]bool{}
	for _, definition := range spec.Sagas {
		if !validName(definition.Name) || seen[definition.Name] {
			return fmt.Errorf("module %s: invalid or duplicate saga %q", spec.Name, definition.Name)
		}
		seen[definition.Name] = true
		if _, err := parseSagaSteps(definition.Name, definition.Steps, nil, definition.Routed); err != nil {
			return err
		}
		if contains(spec.Files, moduleSagaDefinition(definition.Name)) {
			return fmt.Errorf("module %s: saga definition is supplied by the installer", spec.Name)
		}
		for _, host := range definition.Receipts {
			if !modulePascalPattern.MatchString(host) {
				return fmt.Errorf("saga %s: invalid receipt entity %q", definition.Name, host)
			}
		}
	}
	return nil
}

func checkModuleSagaConflicts(root string, manifest Manifest, spec ModuleSpec) error {
	for _, definition := range spec.Sagas {
		path := moduleSagaDefinition(definition.Name)
		if contains(manifest.Sagas, definition.Name) {
			return fmt.Errorf("module %s: saga %s already belongs to the project", spec.Name, definition.Name)
		}
		if _, err := os.Stat(filepath.Join(root, path)); err == nil {
			return fmt.Errorf("module %s: %s already exists", spec.Name, path)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// 共享 DAO 不算模块独占文件，但必须纳入同一安装回滚边界。
func moduleSagaTouchedFiles(root string, spec ModuleSpec, newEntities map[string]bool) ([]string, error) {
	var paths []string
	for _, definition := range spec.Sagas {
		for _, host := range definition.Receipts {
			paths = append(paths, sagaReceiptsFile(toSnake(host)))
			if newEntities[host] {
				paths = append(paths, moduleEntityDefinitions(host)...)
				continue
			}
			target := sagaHandlerTarget{entity: toSnake(host), entityPascal: host}
			if err := resolveSagaReceiptHost(root, definition.Name, &target); err != nil {
				return nil, err
			}
			source, err := locateDaoStruct(root, target.dao)
			if err != nil {
				return nil, err
			}
			if source == nil {
				return nil, fmt.Errorf("saga %s: receipt DAO %s is missing", definition.Name, target.dao)
			}
			paths = append(paths, relativeSlash(root, source.path))
		}
	}
	return uniqueSorted(paths), nil
}

func installModuleSagas(root string, manifest *Manifest, spec ModuleSpec) error {
	for _, definition := range spec.Sagas {
		steps, err := parseSagaSteps(definition.Name, definition.Steps, nil, definition.Routed)
		if err != nil {
			return err
		}
		body, err := format.Source([]byte(renderSagaDefinition(definition.Name, steps)))
		if err != nil {
			return err
		}
		if err := writeAtomic(filepath.Join(root, moduleSagaDefinition(definition.Name)), body, 0o644); err != nil {
			return err
		}
		for _, host := range definition.Receipts {
			target := sagaHandlerTarget{entity: toSnake(host), entityPascal: host}
			if err := resolveSagaReceiptHost(root, definition.Name, &target); err != nil {
				return err
			}
			changes, err := planSagaReceipts(root, manifest.Project.Module, target)
			if err != nil {
				return err
			}
			if err := commitSyncChanges(changes); err != nil {
				return err
			}
		}
		manifest.Sagas = uniqueSorted(append(manifest.Sagas, definition.Name))
		manifest.Features = uniqueSorted(append(manifest.Features, "saga"))
	}
	return nil
}
