package skill

import "fmt"

func runGraphPass(context *compileContext) {
	phases := context.artifacts.ir.phases
	graph := PhaseGraph{
		Index:     make(map[string]int, len(phases)),
		Adjacency: make([][]int, len(phases)),
		Reverse:   make([][]int, len(phases)),
		Reachable: make([]bool, len(phases)),
	}
	for index, phase := range phases {
		if previous, exists := graph.Index[phase.id]; exists {
			context.addDiagnostic(DiagnosticPhaseDuplicate, phase.source.Path+".id", fmt.Sprintf("phase %q duplicates index %d", phase.id, previous))
			continue
		}
		graph.Index[phase.id] = index
	}
	initial, initialExists := graph.Index[context.artifacts.ir.initialPhase]
	if !initialExists {
		context.addDiagnostic(DiagnosticPhaseInitialMissing, "$.initial_phase", "initial phase does not exist")
	}

	for from, phase := range phases {
		seen := make(map[int]bool)
		walkPhaseFlows(phase.events, func(root flowIR) {
			walkFlowTree(root, func(flow flowIR) {
				transition, ok := flow.(*gotoFlowIR)
				if !ok {
					return
				}
				to, exists := graph.Index[transition.phase]
				if !exists {
					context.addDiagnostic(DiagnosticPhaseTargetMissing, transition.source.Path+".phase", fmt.Sprintf("phase %q does not exist", transition.phase))
					return
				}
				if !seen[to] {
					seen[to] = true
					graph.Adjacency[from] = append(graph.Adjacency[from], to)
					graph.Reverse[to] = append(graph.Reverse[to], from)
				}
			})
		})
	}

	if initialExists {
		markReachable(initial, graph.Adjacency, graph.Reachable)
		for index, reachable := range graph.Reachable {
			if !reachable {
				context.addDiagnostic(DiagnosticPhaseUnreachable, phases[index].source.Path, fmt.Sprintf("phase %q is unreachable", phases[index].id))
			}
		}
	}
	graph.TopologicalOrder = stableTopologicalOrder(graph.Adjacency, graph.Reverse)
	if len(graph.TopologicalOrder) != len(phases) {
		context.addDiagnostic(DiagnosticPhaseCycle, "$.phases", "phase transition graph contains a cycle")
	}
	context.artifacts.graph = graph
}

func markReachable(index int, adjacency [][]int, reachable []bool) {
	if index < 0 || index >= len(reachable) || reachable[index] {
		return
	}
	reachable[index] = true
	for _, next := range adjacency[index] {
		markReachable(next, adjacency, reachable)
	}
}

func stableTopologicalOrder(adjacency, reverse [][]int) []int {
	indegree := make([]int, len(reverse))
	for index := range reverse {
		indegree[index] = len(reverse[index])
	}
	order := make([]int, 0, len(indegree))
	used := make([]bool, len(indegree))
	for len(order) < len(indegree) {
		found := -1
		for index := range indegree {
			if !used[index] && indegree[index] == 0 {
				found = index
				break
			}
		}
		if found < 0 {
			break
		}
		used[found] = true
		order = append(order, found)
		for _, next := range adjacency[found] {
			indegree[next]--
		}
	}
	return order
}
