package roost

import (
	"bytes"
	"context"
	"fmt"
	"go/format"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/modules"
	"gopkg.in/yaml.v3"
)

// RecipeSpec 只描述组合，不另建一套模块安装器或生成归属。
// 配方文件在新建工程时写一次，后续 module add / project sync 不覆盖业务规则。
type RecipeSpec struct {
	Summary string   `yaml:"summary"`
	Modules []string `yaml:"modules"`
	Name    string   `yaml:"-"`
}

var recipePlaceholder = regexp.MustCompile(`\{\{[A-Z_]+(?::[A-Za-z0-9_:]+)?\}\}`)

func loadRecipes() (map[string]RecipeSpec, error) {
	raw, err := modules.Recipes.ReadFile("recipes/catalog.yaml")
	if err != nil {
		return nil, err
	}
	var catalog map[string]RecipeSpec
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&catalog); err != nil {
		return nil, fmt.Errorf("recipes: %w", err)
	}
	available, err := loadModuleCatalog()
	if err != nil {
		return nil, err
	}
	for name, recipe := range catalog {
		if !validName(strings.ReplaceAll(name, "-", "_")) || recipe.Summary == "" || len(recipe.Modules) == 0 {
			return nil, fmt.Errorf("recipe %q: name, summary and modules are required", name)
		}
		seen := map[string]bool{}
		for _, module := range recipe.Modules {
			if _, exists := available[module]; !exists || seen[module] {
				return nil, fmt.Errorf("recipe %s: unknown or duplicate module %q", name, module)
			}
			seen[module] = true
		}
		if !seen["enter"] {
			return nil, fmt.Errorf("recipe %s: enter is required for the login path", name)
		}
		recipe.Name = name
		catalog[name] = recipe
	}
	return catalog, nil
}

func resolveRecipe(options *NewOptions) (*RecipeSpec, error) {
	name := strings.TrimSpace(options.Recipe)
	if name == "" {
		return nil, nil
	}
	catalog, err := loadRecipes()
	if err != nil {
		return nil, err
	}
	recipe, ok := catalog[name]
	if !ok {
		names := make([]string, 0, len(catalog))
		for name := range catalog {
			names = append(names, name)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("unknown recipe %q; supported: %s", name, strings.Join(names, ", "))
	}
	if template := strings.TrimSpace(options.Template); template != "" && template != "game" {
		return nil, fmt.Errorf("recipe %s requires template game, got %q", name, template)
	}
	options.Template = "game"
	return &recipe, nil
}

// scaffoldRecipe 在 NewProject 的暂存树内安装全部模块再写规则，失败不会发布半个工程。
// 模块号、with 联动、生命周期、定时器和 self-sync 都复用正式安装器。
func scaffoldRecipe(ctx context.Context, root string, manifest Manifest, service string, recipe RecipeSpec) ([]string, error) {
	plan, err := AddModules(ctx, root, recipe.Modules, ModuleOptions{Service: service, deferDependencies: true})
	if err != nil {
		return nil, fmt.Errorf("recipe %s: %w", recipe.Name, err)
	}
	var written []string
	for _, item := range plan.Modules {
		written = append(written, item.Files...)
	}
	replacer := strings.NewReplacer("{{MODULE}}", manifest.Project.Module, "{{PROJECT}}", manifest.Project.Name, "{{GAME_SERVICE}}", service, "{{RECIPE}}", recipe.Name)
	for _, dir := range []string{"common", recipe.Name} {
		base := "recipes/" + dir
		err := fs.WalkDir(modules.Recipes, base, func(file string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			if !strings.HasSuffix(file, ".tmpl") {
				return fmt.Errorf("recipe source %s must end in .tmpl", file)
			}
			rel := strings.TrimSuffix(strings.TrimPrefix(file, base+"/"), ".tmpl")
			raw, err := modules.Recipes.ReadFile(file)
			if err != nil {
				return err
			}
			rendered, err := applyInstalledModuleIDs(root, rel, replacer.Replace(string(raw)))
			if err != nil {
				return err
			}
			body := []byte(rendered)
			if recipePlaceholder.Match(body) {
				return fmt.Errorf("recipe %s: unresolved placeholder in %s", recipe.Name, file)
			}
			if path.Ext(rel) == ".go" {
				body, err = format.Source(body)
				if err != nil {
					return fmt.Errorf("recipe %s: format %s: %w", recipe.Name, file, err)
				}
			}
			if err := writeAtomic(filepath.Join(root, filepath.FromSlash(rel)), body, 0o644); err != nil {
				return err
			}
			written = append(written, rel)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("recipe %s: %w", recipe.Name, err)
		}
	}
	// 配方已提供正式认证器，开发配置可打开 TCP；生产配置仍由部署者显式开启。
	if _, err := ensurePlayerTCPConfig(root, service, "", true); err != nil {
		return nil, err
	}
	if err := writeRecipeLoginDevConfig(root, service); err != nil {
		return nil, err
	}
	written = append(written, "configs/service/config.account.yaml")
	if err := generate(ctx, root, GenerateOptions{Stdout: io.Discard}); err != nil {
		return nil, fmt.Errorf("recipe %s: regenerate: %w", recipe.Name, err)
	}
	return written, nil
}
