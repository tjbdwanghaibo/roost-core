package actionflow

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// U-0125 · C2（空洞测试）· classscan 后 actionflow 采样 9/25 无覆盖。
//
// 任务计划在归一化时拒绝所有空 / 越界的形状（无步骤、起点越界、动作为 none、
// 后继步骤越界、nil 指针），之后按步索引执行时再守一次越界；任务运行器的状态
// / 变更钩子若在启动途中结束了任务，StartMission 必须报 ErrReentrantMutation
// 而不是把一个已结束的任务当作当前任务；动作构建器交出 nil 动作必须被
// ErrBuilderNil 拒绝。U-0100 记的另两处（action_runner 96、mission_runner 81）
// 复核：前者与 finish 内部的同一判定重复，后者在 ending 标志下不可达。

func TestNormalizePlanRefusesEveryInvalidShape(t *testing.T) {
	valid := MissionPlan{Steps: []MissionStep{{Action: 1}, {Action: 2}}}
	if _, err := NormalizePlan(valid); err != nil {
		t.Fatalf("baseline plan rejected: %v", err)
	}
	cases := []struct {
		name string
		plan MissionPlan
		want error
		text string
	}{
		{"no steps", MissionPlan{}, ErrMissionPlanInvalid, ""},
		{"start below zero", MissionPlan{Start: -1, Steps: valid.Steps}, ErrMissionPlanInvalid, ""},
		{"start past the end", MissionPlan{Start: 2, Steps: valid.Steps}, ErrMissionPlanInvalid, ""},
		{"action none", MissionPlan{Steps: []MissionStep{{Action: 1}, {Action: 0}}}, ErrMissionStepInvalid, "action none at 1"},
		{"success next past the end", MissionPlan{Steps: []MissionStep{{Action: 1, OnSuccess: NextMissionStep(5)}}}, ErrMissionStepInvalid, "next step 5"},
		{"fail next below zero", MissionPlan{Steps: []MissionStep{{Action: 1, OnFail: NextMissionStep(-1)}}}, ErrMissionStepInvalid, "next step -1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizePlan(tc.plan)
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("NormalizePlan = %v, want %v containing %q", err, tc.want, tc.text)
			}
		})
	}
	var none *MissionPlan
	if _, err := PlanFrom(none); !errors.Is(err, ErrMissionPlanInvalid) {
		t.Fatalf("PlanFrom(nil *MissionPlan) = %v", err)
	}
	if _, err := PlanFrom(&valid); err != nil {
		t.Fatalf("PlanFrom(*MissionPlan) = %v", err)
	}
	if _, err := PlanFrom("not a plan"); !errors.Is(err, ErrMissionPlanInvalid) {
		t.Fatalf("PlanFrom(string) = %v", err)
	}
}

func TestPlanMissionRefusesAStepIndexOutsideThePlan(t *testing.T) {
	plan, err := NormalizePlan(MissionPlan{Steps: []MissionStep{{Action: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	mission := &PlanMission{plan: plan}
	for _, step := range []int{-1, 1, 99} {
		if err := mission.startStep(nil, step); !errors.Is(err, ErrMissionStepInvalid) {
			t.Fatalf("startStep(%d) = %v, want ErrMissionStepInvalid", step, err)
		}
	}
}

func TestMissionRunnerRefusesAHookThatEndsTheMissionDuringStart(t *testing.T) {
	registry := NewRegistry()
	mission := &runnerTestMission{kind: 1}
	if err := registry.RegisterMission(1, func() Mission { return mission }); err != nil {
		t.Fatal(err)
	}
	var runner *MissionRunner
	runner, err := NewMissionRunner(MissionRunnerConfig{Registry: registry, Hooks: MissionRunnerHooks{
		OnChanged: func(MissionInfo) {
			// Ending the mission from the change hook is allowed by the ending
			// guard (starting is not ending); the start must notice that the
			// current mission is no longer the one it just installed.
			runner.EndCurMission(NewActionReason("ended from hook"))
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.StartMission(1, nil); !errors.Is(err, ErrReentrantMutation) {
		t.Fatalf("StartMission with a hook ending the mission = %v, want ErrReentrantMutation", err)
	}
	if runner.CurMission() != nil {
		t.Fatalf("a mission ended during its own start is still current: %v", runner.CurMission())
	}
}

func TestActionRunnerRefusesABuilderThatReturnsNoAction(t *testing.T) {
	registry := NewRegistry()
	if err := registry.RegisterAction(1, func(any) (Action, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	runner, err := NewActionRunner(ActionRunnerConfig{Registry: registry, GroupForKind: func(ActionKind) (ActionGroup, bool) { return 1, true }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Start(1, nil, 0, time.Now()); !errors.Is(err, ErrBuilderNil) {
		t.Fatalf("Start with a builder returning no action = %v, want ErrBuilderNil", err)
	}
	if runner.Current(1) != nil {
		t.Fatal("a nil action was installed as current")
	}
}
