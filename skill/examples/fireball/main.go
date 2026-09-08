package main

import (
	"fmt"

	skill "github.com/tjbdwanghaibo/roost-skill/skill"
)

const fireballJSON = `{
  "schema": "roost.skill/v2",
  "id": "skill.demo.fireball",
  "name": "Fireball",
  "description": "Windup, commit, then burn the target.",
  "activation": {
    "type": "active",
    "policy": {"mode": "tap"},
    "cast_window": {
      "windup_ticks": 3,
      "commit_tick": 2,
      "recovery_ticks": 2,
      "movement": "locked",
      "turning": "allowed",
      "interrupt_tags": [],
      "refund_before_commit": true
    }
  },
  "input_schema": {"type": "entity"},
  "cooldown_ticks": 20,
  "global_cooldown_ticks": 8,
  "costs": [{"resource": "mana", "amount": 5}],
  "memory": {},
  "initial_phase": "cast",
  "phases": [{
    "id": "cast",
    "timeout_ticks": 0,
    "on": {"enter": {"flow": "sequence", "steps": [
      {"flow": "effect", "effect": {
        "type": "damage", "target": "$input.target",
        "amount": 25, "damage_type": "magic", "element": "fire"
      }},
      {"flow": "finish", "reason": "done"}
    ]}}
  }]
}`

func main() {
	// 1. Parse：严格 wire 解析（未知字段、重复键、尾随数据都会被拒绝）。
	definition, err := skill.Parse([]byte(fireballJSON))
	if err != nil {
		panic(err)
	}

	// 2. Compile：编译环境提供属性/资源/状态等权威目录，产出不可变 Program。
	environment := skill.DefaultCompileEnvironment()
	program, diagnostics := skill.Compile(definition, environment)
	for _, diagnostic := range diagnostics {
		fmt.Println("diagnostic:", diagnostic)
	}
	if program == nil {
		panic("compile failed")
	}

	// 3. Host + Runtime：MemoryHost 是内置参考世界；生产环境换成自己的 Host 实现。
	host := skill.NewMemoryHost(skill.AuthorityIdentity{
		Revision: environment.Revision, Digest: environment.Digest,
	})
	host.ConfigureGameplayCatalog(environment.Gameplay)
	host.UpsertEntity(skill.MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100,
		Resources: map[string]int64{"mana": 30}})
	host.UpsertEntity(skill.MemoryEntity{ID: 2, Alive: true, Health: 100, MaxHealth: 100})

	var seed [32]byte // 生产环境使用对局级随机种子
	runtime := skill.NewRuntime(host, skill.RuntimeOptions{MatchSeed: seed})

	// 4. Activate 进入 windup；Advance 推进确定性时间线（tick 为绝对时刻）。
	castID, err := runtime.Activate(program, skill.CastInput{Caster: 1, Target: 2})
	if err != nil {
		panic(err)
	}

	// windup 3 tick（commit 在 tick 2）+ recovery 2 tick => tick 5 施法完整结束。
	if err := runtime.Advance(5); err != nil {
		panic(err)
	}

	cast, _ := runtime.InspectCast(castID)
	fmt.Println("cast status:", cast.Status)                     // finished
	fmt.Println("target hp:", host.HealthForTest(2))             // 75
	fmt.Println("caster mana:", host.ResourceForTest(1, "mana")) // 25

	// 冷却与全局冷却（"$gcd"，从 commit tick 起算）都是普通冷却条目。
	for _, cooldown := range runtime.StateSnapshot().Cooldowns {
		fmt.Printf("cooldown %s: remaining %d ticks\n", cooldown.ProgramID, cooldown.Remaining)
	}

	// Host 事件流：伤害解析等世界事实。
	for _, event := range host.Events(0) {
		fmt.Printf("event tick=%d kind=%s entity=%d\n", event.Tick, event.Kind, event.Entity)
	}
}
