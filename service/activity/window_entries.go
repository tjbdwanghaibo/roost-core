package activity

import (
	"context"
	"log/slog"
)

// 读取已存窗口条目的唯一入口（维护者决定 B9，docs/feature/B9-C5-WINDOW-ENTRIES-ROLE-TABLE-2026-10-06.md）。
//
// 窗口记录是 sweep 的全部索引，它的条目却可能被本包以外的东西改过（存量坏记录、人工修复写错组）。
// “先验证再行动”这条不变量此前在相邻的读循环里一个一个补：RR-20260914-02 → RR-20261001-09 → 其残余 →
// NC-42 → NC-51，每次漏的都是另一个循环。所以读者不再各自判断：PendingActivities、DeliveringActivities、
// RetireDelivered 与 AdvanceExpired 都从 readWindowEntries 拿“可以行动的条目”，坏条目只出现在
// malformed 里——跳过、保留给运维（MalformedWindowEntries / RemoveMalformedWindowEntry）、按列表计数。
//
// 写路径（admitToWindow、confirmWindow、retireFromWindow 的 CAS）只按显式给出的、已验证的键改动，
// 不会碰到坏条目，所以坏条目在那里原样保留，不需要经过这里。

// WindowList names one of a stored window's three entry lists.
type WindowList string

const (
	// WindowKeys is Window.Keys: confirmed pending activities.
	WindowKeys WindowList = "keys"
	// WindowOpening is Window.Opening: admitted, creation not yet confirmed.
	WindowOpening WindowList = "opening"
	// WindowDelivering is Window.Delivering: complete, dispatches not all terminal.
	WindowDelivering WindowList = "delivering"
)

func (l WindowList) valid() bool {
	return l == WindowKeys || l == WindowOpening || l == WindowDelivering
}

// MalformedWindowEntry is a stored window entry this package will not act on.
// Every entry this package writes passes Key.Validate and belongs to its
// window's group, so one of these was written by something else.
type MalformedWindowEntry struct {
	List   WindowList `json:"list"`
	Key    Key        `json:"key"`
	Reason string     `json:"reason"`
}

// windowEntries is a stored window split into what may be acted on and what
// must be skipped.
type windowEntries struct {
	// usable is the stored record with every malformed entry left out; its
	// other fields (ScanAfter, RefusedOpens) are the stored ones.
	usable Window
	// malformed lists what was left out, in stored order.
	malformed []MalformedWindowEntry
}

// readWindowEntries is the one place that decides which stored entries of
// groupID's window may be acted on. An Opening entry's plan is not judged
// here: an entry whose activity already exists is confirmed whatever its plan
// says, so the plan only matters when the sweep is about to execute it
// (openingIntentProblem).
func readWindowEntries(groupID string, stored Window) windowEntries {
	out := windowEntries{usable: stored.clone()}
	out.usable.Keys, out.usable.Opening, out.usable.Delivering = nil, nil, nil
	for _, key := range stored.Keys {
		if reason := windowKeyProblem(key, groupID); reason != "" {
			out.malformed = append(out.malformed, MalformedWindowEntry{List: WindowKeys, Key: key, Reason: reason})
			continue
		}
		out.usable.Keys = append(out.usable.Keys, key)
	}
	for _, entry := range stored.Opening {
		if reason := windowKeyProblem(entry.Key, groupID); reason != "" {
			out.malformed = append(out.malformed, MalformedWindowEntry{List: WindowOpening, Key: entry.Key, Reason: reason})
			continue
		}
		out.usable.Opening = append(out.usable.Opening, entry)
	}
	for _, key := range stored.Delivering {
		if reason := windowKeyProblem(key, groupID); reason != "" {
			out.malformed = append(out.malformed, MalformedWindowEntry{List: WindowDelivering, Key: key, Reason: reason})
			continue
		}
		out.usable.Delivering = append(out.usable.Delivering, key)
	}
	// clone() deep-copied every Intent above, so usable owns its entries.
	return out
}

// in returns the malformed entries of one list.
func (e windowEntries) in(list WindowList) []MalformedWindowEntry {
	var out []MalformedWindowEntry
	for _, entry := range e.malformed {
		if entry.List == list {
			out = append(out, entry)
		}
	}
	return out
}

// loadWindowEntries reads groupID's window through readWindowEntries.
func (s *Service) loadWindowEntries(ctx context.Context, groupID string) (windowEntries, bool, error) {
	window, found, err := s.cfg.Windows.Get(ctx, groupID)
	if err != nil || !found {
		return windowEntries{}, found, err
	}
	return readWindowEntries(groupID, window.Value), true, nil
}

// malformedID names one reported entry: the same key can be malformed in two
// lists at once, and each is reported on its own.
type malformedID struct {
	list WindowList
	key  Key
}

// malformedReport is how each list's malformed entries reach an operator: a
// counter every tick, a log line when an entry first appears and once when it
// is gone or usable again, never on every sweep.
var malformedReport = map[WindowList]struct{ metric, appeared, cleared string }{
	WindowKeys: {
		metric:   "sweep.window_key_malformed",
		appeared: "activity: confirmed window key is malformed; entry skipped and kept until repaired",
		cleared:  "activity: confirmed window key is usable again or the entry is gone",
	},
	WindowOpening: {
		metric:   "sweep.opening_intent_malformed",
		appeared: "activity: opening intent is malformed; entry skipped and its slot kept until repaired",
		cleared:  "activity: opening intent is usable again or the entry is gone",
	},
	WindowDelivering: {
		metric:   "sweep.delivering_key_malformed",
		appeared: "activity: delivering window key is malformed; entry skipped and kept until repaired",
		cleared:  "activity: delivering window key is usable again or the entry is gone",
	},
}

// noteMalformed reports this tick's malformed entries of one list of
// groupID's window. found must be every malformed entry of that list this
// tick could see. unseen says whether an entry reported earlier but absent
// from found was simply outside this tick's bounded scan — not seeing it is
// not the same as seeing it healthy (RR-20261001-09) — and may be nil when
// the whole list was judged.
func (s *Service) noteMalformed(groupID string, list WindowList, found []MalformedWindowEntry, unseen func(Key) bool) {
	report := malformedReport[list]
	s.report.Dropped(report.metric, len(found))
	s.malformedMu.Lock()
	defer s.malformedMu.Unlock()
	if s.malformed == nil {
		s.malformed = make(map[string]map[malformedID]string)
	}
	// Keyed by the window's group: a malformed entry's own Key.GroupID is
	// exactly what cannot be trusted.
	known := s.malformed[groupID]
	if known == nil {
		known = make(map[malformedID]string)
		s.malformed[groupID] = known
	}
	current := make(map[malformedID]struct{}, len(found))
	for _, entry := range found {
		id := malformedID{list: list, key: entry.Key}
		current[id] = struct{}{}
		if known[id] == entry.Reason {
			continue
		}
		known[id] = entry.Reason
		slog.Warn(report.appeared, "group_id", groupID, "activity_id", entry.Key.ActivityID,
			"phase", entry.Key.Phase, "key_group_id", entry.Key.GroupID, "reason", entry.Reason)
	}
	for id := range known {
		if id.list != list {
			continue
		}
		if _, still := current[id]; still {
			continue
		}
		if unseen != nil && unseen(id.key) {
			continue
		}
		delete(known, id)
		slog.Info(report.cleared, "group_id", groupID, "activity_id", id.key.ActivityID, "phase", id.key.Phase)
	}
	if len(known) == 0 {
		delete(s.malformed, groupID)
	}
}
