package activity

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// 活动组文件（维护者决定 C4，docs/feature/C4-ACTIVITY-GROUPS-FILE-2026-10-06.md）。
//
// 活动组是参与同一个全服活动的 game sid 集合，组 id 就是 Key.GroupID。它由一个文件定义，game 进程
// （按自己的 sid 找组，组成员是开窗时 expected 集合的候选）与协调器（后台 sweep 兜底这些组）读同一个
// 文件、经同一个函数校验，所以两边不会对“哪些服是一组”各写一份：
//
//	groups:
//	  - id: alliance-a
//	    game_sids: [1000, 1001, 1002]
//
// 每组成员数的上限就是协调器单个窗口 expected 集合的上限 MaxExpectedGames：一个比它大的组，同时活着
// 的服超过上限时每个窗口都会被 OpenActivity 拒绝，所以在加载时就拒绝，而不是运行起来每一拍告警。

// Group is one activity group: the game servers that take part in the same
// server-wide activity.
type Group struct {
	// ID is the coordinator's Key.GroupID for this group's activities.
	ID string
	// GameSIDs is every game server that may take part, in file order, each
	// once. Which of them an activity waits for is decided when it opens (the
	// live ones); this is the set it is drawn from.
	GameSIDs []int32
}

// Groups is a validated activity groups file.
type Groups struct {
	source string
	groups []Group
	bySID  map[int32]int
	byID   map[string]int
}

// groupsFile is the file's shape. The sids are decoded wider than int32 so a
// value that does not fit is refused by name instead of wrapping into another
// server's sid.
type groupsFile struct {
	Groups []struct {
		ID       string  `yaml:"id"`
		GameSIDs []int64 `yaml:"game_sids"`
	} `yaml:"groups"`
}

// LoadGroupsFile reads and validates an activity groups file. A relative path
// resolves against the process working directory, like config_data.dir.
func LoadGroupsFile(path string) (Groups, error) {
	if strings.TrimSpace(path) == "" {
		return Groups{}, errors.New("activity groups: no file given")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Groups{}, fmt.Errorf("activity groups: %w", err)
	}
	return ParseGroups(raw, path)
}

// ParseGroups validates an activity groups document; source names it in every
// error. It refuses, rather than repairs, anything that would make a window
// impossible to open or a game's group ambiguous:
//
//   - no group, an empty or repeated group id, or an id containing '/' (a key
//     component the store cannot keep apart);
//   - a group with no game, or more than MaxExpectedGames — the coordinator's
//     limit for one window's expected set;
//   - a sid that is not a positive int32, repeated within a group, or listed
//     in two groups (a game finds its group by its own sid);
//   - an unknown field, so a misspelt key is not read as an empty list.
func ParseGroups(raw []byte, source string) (Groups, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var file groupsFile
	if err := decoder.Decode(&file); err != nil {
		if errors.Is(err, io.EOF) {
			return Groups{}, fmt.Errorf("activity groups %s: the file is empty; it must define at least one group", source)
		}
		return Groups{}, fmt.Errorf("activity groups %s: %w", source, err)
	}
	if len(file.Groups) == 0 {
		return Groups{}, fmt.Errorf("activity groups %s: no group is defined", source)
	}
	out := Groups{source: source, groups: make([]Group, 0, len(file.Groups)), bySID: map[int32]int{}, byID: map[string]int{}}
	for index, entry := range file.Groups {
		id := strings.TrimSpace(entry.ID)
		switch {
		case id == "":
			return Groups{}, fmt.Errorf("activity groups %s: group %d has no id", source, index+1)
		case strings.Contains(id, "/"):
			return Groups{}, fmt.Errorf("activity groups %s: group id %q contains '/'", source, id)
		}
		if _, repeated := out.byID[id]; repeated {
			return Groups{}, fmt.Errorf("activity groups %s: group %q is defined twice", source, id)
		}
		out.byID[id] = len(out.groups)
		if len(entry.GameSIDs) == 0 {
			return Groups{}, fmt.Errorf("activity groups %s: group %q lists no game server", source, id)
		}
		if len(entry.GameSIDs) > MaxExpectedGames {
			return Groups{}, fmt.Errorf("activity groups %s: group %q has %d game servers, more than the %d one activity window can expect; split it into more groups",
				source, id, len(entry.GameSIDs), MaxExpectedGames)
		}
		group := Group{ID: id, GameSIDs: make([]int32, 0, len(entry.GameSIDs))}
		for _, wide := range entry.GameSIDs {
			if wide <= 0 || wide > math.MaxInt32 {
				return Groups{}, fmt.Errorf("activity groups %s: group %q lists %d, which is not a server id (a positive int32)", source, id, wide)
			}
			sid := int32(wide)
			if other, taken := out.bySID[sid]; taken {
				// The group being read is not appended yet, so its own sids map
				// to len(out.groups).
				if other == len(out.groups) {
					return Groups{}, fmt.Errorf("activity groups %s: group %q lists game %d twice", source, id, sid)
				}
				return Groups{}, fmt.Errorf("activity groups %s: game %d is in group %q and group %q; a game belongs to one group", source, sid, out.groups[other].ID, id)
			}
			out.bySID[sid] = len(out.groups)
			group.GameSIDs = append(group.GameSIDs, sid)
		}
		out.groups = append(out.groups, group)
	}
	return out, nil
}

// Of is the group gameSID belongs to.
func (g Groups) Of(gameSID int32) (Group, bool) {
	index, ok := g.bySID[gameSID]
	if !ok {
		return Group{}, false
	}
	return g.groups[index].clone(), true
}

// checkExpected is the coordinator's half of the file (RR-20261006-17): the
// expected set of a window opened under groupID must be drawn from that group.
// A subset is normal — a game passes the members that are live — but a sid
// from another group or from no group, or a group the file does not define,
// means the opener and the coordinator disagree about who is in the group:
// the window would wait for a game that never looks at it, or hand a result to
// one that is not part of the activity. The error names the file, the group
// and the sid, and wraps ErrInvalid so the RPC reports a caller error.
func (g Groups) checkExpected(groupID string, expected []int32) error {
	index, ok := g.byID[groupID]
	if !ok {
		return fmt.Errorf("%w: group %q is not defined in activity groups file %s (groups: %s)",
			ErrInvalid, groupID, g.source, strings.Join(g.IDs(), ", "))
	}
	for _, sid := range expected {
		other, member := g.bySID[sid]
		switch {
		case !member:
			return fmt.Errorf("%w: game %d is expected by a window of group %q but is in no group of activity groups file %s",
				ErrInvalid, sid, groupID, g.source)
		case other != index:
			return fmt.Errorf("%w: game %d is expected by a window of group %q but belongs to group %q in activity groups file %s",
				ErrInvalid, sid, groupID, g.groups[other].ID, g.source)
		}
	}
	return nil
}

// IDs is every group id, in file order.
func (g Groups) IDs() []string {
	out := make([]string, 0, len(g.groups))
	for _, group := range g.groups {
		out = append(out, group.ID)
	}
	return out
}

// Source names the file the groups were read from, for errors that point an
// operator at it.
func (g Groups) Source() string { return g.source }

func (g Group) clone() Group {
	g.GameSIDs = append([]int32(nil), g.GameSIDs...)
	return g
}
