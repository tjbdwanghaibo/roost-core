package account

// B9：建角判定表的每个格子（名额状态 × 入口 × 名字状态 → 动作）。
//
// 这张表是规格，独立于 decideCreation 的写法：改了表里任何一格，这里必须同时改，评审时能直接看到
// 哪个入口在哪个事实上换了动作。行为层面的回归仍是 RR-20260929-19、RR-20261001-06 与其残余的既有用例
// （rr_20260929_round3_test.go、pending_creation_*_promises_test.go），它们走真实的 Service。

import (
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/kit/service/directory"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// Column order of every row below.
var tableNames = []nameState{nameNotRead, nameFree, nameReservedByPlan, nameCommittedByPlan, nameReservedElsewhere, nameCommittedElsewhere}

// Cell codes.
var tableActions = map[string]creationAction{
	"plan":    actMakePlan,
	"role":    actReturnRole,
	"resume":  actResume,
	"read":    actReadName,
	"retry":   actReleaseAndRetry,
	"taken-r": actReleaseRefuseNameTaken,
	"taken":   actRefuseNameTaken,
	"limit":   actRefuseRoleLimit,
	"confl":   actRefuseConflict,
	"release": actResolveRelease,
	"unres":   actRefuseUnresolvable,
}

// creationTableSpec is the table: one row per (slot, entry), one column per
// name state. Rows for a pending plan carry the reasons in comments.
const creationTableSpec = `
# slot        entry           not-read  free     by-plan  com-plan  res-else  com-else
absent        same            plan      plan     plan     plan      plan      plan
absent        same-refused    plan      plan     plan     plan      plan      plan
absent        other           plan      plan     plan     plan      plan      plan
absent        resolve         unres     unres    unres    unres     unres     unres

foreign       same            confl     confl    confl    confl     confl     confl
foreign       same-refused    confl     confl    confl    confl     confl     confl
foreign       other           confl     confl    confl    confl     confl     confl
foreign       resolve         confl     confl    confl    confl     confl     confl

legacy        same            confl     confl    confl    confl     confl     confl
legacy        same-refused    confl     confl    confl    confl     confl     confl
legacy        other           confl     confl    confl    confl     confl     confl
legacy        resolve         unres     unres    unres    unres     unres     unres

# Same name resumes without reading the name: Reserve is the atomic answer.
# Refused, a never-admitted plan is released (a definite pre-role refusal);
# an admitted one only when its name is final elsewhere (RR-20261001-06).
# Another name gives way only to a final fact (RR-20261001-06 残余); the
# operator may release anything that does not hold its own name.
unadmitted    same            resume    resume   resume   resume    resume    resume
unadmitted    same-refused    read      taken-r  taken-r  taken-r   taken-r   taken-r
unadmitted    other           read      limit    limit    limit     limit     retry
unadmitted    resolve         read      release  unres    unres     release   release

admitted      same            resume    resume   resume   resume    resume    resume
admitted      same-refused    read      taken    taken    taken     taken     taken-r
admitted      other           read      limit    limit    limit     limit     retry
admitted      resolve         read      release  unres    unres     release   release

published     same            role      role     role     role      role      role
published     same-refused    role      role     role     role      role      role
published     other           limit     limit    limit    limit     limit     limit
published     resolve         unres     unres    unres    unres     unres     unres
`

func TestCreationTableEveryCell(t *testing.T) {
	slots := map[string]slotState{
		"absent": slotAbsent, "foreign": slotForeign, "legacy": slotLegacy,
		"unadmitted": slotPendingUnadmitted, "admitted": slotPendingAdmitted, "published": slotPublished,
	}
	entries := map[string]creationEntry{
		"same": entrySameName, "same-refused": entrySameNameRefused, "other": entryOtherName, "resolve": entryResolve,
	}
	seen := map[[2]string]bool{}
	covered := 0
	for _, line := range strings.Split(creationTableSpec, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if len(fields) != 2+len(tableNames) {
			t.Fatalf("row %q has %d fields, want %d", line, len(fields), 2+len(tableNames))
		}
		slot, ok := slots[fields[0]]
		entry, ok2 := entries[fields[1]]
		if !ok || !ok2 {
			t.Fatalf("row %q: unknown slot or entry", line)
		}
		if seen[[2]string{fields[0], fields[1]}] {
			t.Fatalf("row %q appears twice", line)
		}
		seen[[2]string{fields[0], fields[1]}] = true
		for i, code := range fields[2:] {
			want, ok := tableActions[code]
			if !ok {
				t.Fatalf("row %q: unknown cell code %q", line, code)
			}
			if got := decideCreation(slot, entry, tableNames[i]); got != want {
				t.Errorf("%s / %s / name=%d: decideCreation = %d, table says %s (%d)", fields[0], fields[1], tableNames[i], got, code, want)
			}
			covered++
		}
	}
	// Every combination is in the table exactly once.
	if want := len(slots) * len(entries) * len(tableNames); covered != want {
		t.Fatalf("table covers %d cells, want %d", covered, want)
	}
}

// The two classifiers are what createRole and Admin share; their edges are
// the facts the RR chain kept re-deciding.
func TestCreationTableClassifiers(t *testing.T) {
	plan := RoleCreation{ID: "plan-1", PlayerID: 42, Name: "Hero"}
	slotOf := func(mutate func(*Slot)) versionstore.Versioned[Slot] {
		s := Slot{AccountID: "a", ServerID: 1, Creation: plan}
		mutate(&s)
		return versionstore.Versioned[Slot]{Value: s, Version: 1}
	}
	for _, tc := range []struct {
		name  string
		slot  versionstore.Versioned[Slot]
		found bool
		want  slotState
	}{
		{"absent", versionstore.Versioned[Slot]{}, false, slotAbsent},
		{"other account", slotOf(func(s *Slot) { s.AccountID = "b" }), true, slotForeign},
		{"other server", slotOf(func(s *Slot) { s.ServerID = 2 }), true, slotForeign},
		{"published", slotOf(func(s *Slot) { s.PlayerID = 42 }), true, slotPublished},
		{"published legacy", slotOf(func(s *Slot) { s.PlayerID = 42; s.Creation = RoleCreation{} }), true, slotPublished},
		{"legacy empty", slotOf(func(s *Slot) { s.Creation = RoleCreation{} }), true, slotLegacy},
		{"plan without player id", slotOf(func(s *Slot) { s.Creation.PlayerID = 0 }), true, slotLegacy},
		{"pending", slotOf(func(*Slot) {}), true, slotPendingUnadmitted},
		{"admitted", slotOf(func(s *Slot) { s.Creation.Admitted = true }), true, slotPendingAdmitted},
	} {
		if got := classifySlot(tc.slot, tc.found, "a", 1); got != tc.want {
			t.Errorf("classifySlot(%s) = %d, want %d", tc.name, got, tc.want)
		}
	}

	owner := directory.Owner("a@1/plan-1")
	expires := time.Unix(1_700_000_060, 0).Unix()
	for _, tc := range []struct {
		name  string
		entry directory.Entry
		found bool
		want  nameState
	}{
		{"free", directory.Entry{}, false, nameFree},
		{"reserved by plan", directory.Entry{Owner: owner, State: directory.StateReserved, ExpiresAtUnix: expires}, true, nameReservedByPlan},
		{"committed by plan", directory.Entry{Owner: owner, State: directory.StateCommitted}, true, nameCommittedByPlan},
		{"reserved elsewhere", directory.Entry{Owner: "b@1/plan-9", State: directory.StateReserved, ExpiresAtUnix: expires}, true, nameReservedElsewhere},
		{"committed elsewhere", directory.Entry{Owner: "b@1/plan-9", State: directory.StateCommitted}, true, nameCommittedElsewhere},
		// Another attempt of the same account and slot is another owner: an
		// abandoned plan cannot claim a later plan's name (creationOwner).
		{"committed by a later plan", directory.Entry{Owner: "a@1/plan-2", State: directory.StateCommitted}, true, nameCommittedElsewhere},
	} {
		if got := classifyName(tc.entry, tc.found, owner); got != tc.want {
			t.Errorf("classifyName(%s) = %d, want %d", tc.name, got, tc.want)
		}
	}

	for _, tc := range []struct {
		slot Slot
		name string
		want creationEntry
	}{
		{Slot{Creation: plan}, "Hero", entrySameName},
		{Slot{Creation: plan}, "Knight", entryOtherName},
		{Slot{}, "Hero", entryOtherName}, // no creation identity: nothing to match
		{Slot{Creation: RoleCreation{Name: "Hero"}}, "Hero", entryOtherName},
	} {
		if got := creationEntryFor(tc.slot, tc.name); got != tc.want {
			t.Errorf("creationEntryFor(%+v, %q) = %d, want %d", tc.slot.Creation, tc.name, got, tc.want)
		}
	}
}
