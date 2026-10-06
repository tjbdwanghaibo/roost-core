package remoteentity

// A4 ①：0 本身有含义的键（写许可 0 = 不限、墓碑副本 0 = 关闭、每节点配额 0 = 按 subs 推出）不能用“0 取缺省”，
// 它们的声明写出 default；这个 default 必须与 core 的 DefaultConfig、Mirror 的停机预算常量相同，否则生成器写出、
// doctor 认可的缺省值与运行时实际用的分叉。

import (
	"strconv"
	"testing"

	coreremote "github.com/tjbdwanghaibo/roost-core/remoteentity"
)

func TestRemoteEntityDeclaredDefaultsMatchCoreDefaults(t *testing.T) {
	defaults := coreremote.DefaultConfig()
	entity := NewRemoteEntityMod(1).ConfigSchema()
	mirror := NewRemoteMirrorMod(1).ConfigSchema()
	for _, tc := range []struct {
		name, key, want string
	}{
		{"entity", "remote_entity.max_concurrent_writes", strconv.Itoa(defaults.MaxConcurrentWrites)},
		{"entity", "remote_entity.snapshot_l2_tombstone_wait_replicas", strconv.Itoa(defaults.SnapshotL2TombstoneWaitReplicas)},
		{"entity", "remote_entity.snapshot_l2_tombstone_wait_timeout", defaults.SnapshotL2TombstoneWaitTimeout.String()},
		{"entity", "remote_entity.snapshot_l2_tombstone_wait_timeout max", coreremote.MaxSnapshotL2TombstoneWaitTimeout.String()},
		{"mirror", "remote_entity.mirror.shutdown_timeout", defaultMirrorShutdownTimeout.String()},
	} {
		schema := entity
		if tc.name == "mirror" {
			schema = mirror
		}
		name, wantMax := tc.key, false
		if n := len(name); n > 4 && name[n-4:] == " max" {
			name, wantMax = name[:n-4], true
		}
		key, ok := schema.Lookup(name)
		got := key.Default
		if wantMax {
			got = key.Max
		}
		if !ok || got != tc.want {
			t.Errorf("%s: declared %q, core says %q", tc.key, got, tc.want)
		}
	}
	if defaults.SnapshotInterestPerConsumer != 0 {
		t.Errorf("DefaultConfig().SnapshotInterestPerConsumer = %d; the declaration leaves it 0 (derived from subs)", defaults.SnapshotInterestPerConsumer)
	}
}
