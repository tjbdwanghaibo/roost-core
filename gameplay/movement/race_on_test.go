//go:build race

package movement

// raceEnabled：-race 下执行慢约一个数量级，冒烟测试减少实体数，只验证功能。
const raceEnabled = true
