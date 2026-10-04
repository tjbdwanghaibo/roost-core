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

// noCollectionKeySuffix is the constant the DAO generator writes for a
// `//roost:dao nocoll` DAO (<Dao>RegistryKey) in place of <Dao>Collection.
// It is the contract between the two generators: its presence is how this
// generator recognizes a DAO that has no collection.
const noCollectionKeySuffix = "RegistryKey"

// resolveNoCollectionDaos marks the entity's nocoll DAOs and refuses an entity
// that would need them to be stored (W-2026-09-18-09, R4 of
// docs/feature/DAO-NO-COLLECTION-2026-10-04.md).
//
// A nocoll DAO has no collection, database or persistence methods. Only a
// noPersist entity may use one: a persistent entity loads and deletes every
// DAO through storage, and remote=managed commits every DAO through
// MarshalPersist. Like validateRemoteDaoScopes it reads the DAO package's
// generated source inside the entity's module; a DAO outside the module cannot
// be inspected here and keeps the <Dao>Collection wiring, which a nocoll DAO
// does not define, so that combination still fails at compile time.
func resolveNoCollectionDaos(ent *EntityDef, dir string) error {
	for i := range ent.Daos {
		dao := &ent.Daos[i]
		pkgDir, typeName := daoSourceDir(*ent, dir, dao.TypeName)
		if pkgDir == "" {
			continue
		}
		noCollection, err := declaresConst(pkgDir, typeName+noCollectionKeySuffix)
		if err != nil {
			return fmt.Errorf("entity %s: inspect DAO %s: %w", ent.Name, dao.TypeName, err)
		}
		if !noCollection {
			continue
		}
		if !ent.NoPersist {
			return fmt.Errorf("entity %s: DAO field %s (%s) is declared //roost:dao nocoll and is never stored, but the entity is persistent; mark the entity noPersist=true, or give the DAO coll= and db= and regenerate",
				ent.Name, dao.FieldName, dao.TypeName)
		}
		if ent.RemotePolicy == "entity.RemotePolicyManaged" {
			return fmt.Errorf("entity %s: DAO field %s (%s) is declared //roost:dao nocoll, but remote=managed commits and loads every DAO through storage; drop remote=managed, or give the DAO coll= and db= and regenerate",
				ent.Name, dao.FieldName, dao.TypeName)
		}
		dao.NoCollection = true
	}
	return nil
}

// declaresConst reports whether a non-test file in pkgDir declares a
// package-level constant with the given name.
func declaresConst(pkgDir, name string) (bool, error) {
	entries, err := os.ReadDir(pkgDir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		file := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(pkgDir, file), nil, parser.SkipObjectResolution)
		if err != nil {
			return false, err
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				for _, ident := range spec.(*ast.ValueSpec).Names {
					if ident.Name == name {
						return true, nil
					}
				}
			}
		}
	}
	return false, nil
}
