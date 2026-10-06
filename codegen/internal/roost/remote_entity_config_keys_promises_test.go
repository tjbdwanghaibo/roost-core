package roost

// A8（收尾第 2 批，2026-10-06）：生成的 remote_entity 段要写出后来加进 kit 的键——B2 的
// cached_max_staleness、O4 的 snapshot_interest_per_consumer、O-M6-3 的 snapshot_l2_tombstone_wait_*、
// Mirror 第 5 步的 mirror.shutdown_timeout——开发配置、生产示例、k8s Secret 示例三份一致。
//
// 旧行为：catalog.go 的模板没有这些键，运维要在 USER_GUIDE 里找键名才知道能调。生产化时把 "replicas: 1"
// 整串替换成 "replicas: 3"，加了 snapshot_l2_tombstone_wait_replicas 之后会把墓碑 WAIT 的副本数一起改成 3，
// 所以生产化只改独占一行的流副本数。取值与 core DefaultConfig 的比对、两个 Mod 的 Init 在
// generated_config_validation_promises_test.go 里对生成工程的真实文件做。

import (
	"strings"
	"testing"
)

func TestGeneratedRemoteEntitySectionCarriesTheSnapshotKeys(t *testing.T) {
	m := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"remote_entity"}, []string{"config"})
	for _, production := range []bool{false, true} {
		config := renderServiceConfig(m, "game", production)
		for _, want := range []string{
			"\n  cached_max_staleness: 30s\n",
			"\n  snapshot_interest_per_consumer: 0\n",
			"\n  snapshot_l2_tombstone_wait_replicas: 1\n",
			"\n  snapshot_l2_tombstone_wait_timeout: 50ms\n",
			"\n  mirror:\n    shutdown_timeout: 5s\n",
		} {
			if !strings.Contains(config, want) {
				t.Errorf("production=%v: config lacks %q:\n%s", production, strings.TrimSpace(want), config)
			}
		}
		if production && !strings.Contains(config, "\n  replicas: 3\n") {
			t.Errorf("production config no longer raises the stream replicas:\n%s", config)
		}
	}
}
