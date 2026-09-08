package skill

import "sort"

type ActivePresentationKind string

const (
	ActivePresentationCast    ActivePresentationKind = "cast"
	ActivePresentationProcess ActivePresentationKind = "process"
)

type ActivePresentation struct {
	Kind               ActivePresentationKind `json:"kind"`
	ProgramID          string                 `json:"program_id"`
	GameplayDigest     string                 `json:"gameplay_digest"`
	PresentationDigest string                 `json:"presentation_digest"`
	VisualIndex        VisualIndex            `json:"visual_index"`
	CastID             CastID                 `json:"cast_id,omitempty"`
	ProcessID          ProcessID              `json:"process_id,omitempty"`
	ProcessTemplate    ProcessTemplateIndex   `json:"process_template,omitempty"`
	CastStatus         CastStatus             `json:"cast_status,omitempty"`
	ProcessStatus      ProcessStatus          `json:"process_status,omitempty"`
	Anchor             PresentationAnchor     `json:"anchor"`
}

// PresentationRecoverySnapshot is the authoritative set of continuing visual
// instances. Transient one-shot effects are intentionally not resurrected.
type PresentationRecoverySnapshot struct {
	Tick                       Tick                 `json:"tick"`
	WorldRevision              WorldRevision        `json:"world_revision"`
	LatestPresentationSequence uint64               `json:"latest_presentation_sequence"`
	Active                     []ActivePresentation `json:"active,omitempty"`
}

func (runtime *Runtime) PresentationSnapshot() PresentationRecoverySnapshot {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	revision := WorldRevision(0)
	if runtime.host != nil {
		revision = runtime.host.CurrentRevision()
	}
	result := PresentationRecoverySnapshot{Tick: runtime.currentTick, WorldRevision: revision, LatestPresentationSequence: runtime.presentationSequence}
	for _, cast := range runtime.casts {
		if cast == nil || cast.program == nil || !cast.program.hasCastVisual || (cast.status != CastRunning && cast.status != CastSuspended) {
			continue
		}
		result.Active = append(result.Active, ActivePresentation{
			Kind: ActivePresentationCast, ProgramID: cast.program.id,
			GameplayDigest: cast.program.identity.gameplayDigest, PresentationDigest: cast.program.identity.presentationDigest,
			VisualIndex: cast.program.castVisual, CastID: cast.id, CastStatus: cast.status,
			Anchor: PresentationAnchor{Source: cast.caster, Target: cast.primaryTarget},
		})
	}
	for _, process := range runtime.processes {
		if process == nil || process.Program == nil || process.Status != ProcessRunning || int(process.TemplateIndex) >= len(process.Program.processTemplates) {
			continue
		}
		template := process.Program.processTemplates[process.TemplateIndex]
		if !template.hasVisual {
			continue
		}
		position, direction := process.Motion.Position, process.Motion.Direction
		result.Active = append(result.Active, ActivePresentation{
			Kind: ActivePresentationProcess, ProgramID: process.Program.id,
			GameplayDigest: process.Program.identity.gameplayDigest, PresentationDigest: process.Program.identity.presentationDigest,
			VisualIndex: template.visual, CastID: process.CastID, ProcessID: process.ID,
			ProcessTemplate: process.TemplateIndex, ProcessStatus: process.Status,
			Anchor: PresentationAnchor{Source: process.Owner, Target: process.LifecycleEntity, Position: &position, Direction: &direction},
		})
	}
	sort.Slice(result.Active, func(i, j int) bool {
		if result.Active[i].Kind != result.Active[j].Kind {
			return result.Active[i].Kind < result.Active[j].Kind
		}
		if result.Active[i].CastID != result.Active[j].CastID {
			return result.Active[i].CastID < result.Active[j].CastID
		}
		return result.Active[i].ProcessID < result.Active[j].ProcessID
	})
	return result
}
