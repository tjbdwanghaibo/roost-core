package skillcompose

func (metrics Metrics) Bounded() bool {
	return metrics.Targets >= 0 && metrics.Processes >= 0 && metrics.LifetimeTicks >= 0
}
