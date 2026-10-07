//go:build protocoldef

package protocoldef

// SkillCatalogRequest asks which skills this server compiled at startup.
type SkillCatalogRequest struct{}

// SkillCatalogResponse lists the compiled skill ids (roost-core/skill
// programs, from game/skills/*.json) and how many compile warnings the
// catalog carries — a client or a release check can refuse a non-zero count.
type SkillCatalogResponse struct {
	Code     int32    `pb:"1"`
	Reason   string   `pb:"2"`
	Skills   []string `pb:"3"`
	Warnings int32    `pb:"4"`
}

//roost:protocol group=game handler=player
type SkillCatalogProtocol interface {
	//roost:msg id=10012
	SkillCatalog(SkillCatalogRequest) SkillCatalogResponse
}
