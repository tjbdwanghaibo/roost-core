package skillcompose

type CausalGraph struct {
	Edges map[int][]int
	Roots []int
	Sinks map[int]bool
}

func (graph CausalGraph) ReachesSink() bool {
	seen := map[int]bool{}
	queue := append([]int(nil), graph.Roots...)
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		if seen[node] {
			continue
		}
		seen[node] = true
		if graph.Sinks[node] {
			return true
		}
		queue = append(queue, graph.Edges[node]...)
	}
	return false
}

// GraphFromProfile 是操作清单的线性摘要，用于检查是否存在有效果的出口。
// SkillProfile 不含控制流边，不能据此证明某个分支运行时必达，也不能替代编译器校验。
func GraphFromProfile(profile SkillProfile) CausalGraph {
	graph := CausalGraph{Edges: map[int][]int{}, Sinks: map[int]bool{}}
	if len(profile.Operations) > 0 {
		graph.Roots = []int{0}
	}
	for i, op := range profile.Operations {
		if i+1 < len(profile.Operations) {
			graph.Edges[i] = []int{i + 1}
		}
		switch op {
		case "damage", "heal", "summon", "shield", "status", "modify_status_instance",
			"attribute_modifier", "resource", "state", "ability_state", "entity_command",
			"teleport", "motion_impulse", "stop_movement", "modify_spawn", "restore_snapshot":
			graph.Sinks[i] = true
		}
	}
	return graph
}
