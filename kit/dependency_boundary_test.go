package kit_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Kit is the assembly layer: it may import core and third-party drivers, but
// never codegen (a build-time tool), another framework identity, or the two
// repositories that were folded into core / kit (their old import paths must
// not survive anywhere). Every Go file in the root module is parsed, tests and
// inactive build tags included; nested modules are separate consumers.
func TestKitDependencyBoundary(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "vendor" {
				return filepath.SkipDir
			}
			if path != "." {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				} else if !os.IsNotExist(err) {
					return err
				}
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			name, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if forbiddenKitImport(name) {
				t.Errorf("%s: forbidden Kit dependency %s", path, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func forbiddenKitImport(name string) bool {
	const owner = "github.com/tjbdwanghaibo/"
	if strings.HasPrefix(name, owner+"cube-") {
		return true
	}
	for _, module := range []string{"roost-codegen", "roost-skill", "roost-service"} {
		if name == owner+module || strings.HasPrefix(name, owner+module+"/") {
			return true
		}
	}
	return false
}

func TestForbiddenKitImport(t *testing.T) {
	for _, name := range []string{
		"github.com/tjbdwanghaibo/roost-codegen/internal/roost",
		"github.com/tjbdwanghaibo/roost-skill/skill",
		"github.com/tjbdwanghaibo/roost-service/mail",
		"github.com/tjbdwanghaibo/cube-kit/redis",
	} {
		if !forbiddenKitImport(name) {
			t.Errorf("accepted forbidden import %s", name)
		}
	}
	for _, name := range []string{"context", "github.com/tjbdwanghaibo/roost-core/redis", "github.com/tjbdwanghaibo/roost-kit/mods", "github.com/redis/go-redis/v9"} {
		if forbiddenKitImport(name) {
			t.Errorf("rejected allowed import %s", name)
		}
	}
}
