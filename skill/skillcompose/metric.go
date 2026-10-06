package skillcompose

func (metrics Metrics) Bounded() bool {
	return metrics.Targets >= 0 && metrics.Spawns >= 0 && metrics.LifetimeTicks >= 0
}
