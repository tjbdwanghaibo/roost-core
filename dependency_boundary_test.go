package roostcore_test

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

// Parse all root-module source files, including tests and inactive build tags.
// Nested modules are separate consumers, not part of Core's dependency layer.
func TestCoreDependencyBoundary(t *testing.T) {
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
			if forbiddenCoreImport(name) {
				t.Errorf("%s: forbidden Core dependency %s", path, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// driverPackages are the client implementations split out of their contract
// packages (mongo, nats, redis, etcd). Contract packages stay free of driver
// dependencies only if nothing inside Core links a driver: assembly happens
// in kit's Mods. Tests may use them freely.
var driverPackages = []string{
	"github.com/tjbdwanghaibo/roost-core/mongo/driver",
	"github.com/tjbdwanghaibo/roost-core/nats/driver",
	"github.com/tjbdwanghaibo/roost-core/redis/driver",
	"github.com/tjbdwanghaibo/roost-core/etcd/driver",
}

// TestCoreContractsDoNotLinkDrivers walks every non-test Go file outside the
// driver packages and refuses an import of a driver package. Keeping the
// contracts light is the reason the drivers live in subpackages at all
// (B-26): a binary that only wants IMongo must not pull in TLS and SCRAM.
func TestCoreContractsDoNotLinkDrivers(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "vendor" || entry.Name() == "driver" {
				return filepath.SkipDir
			}
			if path != "." {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
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
			for _, driver := range driverPackages {
				if name == driver {
					t.Errorf("%s: contract-side code links driver package %s; construct clients in kit's Mod instead", path, name)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func forbiddenCoreImport(name string) bool {
	const owner = "github.com/tjbdwanghaibo/"
	if strings.HasPrefix(name, owner+"cube-") {
		return true
	}
	for _, module := range []string{"roost-kit", "roost-skill", "roost-service", "roost-codegen"} {
		if name == owner+module || strings.HasPrefix(name, owner+module+"/") {
			return true
		}
	}
	return false
}

func TestForbiddenCoreImport(t *testing.T) {
	for _, name := range []string{
		"github.com/tjbdwanghaibo/roost-kit/mongo/mongotest",
		"github.com/tjbdwanghaibo/roost-service",
		"github.com/tjbdwanghaibo/roost-skill/skill",
		"github.com/tjbdwanghaibo/roost-codegen/cmd/roost",
		"github.com/tjbdwanghaibo/cube-core/entity",
	} {
		if !forbiddenCoreImport(name) {
			t.Errorf("accepted forbidden import %s", name)
		}
	}
	for _, name := range []string{"context", "github.com/tjbdwanghaibo/roost-core/entity", "go.mongodb.org/mongo-driver/v2/mongo"} {
		if forbiddenCoreImport(name) {
			t.Errorf("rejected allowed import %s", name)
		}
	}
}
