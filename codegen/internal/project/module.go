// Package project discovers Go module metadata needed by code generators.
package project

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

type Info struct {
	Root       string
	ModulePath string
}

func Discover(start string) (Info, error) {
	start, err := filepath.Abs(start)
	if err != nil {
		return Info{}, fmt.Errorf("resolve project directory: %w", err)
	}
	if stat, err := os.Stat(start); err != nil {
		return Info{}, err
	} else if !stat.IsDir() {
		start = filepath.Dir(start)
	}
	for dir := start; ; dir = filepath.Dir(dir) {
		goMod := filepath.Join(dir, "go.mod")
		if raw, err := os.ReadFile(goMod); err == nil {
			modulePath, err := modulePath(raw)
			if err != nil {
				return Info{}, fmt.Errorf("parse %s: %w", goMod, err)
			}
			return Info{Root: dir, ModulePath: modulePath}, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return Info{}, fmt.Errorf("go.mod not found from %s", start)
}

func (i Info) ImportPath(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(i.Root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("directory %s is outside module root %s", dir, i.Root)
	}
	if rel == "." {
		return i.ModulePath, nil
	}
	return i.ModulePath + "/" + filepath.ToSlash(rel), nil
}

func (i Info) PackageName(dir string) (string, error) {
	if _, err := i.ImportPath(dir); err != nil {
		return "", err
	}
	packages, err := parsePackages(dir, false)
	if err != nil {
		return "", err
	}
	if len(packages) == 0 {
		packages, err = parsePackages(dir, true)
		if err != nil {
			return "", err
		}
	}
	if len(packages) != 1 {
		return "", fmt.Errorf("expected one non-generated package in %s, found %d", dir, len(packages))
	}
	for name := range packages {
		return name, nil
	}
	return "", fmt.Errorf("no package found in %s", dir)
}

func parsePackages(dir string, includeGenerated bool) (map[string]*ast.Package, error) {
	set := token.NewFileSet()
	return parser.ParseDir(set, dir, func(info os.FileInfo) bool {
		name := info.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return false
		}
		return includeGenerated || (!strings.HasPrefix(name, "gen_") && !strings.HasSuffix(name, "_gen.go"))
	}, parser.PackageClauseOnly)
}

func modulePath(raw []byte) (string, error) {
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "module" {
			return strings.Trim(fields[1], `"`), nil
		}
	}
	return "", fmt.Errorf("module directive not found")
}
