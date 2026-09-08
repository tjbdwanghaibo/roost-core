package skill

type TemporalCaptureCommand struct {
	Owner          EntityID
	Target         EntityID
	ProgramID      string
	GameplayDigest string
	Profile        TemporalProfileHandle
	Context        EventContext
}

type TemporalRestoreCommand struct {
	Owner          EntityID
	Target         EntityID
	ProgramID      string
	GameplayDigest string
	Token          SnapshotToken
	OnBlocked      string
	Context        EventContext
}

func (TemporalCaptureCommand) isEffectCommandPayload() {}
func (TemporalRestoreCommand) isEffectCommandPayload() {}
