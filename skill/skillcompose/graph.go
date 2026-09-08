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
func GraphFromProfile(profile SkillProfile) CausalGraph {
	graph := CausalGraph{Edges: map[int][]int{}, Sinks: map[int]bool{}}
	if len(profile.Operations) > 0 {
		graph.Roots = []int{0}
	}
	for i, op := range profile.Operations {
		if i+1 < len(profile.Operations) {
			graph.Edges[i] = []int{i + 1}
		}
		if op == "damage" || op == "heal" || op == "spawn" {
			graph.Sinks[i] = true
		}
	}
	return graph
}
