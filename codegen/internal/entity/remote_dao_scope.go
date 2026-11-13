package entity

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// validateRemoteDaoScopes 拒绝 remote=managed 实体使用 dbscope=sid 的 DAO。
//
// 托管实体可由任一进程写入、所有权可迁移，而 Remote 提交按提交方 sid 选库、加载按本服 sid
// 选库；按服选库会让实体在跨服写或迁移后读到旧版本 / 缺失而不可写。DAO 的作用域由 DAO
// 生成器写成 DbScope 方法，这里读取实体所在模块内 DAO 包的源码判定；DAO 位于模块外、
// 源码不可读或 DbScope 不是直接返回常量时无法在生成期判定，由运行时装配校验
// （remoteentity.Assemble / Start）兜底。
func validateRemoteDaoScopes(ent EntityDef, dir string) error {
	if ent.RemotePolicy != "entity.RemotePolicyManaged" {
		return nil
	}
	for _, dao := range ent.Daos {
		pkgDir, typeName := daoSourceDir(ent, dir, dao.TypeName)
		if pkgDir == "" {
			continue
		}
		file, err := serverScopedDaoFile(pkgDir, typeName)
		if err != nil {
			return fmt.Errorf("entity %s: inspect DAO %s scope: %w", ent.Name, dao.TypeName, err)
		}
		if file != "" {
			return fmt.Errorf("entity %s: remote=managed DAO field %s (%s) uses dbscope=sid (%s); a remote-managed entity is committed and loaded from one shared database by every process, so declare the DAO with dbscope=global (//roost:dao ... dbscope=global) and regenerate",
				ent.Name, dao.FieldName, dao.TypeName, file)
		}
	}
	return nil
}

// validateLocalPreloadDaoScopes 拒绝本地实体（不是 remote=managed）写了 preload=startup 却有 dbscope=global 的 DAO
// （维护者 2026-11-13：“local 就是 dbscope=sid”）。本地预加载加载的是“本服库里这个 kind 的文档”：物理库 <库>_<sid>
// 本身就按服区分，所以每个 DAO 都必须按服选库；global 的库是各服共用的，按它枚举会把别的服的实体加载进来。
// 与 validateRemoteDaoScopes 一样读 DAO 包的源码判定；读不到（模块外、DbScope 不是直接返回常量）时不在这里判，
// 由运行期 wiring/dataengine.LocalPreloadMod 的 Init（engine.ValidateLocalPreloadKinds）兜底。
func validateLocalPreloadDaoScopes(ent EntityDef, dir string) error {
	if !ent.Preload || ent.RemotePolicy == "entity.RemotePolicyManaged" {
		return nil
	}
	for _, dao := range ent.Daos {
		pkgDir, typeName := daoSourceDir(ent, dir, dao.TypeName)
		if pkgDir == "" {
			continue
		}
		scope, file, err := declaredDaoScope(pkgDir, typeName)
		if err != nil {
			return fmt.Errorf("entity %s: inspect DAO %s scope: %w", ent.Name, dao.TypeName, err)
		}
		if scope == "global" {
			return fmt.Errorf("entity %s: %spreload=startup loads this server's stored %s entities, but DAO field %s (%s, %s) uses dbscope=global, a database every server shares; "+
				"a local entity's DAOs are dbscope=sid (//roost:dao ... dbscope=sid, one database per server), so change the DAO to dbscope=sid and regenerate, or drop preload=startup",
				ent.Name, residentNote(ent), ent.Name, dao.FieldName, dao.TypeName, file)
		}
	}
	return nil
}

// daoSourceDir 返回 DAO 类型所在包的目录与不带包限定的类型名：同包类型就是实体目录；
// 带限定的类型按实体的 import 映射到模块内目录，模块外返回空目录（生成期无法读取源码）。
func daoSourceDir(ent EntityDef, dir, daoType string) (string, string) {
	typeName := derefType(daoType)
	alias := qualifier(typeName)
	if alias == "" {
		return dir, typeName
	}
	typeName = strings.TrimPrefix(typeName, alias+".")
	for _, imp := range ent.Imports {
		if imp.Alias == alias {
			return moduleDirForImport(dir, imp.Path), typeName
		}
	}
	return "", typeName
}

// moduleDirForImport 把实体所在模块内的 import path 映射到目录；模块外返回空串。
func moduleDirForImport(start, importPath string) string {
	for dir := start; ; {
		if raw, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
			for _, line := range strings.Split(string(raw), "\n") {
				line = strings.TrimSpace(line)
				if !strings.HasPrefix(line, "module ") {
					continue
				}
				module := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "module ")), `"`)
				switch {
				case importPath == module:
					return dir
				case strings.HasPrefix(importPath, module+"/"):
					return filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(importPath, module+"/")))
				}
				return ""
			}
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// serverScopedDaoFile 在 pkgDir 的非测试源码中查找 typeName 的 DbScope 方法；
// 方法直接返回 DatabaseServer 时返回所在文件名，否则返回空串。
func serverScopedDaoFile(pkgDir, typeName string) (string, error) {
	scope, file, err := declaredDaoScope(pkgDir, typeName)
	if err != nil || scope != "sid" {
		return "", err
	}
	return file, nil
}

// declaredDaoScope 读 pkgDir 里 typeName 的 DbScope 方法：直接返回 DatabaseServer 为 "sid"、DatabaseGlobal 为 "global"，
// 并给出所在文件名；找不到方法或不是直接返回常量时 scope 为 ""（判断不了）。
func declaredDaoScope(pkgDir, typeName string) (scope, file string, err error) {
	entries, err := os.ReadDir(pkgDir)
	if os.IsNotExist(err) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(pkgDir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return "", "", err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "DbScope" || fn.Recv == nil || len(fn.Recv.List) != 1 || fn.Body == nil {
				continue
			}
			if receiverTypeName(fn.Recv.List[0].Type) != typeName {
				continue
			}
			switch returnedScope(fn.Body) {
			case "DatabaseServer":
				return "sid", name, nil
			case "DatabaseGlobal":
				return "global", name, nil
			}
			return "", "", nil
		}
	}
	return "", "", nil
}

// returnedScope 返回 DbScope 方法体直接返回的常量名（DatabaseServer / DatabaseGlobal），不是这种写法时为 ""。
func returnedScope(body *ast.BlockStmt) string {
	if len(body.List) != 1 {
		return ""
	}
	ret, ok := body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return ""
	}
	switch value := ret.Results[0].(type) {
	case *ast.SelectorExpr:
		return value.Sel.Name
	case *ast.Ident:
		return value.Name
	}
	return ""
}

// residentNote 在 lifetime=resident 的实体上说明预加载从哪来（它隐含 preload=startup）。
func residentNote(ent EntityDef) string {
	if ent.Lifetime == "entity.EntityLifetimeResident" {
		return "lifetime=resident implies "
	}
	return ""
}
