package roost

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 服务包名（DESIGN-TODO D8，维护者 2026-10-16 决定 C）。
//
// internal/service/<name>/ 下的文件分两种所有者：service.go、managers.go、collaborators.go 归业务，
// 生成一次后不再改写；framework_clients_gen.go 归生成器，每次 sync 重写。同一目录只能有一个包名，
// 所以生成器不能自己决定包名，只能跟随目录：
//   - 目录里已有 Go 文件（已有工程）：沿用它们的包名，优先取业务文件的（生成器从不改写它们）；
//   - 目录还不存在（新工程、新服务）：按 Go 惯例取小写名 defaultServicePackage。
// 已有工程不迁移、不改写业务文件：大写包名的老工程继续是大写，新工程是小写。

// defaultServicePackage 把服务名（roost.yaml 的键，满足 ^[a-z][a-z0-9_]*$）变成 Go 包名：
// 去掉下划线与连字符、全小写（"game" → "game"，"world_boss" → "worldboss"）；结果是 Go 关键字时
// 加后缀 "service"（"select" → "selectservice"），保证 package 子句合法。
func defaultServicePackage(name string) string {
	pkg := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(name))
	if token.IsKeyword(pkg) {
		pkg += "service"
	}
	return pkg
}

// servicePackage 是生成 internal/service/<name>/ 下文件时用的包名。
func (m Manifest) servicePackage(name string) string {
	if pkg, ok := m.servicePackages[name]; ok {
		return pkg
	}
	return defaultServicePackage(name)
}

// withServicePackages 读出 root 里每个服务目录现有的包名。读不出（目录不存在、没有 Go 文件、
// 文件解析失败）的服务留给 defaultServicePackage；解析失败的工程反正编译不过，由 build / doctor 报告。
func (m Manifest) withServicePackages(root string) Manifest {
	packages := make(map[string]string, len(m.Services))
	for name := range m.Services {
		if pkg, ok := existingServicePackage(filepath.Join(root, "internal", "service", name)); ok {
			packages[name] = pkg
		}
	}
	m.servicePackages = packages
	return m
}

// existingServicePackage 返回目录里非测试 Go 文件的包名：先看业务文件（不带生成头），没有业务文件时
// 才看生成文件；同类文件按文件名排序取第一个。
func existingServicePackage(dir string) (string, bool) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return "", false
	}
	sort.Strings(paths)
	generated := ""
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, raw, parser.PackageClauseOnly)
		if err != nil || file.Name == nil {
			continue
		}
		if !strings.HasPrefix(string(raw), generatedHeader) {
			return file.Name.Name, true
		}
		if generated == "" {
			generated = file.Name.Name
		}
	}
	return generated, generated != ""
}
