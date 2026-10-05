//go:build race

package skill

// race 构建下变异性质测试按 1/40 抽样（见 mutationStride）。
func init() { mutationRaceStride = 40 }
