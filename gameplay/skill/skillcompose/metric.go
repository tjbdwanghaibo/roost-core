package skillcompose

// Bounded 报告各项度量都不为负。
func (metrics Metrics) Bounded() bool {
	return metrics.Targets >= 0 && metrics.Spawns >= 0 && metrics.LifetimeTicks >= 0
}
