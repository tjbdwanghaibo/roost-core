package skill

type visualProgram struct {
	index    VisualIndex
	category string
	theme    string
	elements []string
}

type VisualView struct {
	Index    VisualIndex
	Category string
	Theme    string
	Elements []string
}

type SkillVisualManifest struct {
	Digest          string
	CatalogRevision string
	CatalogDigest   string
	Entries         []VisualView
}
