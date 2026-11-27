package registry

import (
	"fmt"
	"go/ast"
	"go/token"
	"path"
	"path/filepath"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/entityowner"
)

// 关系只在工程的总装配层生成。子包不需要知道主包路径，主包可正常依赖子包。
type companionType struct{ path, name, kind string }
type companionPair struct{ parent, child companionType }

func companionPairs(root, module string) ([]companionPair, error) {
	entities, err := entityowner.Entities(filepath.Join(root, "game"))
	if err != nil {
		return nil, err
	}
	byName := make(map[string][]entityowner.Owned)
	for _, e := range entities {
		byName[e.Entity] = append(byName[e.Entity], e)
	}
	ref := func(e entityowner.Owned) (companionType, error) {
		if !ast.IsExported(e.Entity) || !token.IsIdentifier(e.Kind) || !ast.IsExported(e.Kind) {
			return companionType{}, fmt.Errorf("%s: companionOf registration requires exported entity and package-local kind names (got %s, %s)", e.Position, e.Entity, e.Kind)
		}
		rel, err := filepath.Rel(root, e.Dir)
		if err != nil {
			return companionType{}, err
		}
		return companionType{path.Join(module, filepath.ToSlash(rel)), e.Entity, e.Kind}, nil
	}
	var pairs []companionPair
	for _, child := range entities { // Entities is ordered by package path, then struct name.
		if child.CompanionOf == "" {
			continue
		}
		parents := byName[child.CompanionOf]
		if len(parents) != 1 {
			return nil, fmt.Errorf("%s: companionOf=%s needs exactly one matching principal entity under game/entities (found %d)", child.Position, child.CompanionOf, len(parents))
		}
		parent := parents[0]
		if parent.CompanionOf != "" {
			return nil, fmt.Errorf("%s: companionOf=%s names an entity that is itself a companion; self, chains and cycles are not supported", child.Position, parent.Entity)
		}
		// 共享 kind 没有单一写权，伴生配对的挂接与联写都建立在独占写许可上：运行期 entity.RegisterCompanionKind 对任一端
		// 是共享 kind 一律返回 ErrCompanionKind。在生成期报出具体是哪一端，不能让工程编译通过、起服装配时才含糊失败。
		for _, end := range []entityowner.Owned{parent, child} {
			if end.Shared {
				return nil, fmt.Errorf("%s: companionOf=%s: %s is ownership=shared; a shared Remote kind cannot be a companion principal or companion (pairing needs exclusive write permits)", child.Position, parent.Entity, end.Entity)
			}
		}
		if parent.Service != "" && child.Service != "" && parent.Service != child.Service {
			return nil, fmt.Errorf("%s: companionOf=%s has incompatible service owners %q and %q", child.Position, parent.Entity, parent.Service, child.Service)
		}
		p, err := ref(parent)
		if err != nil {
			return nil, err
		}
		c, err := ref(child)
		if err != nil {
			return nil, err
		}
		pairs = append(pairs, companionPair{p, c})
	}
	return pairs, nil
}

func (r companionPair) registration(aliases map[string]string) call {
	p, c := aliases[r.parent.path]+".", aliases[r.child.path]+"."
	return call{
		Expr:         fmt.Sprintf("entity.RegisterCompanionKind[*%s%s, *%s%s](%s%s, %s%s)", p, r.parent.name, c, r.child.name, p, r.parent.kind, c, r.child.kind),
		Qualified:    "companionOf: " + r.child.path + "." + r.child.name + " -> " + r.parent.path + "." + r.parent.name,
		ReturnsError: true,
	}
}
