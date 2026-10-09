package activity

import domain "github.com/tjbdwanghaibo/roost-core/service/activity"

// C4（维护者决定，docs/feature/C4-ACTIVITY-GROUPS-FILE-2026-10-06.md）：活动组由一个文件定义，game 与
// 协调器经同一个 ParseGroups 读它。承诺：
//   - 一组最多 MaxExpectedGames（协调器单个窗口 expected 集合的上限，同一个常量）个 game，65 个在加载时
//     拒绝并点名文件、组和上限；64 个接受，且 64 个全活时协调器能用它开窗；
//   - 非正数 / 超出 int32 的 sid、组内重复、一个 sid 属于两个组、组 id 为空 / 重复 / 含 '/'、没有组、
//     未知字段，都在加载时拒绝并点名；
//   - Of 按 sid 找到所属组，IDs 按文件顺序给出全部组 id；
//   - 协调器 Mod：activity.groups_file 设置后在 Init 校验文件，sweep_groups 为空时扫文件里的全部组，
//     显式 sweep_groups 仍以它为准；文件不合格时 Init 失败并点名键名。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func groupYAML(id string, sids []int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "groups:\n  - id: %s\n    game_sids: [", id)
	for i, sid := range sids {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprint(&b, sid)
	}
	b.WriteString("]\n")
	return b.String()
}

func sidRange(from int64, n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = from + int64(i)
	}
	return out
}

func TestAGroupLargerThanOneWindowIsRefusedWhenLoaded(t *testing.T) {
	_, err := domain.ParseGroups([]byte(groupYAML("alliance-a", sidRange(2000, domain.MaxExpectedGames+1))), "groups.yaml")
	if err == nil {
		t.Fatalf("a group of %d games loaded; the coordinator refuses every window expecting more than %d", domain.MaxExpectedGames+1, domain.MaxExpectedGames)
	}
	for _, want := range []string{"groups.yaml", `"alliance-a"`, fmt.Sprint(domain.MaxExpectedGames + 1), fmt.Sprint(domain.MaxExpectedGames)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %s", err, want)
		}
	}
}

func TestAFullGroupOpensAWindowWithTheCoordinator(t *testing.T) {
	sids := sidRange(2000, domain.MaxExpectedGames)
	groups, err := domain.ParseGroups([]byte(groupYAML("alliance-a", sids)), "groups.yaml")
	if err != nil {
		t.Fatalf("a group of exactly %d games was refused: %v", domain.MaxExpectedGames, err)
	}
	group, ok := groups.Of(2000 + int32(domain.MaxExpectedGames) - 1)
	if !ok || group.ID != "alliance-a" || len(group.GameSIDs) != domain.MaxExpectedGames {
		t.Fatalf("Of(last sid) = %+v, %v", group, ok)
	}
	// Every member live: the expected set is the whole group, which is what
	// the coordinator must take.
	service, _ := newActivityService(t, func(c *domain.Config) { c.Groups = &groups })
	key := domain.Key{GroupID: group.ID, ActivityID: "race-1", Phase: domain.PhaseClose}
	opened, err := service.OpenActivity(context.Background(), key, group.GameSIDs)
	if err != nil {
		t.Fatalf("the coordinator refused a window expecting a whole valid group: %v", err)
	}
	if !slices.Equal(opened.ExpectedGameSIDs, group.GameSIDs) {
		t.Fatalf("opened with %v, want the group %v", opened.ExpectedGameSIDs, group.GameSIDs)
	}
}

func TestAGroupsFileThatCannotBeUsedIsRefusedByName(t *testing.T) {
	cases := []struct {
		name, body string
		want       []string
	}{
		{"empty-file", "", []string{"empty"}},
		{"no-group", "groups: []\n", []string{"no group"}},
		{"group-without-id", "groups:\n  - game_sids: [1]\n", []string{"group 1 has no id"}},
		{"slash-in-id", groupYAML("a/b", []int64{1}), []string{`"a/b"`, "'/'"}},
		{"repeated-group", groupYAML("a", []int64{1}) + "  - id: a\n    game_sids: [2]\n", []string{`"a"`, "twice"}},
		{"no-games", "groups:\n  - id: a\n    game_sids: []\n", []string{`"a"`, "no game"}},
		{"non-positive", groupYAML("a", []int64{1, 0}), []string{`"a"`, " 0,"}},
		{"beyond-int32", groupYAML("a", []int64{1, 1 << 32}), []string{`"a"`, fmt.Sprint(int64(1 << 32))}},
		{"repeated-sid", groupYAML("a", []int64{1, 2, 1}), []string{`"a"`, "game 1 twice"}},
		{"sid-in-two-groups", groupYAML("a", []int64{1, 2}) + "  - id: b\n    game_sids: [3, 2]\n", []string{"game 2", `"a"`, `"b"`}},
		{"misspelt-key", "groups:\n  - id: a\n    game_sid: [1]\n", []string{"game_sid"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := domain.ParseGroups([]byte(tc.body), "groups.yaml")
			if err == nil {
				t.Fatalf("%q loaded", tc.body)
			}
			for _, want := range append(tc.want, "groups.yaml") {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %q", err, want)
				}
			}
		})
	}
}

func TestGroupsAnswerWhichGroupASIDIsIn(t *testing.T) {
	groups, err := domain.ParseGroups([]byte(groupYAML("a", []int64{1, 2})+"  - id: b\n    game_sids: [3]\n"), "groups.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := groups.IDs(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("IDs = %v", got)
	}
	if group, ok := groups.Of(3); !ok || group.ID != "b" || !slices.Equal(group.GameSIDs, []int32{3}) {
		t.Fatalf("Of(3) = %+v, %v", group, ok)
	}
	if _, ok := groups.Of(4); ok {
		t.Fatal("a sid in no group was given one")
	}
	// A caller's copy is its own.
	group, _ := groups.Of(1)
	group.GameSIDs[0] = 99
	if again, _ := groups.Of(1); again.GameSIDs[0] != 1 {
		t.Fatal("Of handed out the groups' own slice")
	}
}

func writeGroups(t testing.TB, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "activity_groups.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestModSweepsTheGroupsInTheGroupsFile(t *testing.T) {
	path := writeGroups(t, groupYAML("a", []int64{1, 2})+"  - id: b\n    game_sids: [3]\n")
	t.Run("file-groups-when-sweep-groups-is-empty", func(t *testing.T) {
		cfg := modConfig(t)
		cfg.Set("activity.groups_file", path)
		mod := NewMod(nil)
		if err := mod.Init(cfg); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(mod.sweepGroups, []string{"a", "b"}) {
			t.Fatalf("sweep groups = %v, want every group in the file", mod.sweepGroups)
		}
	})
	t.Run("explicit-sweep-groups-decide", func(t *testing.T) {
		cfg := modConfig(t)
		cfg.Set("activity.groups_file", path)
		cfg.Set("activity.sweep_groups", []string{"b"})
		mod := NewMod(nil)
		if err := mod.Init(cfg); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(mod.sweepGroups, []string{"b"}) {
			t.Fatalf("sweep groups = %v, want the configured [b]", mod.sweepGroups)
		}
	})
	t.Run("an-unusable-file-stops-init", func(t *testing.T) {
		cfg := modConfig(t)
		cfg.Set("activity.groups_file", writeGroups(t, groupYAML("a", sidRange(1, domain.MaxExpectedGames+1))))
		err := NewMod(nil).Init(cfg)
		if err == nil || !strings.Contains(err.Error(), "activity.groups_file") {
			t.Fatalf("Init with a %d-game group: %v, want a refusal naming activity.groups_file", domain.MaxExpectedGames+1, err)
		}
	})
	t.Run("a-missing-file-stops-init", func(t *testing.T) {
		cfg := modConfig(t)
		cfg.Set("activity.groups_file", filepath.Join(t.TempDir(), "absent.yaml"))
		if err := NewMod(nil).Init(cfg); err == nil || !strings.Contains(err.Error(), "absent.yaml") {
			t.Fatalf("Init with a missing groups file: %v", err)
		}
	})
}
