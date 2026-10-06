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
	// PrimaryTarget 是 Runtime 为这条持续表现发出的增量事件里的 PrimaryTarget：施法条目与仍归施法的进程是施法目标，
	// 移交后的进程是 lifecycle 实体（detachedProcessCast）。只供 reset 按增量的形状做可见性过滤，不下发。
	// 之前 reset 用 Anchor.Target 代替它，仍归施法的进程两者不同，按 PrimaryTarget 判定的策略对 reset 与增量
	// 得出相反结论（RR-20261006-22）。
	PrimaryTarget EntityID `json:"-"`
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
			Anchor: PresentationAnchor{Source: cast.caster, Target: cast.primaryTarget}, PrimaryTarget: cast.primaryTarget,
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
			Anchor:        PresentationAnchor{Source: process.Owner, Target: process.LifecycleEntity, Position: &position, Direction: &direction},
			PrimaryTarget: runtime.processPresentationTargetLocked(process),
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

// processPresentationTargetLocked 返回 emitProcessPresentation 给这个进程的事件填的 PrimaryTarget：未移交的进程
// 经所属施法发出（appendPresentation 用 cast.primaryTarget），移交后经 detachedProcessCast 发出（lifecycle 实体）。
func (runtime *Runtime) processPresentationTargetLocked(process *ProcessInstance) EntityID {
	if !process.handedOff {
		if cast := runtime.casts[process.CastID]; cast != nil {
			return cast.primaryTarget
		}
	}
	return process.LifecycleEntity
}
