package skillsync

// SKILL-3 补测（RR-20261005-NC-114 的 process 条目）：presentation reset 里的持续进程表现要按“Runtime 为这个进程发出的
// 增量事件”交给 VisibilityPolicy.FilterPresentation，reset 与增量对同一 observer 得出同样的可见性与同样的 Anchor。
// visibility_recovery_promises_test.go 只用了 cast 条目，process 条目此前没有专门用例。
//
// 进程有两种归属：仍在施法里（增量经施法发出，Source / PrimaryTarget 是施法者与施法目标），以及施法结束后移交出去
// （增量经 detachedProcessCast 发出，PrimaryTarget 是 lifecycle 实体）。两种都核对。
//
// RR-20261006-22：补测时“仍在施法里”是红的。activePresentationEvent 把 PrimaryTarget 填成 Anchor.Target（lifecycle
// 实体），而增量里是施法目标；按 PrimaryTarget 判定的策略挡住了增量，reset 却把同一个进程表现发了出去。

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/skill"
	"github.com/tjbdwanghaibo/roost-core/syncstream"
)

// visualAreaProcess 召出一个带 area 视觉的进程：lifecycle 实体是新生成的陷阱，不是施法目标。
const visualAreaProcess = `{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":8},"process":{"kind":"area","duration_ticks":8,"interval_ticks":1,"visual":{"category":"area","theme":"default","elements":["default"]},"area":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":10},"filters":[{"type":"targetable"}],"order":{"by":"stable_id","direction":"asc"},"limit":2}}}`

func visualProcessSkill(id, then string) string {
	return `{"schema":"roost.skill/v2","id":"` + id + `","name":"Visual Area","description":"A continuing process visual.","presentation":{"icon_keywords":["flare","blade","spark"]},"activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"sequence","steps":[` + visualAreaProcess + `,` + then + `]}}}]}`
}

// policyFunc 把一个函数当作 VisibilityPolicy，只用于 presentation。
type policyFunc func(syncstream.Observer, skill.PresentationEvent) (skill.PresentationEvent, bool, error)

func (policy policyFunc) FilterStateSnapshot(_ syncstream.Observer, snapshot skill.RuntimeStateSnapshot) (skill.RuntimeStateSnapshot, error) {
	return snapshot, nil
}
func (policy policyFunc) FilterStateMutation(_ syncstream.Observer, mutation skill.StateMutation) (skill.StateMutation, bool, error) {
	return mutation, true, nil
}
func (policy policyFunc) FilterPresentation(observer syncstream.Observer, event skill.PresentationEvent) (skill.PresentationEvent, bool, error) {
	return policy(observer, event)
}

func TestPresentationResetProcessEntryMatchesItsIncrementalEvent(t *testing.T) {
	for name, then := range map[string]string{
		"owned by the running cast": `{"flow":"wait","ticks":6,"then":{"flow":"finish"}}`,
		"handed off":                `{"flow":"finish"}`,
	} {
		t.Run(name, func(t *testing.T) {
			runtime, program := visibilityRuntimeFor(t, visualProcessSkill("skill.test.sync.visual_area", then), skill.RuntimeOptions{})
			if _, err := runtime.Start(program, skill.CastInput{Caster: 1, Target: 2}); err != nil {
				t.Fatal(err)
			}
			if err := runtime.Advance(1); err != nil {
				t.Fatal(err)
			}
			var latest skill.PresentationEvent
			for _, event := range runtime.PresentationEvents(0) {
				// start / update 带进程自身的 Anchor（signal 的 Anchor.Target 是被作用的实体）。
				if event.HasProcess && (event.Kind == skill.PresentationProcessStart || event.Kind == skill.PresentationProcessUpdate) {
					latest = event
				}
			}
			if latest.ProcessID == 0 {
				t.Fatalf("runtime emitted no process start / update: %+v", runtime.PresentationEvents(0))
			}
			lifecycle := latest.Anchor.Target
			if lifecycle == 0 || lifecycle == 2 {
				t.Fatalf("process lifecycle entity = %d, want the spawned trap (not the cast target 2)", lifecycle)
			}

			// 1. 交给策略的事件形状：除序号 / tick / 类型外与 Runtime 的增量一致。
			var seen []skill.PresentationEvent
			recorder := policyFunc(func(_ syncstream.Observer, event skill.PresentationEvent) (skill.PresentationEvent, bool, error) {
				seen = append(seen, event)
				return event, true, nil
			})
			reset := recoverPresentationReset(t, runtime, recorder)
			var given *skill.PresentationEvent
			for index := range seen {
				if seen[index].HasProcess && seen[index].ProcessID == latest.ProcessID {
					given = &seen[index]
				}
			}
			if given == nil || len(reset) != 1 {
				t.Fatalf("reset entries = %+v, policy saw %+v; want the one process entry", reset, seen)
			}
			if given.Source != latest.Source || given.PrimaryTarget != latest.PrimaryTarget || given.CastID != latest.CastID ||
				given.ProcessTemplate != latest.ProcessTemplate || given.ProcessStatus != latest.ProcessStatus || given.VisualIndex != latest.VisualIndex ||
				given.ProgramID != latest.ProgramID || given.GameplayDigest != latest.GameplayDigest || given.PresentationDigest != latest.PresentationDigest ||
				given.Anchor.Source != latest.Anchor.Source || given.Anchor.Target != latest.Anchor.Target ||
				given.Anchor.Position == nil || latest.Anchor.Position == nil || *given.Anchor.Position != *latest.Anchor.Position {
				t.Errorf("reset hands the policy\n  %+v\nthe runtime's increment for the same process is\n  %+v", *given, latest)
			}

			// 2. 同一策略对 reset 与增量得出同样的结论（可见性与过滤后的 Anchor）。
			policies := map[string]VisibilityPolicy{
				"owner hidden":     EntityVisibilityPolicy{Visible: func(_ syncstream.Observer, entity skill.EntityID) (bool, error) { return entity != 1, nil }},
				"lifecycle hidden": EntityVisibilityPolicy{Visible: func(_ syncstream.Observer, entity skill.EntityID) (bool, error) { return entity != lifecycle, nil }, RedactSpatial: true},
				"all visible":      EntityVisibilityPolicy{Visible: func(syncstream.Observer, skill.EntityID) (bool, error) { return true, nil }},
				// 自定义策略按 PrimaryTarget 决定整条去留：reset 必须和增量给它同一个 PrimaryTarget。
				"primary target hidden": policyFunc(func(_ syncstream.Observer, event skill.PresentationEvent) (skill.PresentationEvent, bool, error) {
					return event, event.PrimaryTarget != 2, nil
				}),
			}
			for policyName, policy := range policies {
				want, allowed, err := policy.FilterPresentation(syncstream.Observer{ID: 7}, latest)
				if err != nil {
					t.Fatal(err)
				}
				got := recoverPresentationReset(t, runtime, policy)
				if !allowed {
					if len(got) != 0 {
						t.Errorf("%s: the increment is hidden but the reset sends %+v", policyName, got)
					}
					continue
				}
				if len(got) != 1 {
					t.Errorf("%s: the increment is visible (%+v) but the reset sends %+v", policyName, want.Anchor, got)
					continue
				}
				anchor := got[0].Anchor
				if anchor.Source != want.Anchor.Source || anchor.Target != want.Anchor.Target || (anchor.Position == nil) != (want.Anchor.Position == nil) || (anchor.Direction == nil) != (want.Anchor.Direction == nil) {
					t.Errorf("%s: reset anchor %+v, the filtered increment's anchor is %+v", policyName, anchor, want.Anchor)
				}
				if got[0].Kind != skill.ActivePresentationProcess || got[0].ProcessID != latest.ProcessID {
					t.Errorf("%s: reset entry %+v is not the process %d", policyName, got[0], latest.ProcessID)
				}
			}
		})
	}
}

// visibilityRuntimeFor 与 visibilityRuntime 相同，但编译给定的技能，并把实体 1 放在原点、实体 2 放在作用圈外。
func visibilityRuntimeFor(t *testing.T, definitionJSON string, options skill.RuntimeOptions) (*skill.Runtime, *skill.Program) {
	t.Helper()
	environment := skill.DefaultCompileEnvironment()
	definition, err := skill.Parse([]byte(definitionJSON))
	if err != nil {
		t.Fatal(err)
	}
	program, diagnostics := skill.Compile(definition, environment)
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == skill.DiagnosticError {
			t.Fatalf("compile: %+v", diagnostic)
		}
	}
	host := skill.NewMemoryHost(skill.AuthorityIdentity{Revision: environment.Revision, Digest: environment.Digest})
	host.ConfigureGameplayCatalog(environment.Gameplay)
	host.UpsertEntity(skill.MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100})
	host.UpsertEntity(skill.MemoryEntity{ID: 2, Alive: true, Health: 100, MaxHealth: 100, Position: skill.Position{X: 1000}})
	return skill.NewRuntime(host, options), program
}

// recoverPresentationReset 用给定策略对 observer 7 走一次 Recover，返回发出的 reset 条目。
func recoverPresentationReset(t *testing.T, runtime *skill.Runtime, policy VisibilityPolicy) []skill.ActivePresentation {
	t.Helper()
	publisher := &recordingPublisher{}
	projector, _ := NewProjector(1)
	coordinator, err := NewCoordinator(CoordinatorOptions{Runtime: runtime, History: syncstream.NewHistory(syncstream.HistoryOptions{SchemaVersion: 1}), Publisher: publisher, Projector: projector, Visibility: policy})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Recover(syncstream.ResyncRequest{Observer: syncstream.Observer{ID: 7}, Stream: syncstream.Stream{Topic: TopicPresentation, Key: 1}, SchemaVersion: 1}); err != nil {
		t.Fatal(err)
	}
	return publishedResets(t, publisher.packets)
}
