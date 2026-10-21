//go:build race

package heartbeat

// raceEnabled：-race 下执行慢约一个数量级，冒烟测试降低拍率，只验证功能不验证吞吐。
const raceEnabled = true
