package skill

type MemoryIndex uint16
type LocalIndex uint16
type PhaseIndex uint16
type RootIndex uint16
type SelectorIndex uint16
type OperationIndex uint32
type EffectIndex uint32
type ProcessTemplateIndex uint16
type VisualIndex uint16
type RandomSiteIndex uint16

type programIdentity struct {
	sourceDocumentDigest string
	gameplayDigest       string
	presentationDigest   string
}

type ProgramIdentityView struct {
	SourceDocumentDigest string
	GameplayDigest       string
	PresentationDigest   string
}
