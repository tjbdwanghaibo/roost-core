package skill

import "sort"

type ActivePresentationKind string

const (
	ActivePresentationCast  ActivePresentationKind = "cast"
	ActivePresentationSpawn ActivePresentationKind = "spawn"
)

type ActivePresentation struct {
	Kind               ActivePresentationKind `json:"kind"`
	ProgramID          string                 `json:"program_id"`
	GameplayDigest     string                 `json:"gameplay_digest"`
	PresentationDigest string                 `json:"presentation_digest"`
	VisualIndex        VisualIndex            `json:"visual_index"`
	CastID             CastID                 `json:"cast_id,omitempty"`
	SpawnID            SpawnID                `json:"spawn_id,omitempty"`
	SpawnTemplate      SpawnTemplateIndex     `json:"spawn_template,omitempty"`
	CastStatus         CastStatus             `json:"cast_status,omitempty"`
	SpawnStatus        SpawnStatus            `json:"spawn_status,omitempty"`
	Anchor             PresentationAnchor     `json:"anchor"`
	// PrimaryTarget 是 Runtime 为这条持续表现发出的增量事件里的 PrimaryTarget：施法条目与仍归施法的衍生物是施法目标，
	// 移交后的衍生物是 lifecycle 实体（detachedSpawnCast）。只供 reset 按增量的形状做可见性过滤，不下发。
	// 之前 reset 用 Anchor.Target 代替它，仍归施法的衍生物两者不同，按 PrimaryTarget 判定的策略对 reset 与增量
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
	// 待停止的衍生物仍在宿主侧运行，表现保留到真正停掉（增量里进入待停止时发过一条带 stop_pending 的 spawn_update）。
	runtime.spawns.each(func(spawn *SpawnInstance) {
		if spawn.Program == nil || int(spawn.TemplateIndex) >= len(spawn.Program.spawnTemplates) {
			return
		}
		template := spawn.Program.spawnTemplates[spawn.TemplateIndex]
		if !template.hasVisual {
			return
		}
		position, direction := spawn.Motion.Position, spawn.Motion.Direction
		result.Active = append(result.Active, ActivePresentation{
			Kind: ActivePresentationSpawn, ProgramID: spawn.Program.id,
			GameplayDigest: spawn.Program.identity.gameplayDigest, PresentationDigest: spawn.Program.identity.presentationDigest,
			VisualIndex: template.visual, CastID: spawn.CastID, SpawnID: spawn.ID,
			SpawnTemplate: spawn.TemplateIndex, SpawnStatus: spawn.Status,
			Anchor:        PresentationAnchor{Source: spawn.Owner, Target: spawn.LifecycleEntity, Position: &position, Direction: &direction},
			PrimaryTarget: runtime.spawnPresentationTargetLocked(spawn),
		})
	}, spawnLivePartitions...)
	sort.Slice(result.Active, func(i, j int) bool {
		if result.Active[i].Kind != result.Active[j].Kind {
			return result.Active[i].Kind < result.Active[j].Kind
		}
		if result.Active[i].CastID != result.Active[j].CastID {
			return result.Active[i].CastID < result.Active[j].CastID
		}
		return result.Active[i].SpawnID < result.Active[j].SpawnID
	})
	return result
}

// spawnPresentationTargetLocked 返回 emitSpawnPresentation 给这个衍生物的事件填的 PrimaryTarget：未移交的衍生物
// 经所属施法发出（appendPresentation 用 cast.primaryTarget），移交后经 detachedSpawnCast 发出（lifecycle 实体）。
func (runtime *Runtime) spawnPresentationTargetLocked(spawn *SpawnInstance) EntityID {
	if !spawn.handedOff {
		if cast := runtime.casts[spawn.CastID]; cast != nil {
			return cast.primaryTarget
		}
	}
	return spawn.LifecycleEntity
}
