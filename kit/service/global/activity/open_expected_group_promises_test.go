package activity

// RR-20261006-17：协调器读了活动组文件（activity.groups_file，C4），OpenActivity 却不按它核对
// expected 集合。承诺：组文件设置后，Key 的组必须在文件里，expected 里的每个 sid 都必须是该组成员
// （组成员的子集是正常的——只有活着的服进 expected）；不一致就拒绝（ErrInvalid）并点名文件、组与 sid，
// 拒绝发生在任何写入之前，窗口里不留 opening 条目。后续：组文件改为协调器必填，缺失时 Mod.Init 拒绝启动。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

func memoryStores() RedisStores {
	return RedisStores{
		Activities:   versionstore.NewMemoryStore[Key, Activity](),
		Participants: versionstore.NewMemoryStore[ParticipantKey, Participant](),
		Ledger:       versionstore.NewMemoryStore[RequestKey, ProgressReservation](),
		Audits:       versionstore.NewMemoryStore[Key, NotifyAuditLog](),
		Dispatches:   versionstore.NewMemoryStore[DispatchKey, Dispatch](),
		Windows:      versionstore.NewMemoryStore[string, Window](),
	}
}

// coordinatorWithGroupsFile wires the Service the way the Mod's Provide does,
// over memory stores, with activity.groups_file pointing at path.
func coordinatorWithGroupsFile(t *testing.T, path string) (*Service, RedisStores) {
	t.Helper()
	cfg := modConfig(t)
	cfg.Set("activity.groups_file", path)
	mod := NewMod(nil)
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	stores := memoryStores()
	service, err := mod.newService(stores, func() time.Time { return time.Unix(1_700_000_000, 0) })
	if err != nil {
		t.Fatal(err)
	}
	return service, stores
}

func TestTheCoordinatorRefusesAnExpectedSetTheGroupsFileDoesNotAllow(t *testing.T) {
	path := writeGroups(t, groupYAML("alliance-a", []int64{1000, 1001, 1002})+"  - id: alliance-b\n    game_sids: [2000, 2001]\n")
	cases := []struct {
		name     string
		group    string
		expected []int32
		mustName []string
	}{
		{"a-sid-of-another-group", "alliance-a", []int32{1000, 2000}, []string{"alliance-a", "2000", "alliance-b"}},
		{"a-sid-in-no-group", "alliance-a", []int32{1000, 1001, 3000}, []string{"alliance-a", "3000"}},
		{"a-group-not-in-the-file", "alliance-c", []int32{1000}, []string{"alliance-c"}},
		{"the-whole-other-group-under-this-key", "alliance-a", []int32{2000, 2001}, []string{"alliance-a", "2000"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, stores := coordinatorWithGroupsFile(t, path)
			key := Key{GroupID: tc.group, ActivityID: "race-1", Phase: PhaseClose}
			opened, err := service.OpenActivity(context.Background(), key, tc.expected)
			if err == nil {
				t.Fatalf("OpenActivity(%s, %v) opened %+v; the groups file does not allow that expected set", tc.group, tc.expected, opened.ExpectedGameSIDs)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("OpenActivity error %v is not ErrInvalid", err)
			}
			for _, want := range append([]string{path}, tc.mustName...) {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %s", err, want)
				}
			}
			if _, found, err := stores.Windows.Get(context.Background(), tc.group); err != nil || found {
				t.Fatalf("a refused open left a window for %s (found=%v, err=%v)", tc.group, found, err)
			}
			if _, found, _ := stores.Activities.Get(context.Background(), key); found {
				t.Fatalf("a refused open stored activity %s", key)
			}
		})
	}
}

func TestTheCoordinatorOpensAnyLiveSubsetOfTheGroup(t *testing.T) {
	path := writeGroups(t, groupYAML("alliance-a", []int64{1000, 1001, 1002}))
	service, _ := coordinatorWithGroupsFile(t, path)
	for i, expected := range [][]int32{{1000, 1001, 1002}, {1002}, {1001, 1000}} {
		key := Key{GroupID: "alliance-a", ActivityID: "race-" + string(rune('a'+i)), Phase: PhaseClose}
		if _, err := service.OpenActivity(context.Background(), key, expected); err != nil {
			t.Fatalf("OpenActivity with live members %v of the group: %v", expected, err)
		}
	}
}

// 后续（维护者 2026-10-06：线上未部署、不做旧版兼容、配置要简单）：groups_file 改为必填。
// 协调器没有组文件就无从核对 expected，修前 Init 照常通过、开窗只查形状；现在 Init 拒绝启动，
// 错误点名键名和生成器写的默认路径，空白值等同缺失。
func TestTheCoordinatorRefusesToStartWithoutAGroupsFile(t *testing.T) {
	for name, value := range map[string]any{"unset": nil, "blank": "  "} {
		t.Run(name, func(t *testing.T) {
			cfg := viper.New()
			cfg.Set("activity.key_prefix", "roost:activity")
			cfg.Set("activity.reservation_ttl", 30*time.Minute)
			if value != nil {
				cfg.Set("activity.groups_file", value)
			}
			err := NewMod(nil).Init(cfg)
			if err == nil {
				t.Fatal("Init accepted a coordinator with no activity.groups_file; it would open windows nothing checks against the groups")
			}
			for _, want := range []string{"activity.groups_file", "configs/activity_groups.yaml"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %s", err, want)
				}
			}
		})
	}
}

// 后续二（维护者 2026-10-06：不留能绕过核对的分支）：groups_file 必填之后，“没有组就不核对”只剩直接调
// New 构造这一条路。承诺：Config.Groups 为 nil 时 New 拒绝构造，错误点名 Groups 与它对应的配置键；
// 修前 New 照常返回，开一个组文件里根本不存在的组的窗口也会成功。
func TestNewRefusesACoordinatorWithoutGroups(t *testing.T) {
	stores := memoryStores()
	service, err := New(Config{
		Activities: stores.Activities, Participants: stores.Participants, Ledger: stores.Ledger,
		Audits: stores.Audits, Dispatches: stores.Dispatches, Windows: stores.Windows,
	})
	if err == nil {
		key := Key{GroupID: "no-such-group", ActivityID: "race-1", Phase: PhaseClose}
		_, openErr := service.OpenActivity(context.Background(), key, []int32{4242})
		t.Fatalf("New accepted a coordinator with no Groups; OpenActivity(%s, [4242]) then returned %v without checking the group", key.GroupID, openErr)
	}
	for _, want := range []string{"Groups", "activity.groups_file"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %s", err, want)
		}
	}
}
