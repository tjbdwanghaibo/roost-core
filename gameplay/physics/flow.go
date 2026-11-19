package physics

import (
	"errors"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/gameplay/level"
)

// 关卡流程在物理房间里能看到的事实，以及能请求物理房间执行的动作。名字与参数类型是关卡格式的一部分：
// 流程定义（Level.Flow）里只能写这里声明的、以及业务另外登记的名字。执行器在 service/physicsroom。
//
// actor 一律是房间内的刚体 ID（小于 2^63，放得进 int64）；玩家 ID 与刚体的对应关系在业务的座位表里。
const (
	EventRegionEnter = "region.enter" // actor:int, region:string
	EventRegionLeave = "region.leave" // actor:int, region:string
	EventMemberJoin  = "member.join"  // actor:int
	EventMemberLeave = "member.leave" // actor:int

	ActionTeleport   = "physics_teleport"   // actor:int, spawn:string  放到命名出生点，速度清零
	ActionCheckpoint = "physics_checkpoint" // actor:int, spawn:string  只记检查点，不移动
	ActionRespawn    = "physics_respawn"    // actor:int                回到最近的检查点，没有就用缺省出生点
)

// FlowRegistry 返回物理房间自带的事件与动作声明。每次返回独立副本，调用方可以修改。
func FlowRegistry() level.Registry {
	return level.Registry{
		Events: map[string]map[string]level.ValueKind{
			EventRegionEnter: {"actor": level.Integer, "region": level.Text},
			EventRegionLeave: {"actor": level.Integer, "region": level.Text},
			EventMemberJoin:  {"actor": level.Integer},
			EventMemberLeave: {"actor": level.Integer},
		},
		Actions: map[string]level.ActionSpec{
			ActionTeleport:   {Version: 1, Args: map[string]level.ValueKind{"actor": level.Integer, "spawn": level.Text}},
			ActionCheckpoint: {Version: 1, Args: map[string]level.ValueKind{"actor": level.Integer, "spawn": level.Text}},
			ActionRespawn:    {Version: 1, Args: map[string]level.ValueKind{"actor": level.Integer}},
		},
	}
}

// ValidateFlow 只做不需要能力表的检查：大小、能否按流程定义解码（未知字段拒绝）、时间域必须是 tick
// （物理房间按已提交的 Tick 推进流程，不读墙钟）。空表示没有流程。
// 流程里用到的能力是否存在、类型是否对得上，要到 CompileFlow 才能核对，因为业务能力由宿主登记。
func ValidateFlow(flow []byte) error {
	if len(flow) == 0 {
		return nil
	}
	_, err := decodeFlow(flow)
	return err
}
func decodeFlow(flow []byte) (level.Definition, error) {
	if len(flow) > MaxFlowBytes {
		return level.Definition{}, errors.New("physics: level flow byte limit")
	}
	def, err := level.DecodeDefinition(flow)
	if err != nil {
		return level.Definition{}, fmt.Errorf("physics: level flow: %w", err)
	}
	if def.TimeDomain != "tick" {
		return level.Definition{}, errors.New("physics: level flow must use the tick time domain")
	}
	return def, nil
}

// CompileFlow 把流程定义编译成可执行的 Program：能力表是物理房间自带的声明加上业务登记的 business
// （重名拒绝，业务不能改写物理能力的含义）。编译之外另查两类写死在流程里的名字，拼错时在发布 / 建房时就报错，
// 而不是运行时悄悄不生效：传送与检查点动作里的字面量出生点名，以及与区域事件的 region 字段比较的字面量区域名。
// 变量或事件字段给出的名字到执行时才知道，由执行器给出失败回执。
func CompileFlow(flow []byte, regions []Region, spawns []Spawn, business level.Registry) (*level.Program, error) {
	def, err := decodeFlow(flow)
	if err != nil {
		return nil, err
	}
	registry := FlowRegistry()
	for name, schema := range business.Events {
		if _, taken := registry.Events[name]; taken {
			return nil, fmt.Errorf("physics: business event %q collides with a physics room event", name)
		}
		registry.Events[name] = schema
	}
	for name, spec := range business.Actions {
		if _, taken := registry.Actions[name]; taken {
			return nil, fmt.Errorf("physics: business action %q collides with a physics room action", name)
		}
		registry.Actions[name] = spec
	}
	program, err := level.Compile(def, registry)
	if err != nil {
		return nil, fmt.Errorf("physics: level flow: %w", err)
	}
	regionNames := make(map[string]bool, len(regions))
	for _, r := range regions {
		regionNames[r.ID] = true
	}
	spawnNames := make(map[string]bool, len(spawns))
	for _, s := range spawns {
		spawnNames[s.ID] = true
	}
	for _, stage := range def.Stages {
		for _, trigger := range stage.Triggers {
			where := fmt.Sprintf("stage %s trigger %s", stage.ID, trigger.ID)
			if trigger.Event == EventRegionEnter || trigger.Event == EventRegionLeave {
				for _, c := range trigger.Conditions {
					for _, pair := range [][2]level.Expr{{c.Left, c.Right}, {c.Right, c.Left}} {
						if pair[0].EventField == "region" && pair[1].Literal != nil && !regionNames[pair[1].Literal.Text] {
							return nil, fmt.Errorf("physics: level flow %s compares with unknown region %q", where, pair[1].Literal.Text)
						}
					}
				}
			}
			for _, action := range trigger.Actions {
				if action.Kind != "request" || action.Name != ActionTeleport && action.Name != ActionCheckpoint {
					continue
				}
				if spawn := action.Args["spawn"].Literal; spawn != nil && !spawnNames[spawn.Text] {
					return nil, fmt.Errorf("physics: level flow %s names unknown spawn %q", where, spawn.Text)
				}
			}
		}
	}
	return program, nil
}

// Program 用关卡自己的区域、出生点与流程调用 CompileFlow；没有流程时返回 nil。
func (l Level) Program(business level.Registry) (*level.Program, error) {
	if len(l.Flow) == 0 {
		return nil, nil
	}
	return CompileFlow(l.Flow, l.Regions, l.Spawns, business)
}
