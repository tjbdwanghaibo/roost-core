// 建角：提交尾部异常不留残余、结果未知只留一个可恢复计划、判定表、名字与名额的丢回复恢复、死计划释放只计一次。

package account

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/security"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
	"github.com/tjbdwanghaibo/roost-core/service/directory"
)

// slotsVanishingOnce makes the slot row disappear exactly when CreateRole
// comes back to record the player on it — the row was deleted between the
// reservation and the final write.
type slotsVanishingOnce struct {
	versionstore.Store[string, Slot]
	vanished bool
}

func (s *slotsVanishingOnce) Update(ctx context.Context, key string, mutate versionstore.Mutate[Slot]) (versionstore.Versioned[Slot], bool, error) {
	if !s.vanished {
		s.vanished = true
		_, _, err := mutate(Slot{}, false)
		return versionstore.Versioned[Slot]{}, false, err
	}
	return s.Store.Update(ctx, key, mutate)
}

// The three commit-tail refusals of CreateRole — a slot that vanished before the final write, an allocated player id already in use,
// and an allocator handing out zero — each surface as an error, and each leaves nothing behind: the same account retries and succeeds.
func TestCreateRoleRefusesEachCommitTailAnomalyAndLeavesNothingBehind(t *testing.T) {
	const fixedID = int64(4242)
	cases := []struct {
		name    string
		mutate  func(*Config)
		seed    func(*testing.T, Config)
		want    error
		message string
	}{
		{"slot vanished before the final write", func(cfg *Config) { cfg.Slots = &slotsVanishingOnce{Store: cfg.Slots} }, nil,
			ErrConflict, "vanished during create"},
		{"player id already in use", nil, func(t *testing.T, cfg Config) {
			if _, created, err := cfg.Roles.Create(context.Background(), fixedID, Role{PlayerID: fixedID, AccountID: "someone-else", ServerID: 1, Name: "Bob"}); err != nil || !created {
				t.Fatalf("seed role: created=%v err=%v", created, err)
			}
		}, ErrConflict, "player id 4242 is already in use"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			allocations := 0
			service, _, cfg := newService(t, func(cfg *Config) {
				if tc.mutate != nil {
					tc.mutate(cfg)
				}
				cfg.Allocator = AllocatorFunc(func(context.Context, int32) (int64, error) {
					allocations++
					return fixedID + int64(allocations) - 1, nil
				})
			})
			if tc.seed != nil {
				tc.seed(t, cfg)
			}
			owner := login(t, service, "u1")
			_, err := service.CreateRole(ctx, owner.ID, 1, "Alice")
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("CreateRole = %v; want %v containing %q", err, tc.want, tc.message)
			}
			if tc.seed == nil {
				if _, found, _ := cfg.Roles.Get(ctx, fixedID); found {
					t.Fatal("the role record survived a failed create")
				}
			}
			// The retry is a clean first attempt: slot free, name free.
			role, err := service.CreateRole(ctx, owner.ID, 1, "Alice")
			if err != nil {
				t.Fatalf("retry after a rolled-back create failed: %v", err)
			}
			if role.Name != "Alice" || role.AccountID != owner.ID {
				t.Fatalf("retry produced %+v", role)
			}
		})
	}
}

// An allocator that returns zero is refused before any role is written; zero
// is the "no player" value everywhere else in the system.
func TestCreateRoleRefusesAZeroPlayerIDAndRollsBack(t *testing.T) {
	ctx := context.Background()
	calls := 0
	service, _, cfg := newService(t, func(cfg *Config) {
		cfg.Allocator = AllocatorFunc(func(context.Context, int32) (int64, error) {
			calls++
			if calls == 1 {
				return 0, nil
			}
			return 7000 + int64(calls), nil
		})
	})
	owner := login(t, service, "u1")
	if _, err := service.CreateRole(ctx, owner.ID, 1, "Alice"); err == nil || !strings.Contains(err.Error(), "allocator returned a zero player id") {
		t.Fatalf("CreateRole with a zero player id = %v", err)
	}
	if _, found, _ := cfg.Roles.Get(ctx, 0); found {
		t.Fatal("a role was recorded under player id 0")
	}
	if _, err := service.CreateRole(ctx, owner.ID, 1, "Alice"); err != nil {
		t.Fatalf("retry after the zero id failed: %v", err)
	}
}

func (s *slotsVanishingOnce) DeleteIf(ctx context.Context, key string, expect versionstore.Versioned[Slot], match func(Slot) bool) error {
	return s.Store.(versionstore.ConditionalDeleter[string, Slot]).DeleteIf(ctx, key, expect, match)
}

// namesFailingCommitOnce is a directory whose first Commit is lost — the
// round trip that never came back — while every other operation is real.
type namesFailingCommitOnce struct {
	directory.Directory
	failed bool
}

func (n *namesFailingCommitOnce) Commit(ctx context.Context, claim directory.Claim) (directory.Entry, error) {
	if !n.failed {
		n.failed = true
		return directory.Entry{}, fmt.Errorf("names: connection reset")
	}
	return n.Directory.Commit(ctx, claim)
}

// slotsFailingUpdateOnce loses the final slot write of a create.
type slotsFailingUpdateOnce struct {
	versionstore.Store[string, Slot]
	failed bool
}

func (s *slotsFailingUpdateOnce) Update(ctx context.Context, key string, mutate versionstore.Mutate[Slot]) (versionstore.Versioned[Slot], bool, error) {
	if !s.failed {
		s.failed = true
		return versionstore.Versioned[Slot]{}, false, fmt.Errorf("slots: connection reset")
	}
	return s.Store.Update(ctx, key, mutate)
}

// Unknown writes retain one durable plan. Retry uses the same ID instead of
// assuming a failed response means that a possibly committed role is absent.
func TestACreateWithAnUnknownWriteRetainsOneRecoverablePlan(t *testing.T) {
	const fixedID = int64(4242)
	for name, mutate := range map[string]func(*Config){
		"name commit lost": func(cfg *Config) { cfg.Names = &namesFailingCommitOnce{Directory: cfg.Names} },
		"slot write lost":  func(cfg *Config) { cfg.Slots = &slotsFailingUpdateOnce{Store: cfg.Slots} },
	} {
		t.Run(name, func(t *testing.T) {
			sink := servicemetrics.NewRecorder()
			service, _, cfg := newService(t, mutate, func(cfg *Config) {
				cfg.Allocator = AllocatorFunc(func(context.Context, int32) (int64, error) { return fixedID, nil })
				cfg.Metrics = sink
			})
			ctx := context.Background()
			owner := login(t, service, "u1")
			other := login(t, service, "u2")

			if _, err := service.CreateRole(ctx, owner.ID, 1, "Alice"); err == nil {
				t.Fatal("a create whose commit tail failed reported success")
			}
			slot, found, err := cfg.Slots.Get(ctx, slotKeyFor(owner.ID, 1))
			if err != nil || !found || slot.Value.Creation.ID == "" || slot.Value.Creation.PlayerID != fixedID {
				t.Fatalf("unknown write lost its recovery plan: %+v %v %v", slot, found, err)
			}
			if stored, exists, err := cfg.Roles.Get(ctx, fixedID); err != nil || (exists && stored.Value.CreationID != slot.Value.Creation.ID) {
				t.Fatalf("role is not linked to recovery intent: %+v %v", stored, err)
			}
			if got := sink.Count("accepted:create_role"); got != 0 {
				t.Fatalf("a failed create was counted as accepted (%d); %s", got, sink.Events())
			}
			if got := sink.Count("dropped:rollback.failed"); got != 0 {
				t.Fatalf("the rollback reported %d failures; %s", got, sink.Events())
			}

			// The retry resumes this plan, including a possibly stored role.
			role, err := service.CreateRole(ctx, owner.ID, 1, "Alice")
			if err != nil {
				t.Fatalf("the retry after a rolled-back create failed: %v", err)
			}
			if role.PlayerID != fixedID || role.Name != "Alice" {
				t.Fatalf("retry produced %+v", role)
			}
			// And the committed name is really committed: another account is
			// refused, not admitted after some lapse.
			if _, err := service.CreateRole(ctx, other.ID, 1, "alice"); !errors.Is(err, ErrNameTaken) {
				t.Fatalf("another account took a committed name: %v", err)
			}
		})
	}
}

func (s *slotsFailingUpdateOnce) DeleteIf(ctx context.Context, key string, expect versionstore.Versioned[Slot], match func(Slot) bool) error {
	return s.Store.(versionstore.ConditionalDeleter[string, Slot]).DeleteIf(ctx, key, expect, match)
}

// 建角判定表的每个格子（名额状态 × 入口 × 名字状态 → 动作）。这张表是规格，独立于 decideCreation 的写法：改了表里任何一格，这里必须
// 同时改。行为层面的回归是本文件与 pending_contract_test.go 里走真实 Service 的用例。

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
# Another name gives way to a final fact (RR-20261001-06 残余), and — for a
# never-admitted plan — also to a foreign reservation, the fact on which a
# same-name retry already releases it (维护者第五轮决定 2026-10-06), and to a
# free name: nothing holds it, so no attempt of the plan is in flight past
# Reserve (O37, 维护者第七轮决定 2026-10-06); the operator may release
# anything that does not hold its own name.
unadmitted    same            resume    resume   resume   resume    resume    resume
unadmitted    same-refused    read      taken-r  taken-r  taken-r   taken-r   taken-r
unadmitted    other           read      retry    limit    limit     retry     retry
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

type reviewFailSlot struct {
	versionstore.Store[string, Slot]
	fail bool
}

func (s *reviewFailSlot) Update(ctx context.Context, k string, m versionstore.Mutate[Slot]) (versionstore.Versioned[Slot], bool, error) {
	if s.fail {
		return versionstore.Versioned[Slot]{}, false, fmt.Errorf("injected slot write failure")
	}
	return s.Store.Update(ctx, k, m)
}
func TestCrossServerRoleRollbackKeepsExistingName(t *testing.T) {
	slots := &reviewFailSlot{Store: versionstore.NewMemoryStore[string, Slot]()}
	s, _, cfg := newService(t, func(c *Config) { c.Slots = slots })
	ctx := context.Background()
	a, err := s.Login(ctx, Identity{Channel: "test", OpenID: "same", Credential: "good"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpsertServer(ctx, GameServer{ID: 2, Status: ServerOpen}); err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRole(ctx, a.ID, 1, "Hero")
	if err != nil {
		t.Fatal(err)
	}
	slots.fail = true
	if _, err = s.CreateRole(ctx, a.ID, 2, "Hero"); err == nil {
		t.Fatal("expected failure")
	}
	if _, found, err := cfg.Roles.Get(ctx, r.PlayerID); err != nil || !found {
		t.Fatal("original role missing", err)
	}
	if _, found, err := cfg.Names.Lookup(ctx, "Hero"); err != nil || !found {
		t.Fatalf("second creation rollback removed first role name: found=%v err=%v", found, err)
	}
}

func (s *reviewFailSlot) DeleteIf(ctx context.Context, key string, expect versionstore.Versioned[Slot], match func(Slot) bool) error {
	return s.Store.(versionstore.ConditionalDeleter[string, Slot]).DeleteIf(ctx, key, expect, match)
}

type review3FailCreate[K comparable, V any] struct {
	versionstore.Store[K, V]
	failed bool
}

func (s *review3FailCreate[K, V]) Create(ctx context.Context, k K, v V) (versionstore.Versioned[V], bool, error) {
	if !s.failed {
		s.failed = true
		return versionstore.Versioned[V]{}, false, errors.New("control: before write")
	}
	return s.Store.Create(ctx, k, v)
}

type review3FailCommit struct {
	directory.Directory
	failed bool
}

func (s *review3FailCommit) Commit(ctx context.Context, c directory.Claim) (directory.Entry, error) {
	if !s.failed {
		s.failed = true
		return directory.Entry{}, errors.New("control: before commit")
	}
	return s.Directory.Commit(ctx, c)
}
func TestBeforeWriteFailuresRemainRetryable(t *testing.T) {
	for _, point := range []string{"slot", "role", "name"} {
		t.Run(point, func(t *testing.T) {
			s, _, _ := review3AccountService(t, func(c *Config) {
				switch point {
				case "slot":
					c.Slots = &review3FailCreate[string, Slot]{Store: c.Slots}
				case "role":
					c.Roles = &review3FailCreate[int64, Role]{Store: c.Roles}
				case "name":
					c.Names = &review3FailCommit{Directory: c.Names}
				}
			})
			ctx := context.Background()
			a, e := s.Login(ctx, Identity{Channel: "test", OpenID: "control", Credential: "good"})
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.CreateRole(ctx, a.ID, 1, "Hero"); e == nil {
				t.Fatal("control must reject first operation")
			}
			if _, e = s.CreateRole(ctx, a.ID, 1, "Hero"); e != nil {
				t.Fatalf("before-write failure should permit clean retry: %v", e)
			}
		})
	}
}

func review3AccountService(t *testing.T, mutate ...func(*Config)) (*Service, *clock, Config) {
	t.Helper()
	if os.Getenv("ROOST_REVIEW3_BACKEND") != "redis" {
		return newService(t, mutate...)
	}
	addr := os.Getenv("ROOST_REVIEW_REDIS")
	if addr == "" {
		t.Fatal("Redis backend requires ROOST_REVIEW_REDIS")
	}
	client, err := driver.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	stores, err := NewRedisStores(client, fmt.Sprintf("account-review3:%d", time.Now().UnixNano()), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	base := func(c *Config) {
		c.Accounts = stores.Accounts
		c.Roles = stores.Roles
		c.Servers = stores.Servers
		c.Slots = stores.Slots
		c.Names = stores.Names
	}
	return newService(t, append([]func(*Config){base}, mutate...)...)
}

type review3LostCommit struct {
	directory.Directory
	lost bool
}

func (n *review3LostCommit) Commit(ctx context.Context, c directory.Claim) (directory.Entry, error) {
	v, e := n.Directory.Commit(ctx, c)
	if e == nil && !n.lost {
		n.lost = true
		return directory.Entry{}, errors.New("name commit reply lost")
	}
	return v, e
}
func TestNameCommitLostReplyCannotBurnName(t *testing.T) {
	s, _, cfg := review3AccountService(t, func(c *Config) { c.Names = &review3LostCommit{Directory: c.Names} })
	ctx := context.Background()
	a, e := s.Login(ctx, Identity{Channel: "test", OpenID: "name-lost", Credential: "good"})
	if e != nil {
		t.Fatal(e)
	}
	_, firstErr := s.CreateRole(ctx, a.ID, 1, "Hero")
	if firstErr == nil {
		t.Fatal("fixture must lose the first commit reply")
	}
	name, found, e := cfg.Names.Lookup(ctx, "Hero")
	if e != nil {
		t.Fatal(e)
	}
	slot, slotFound, e := cfg.Slots.Get(ctx, slotKeyFor(a.ID, 1))
	if e != nil {
		t.Fatal(e)
	}
	recovered, retryErr := s.CreateRole(ctx, a.ID, 1, "Hero")
	if retryErr != nil || !found || name.State != directory.StateCommitted || !slotFound || recovered.PlayerID != slot.Value.Creation.PlayerID {
		t.Fatalf("committed name burned: name=%+v slot=%+v slotFound=%v retry=%v", name, slot, slotFound, retryErr)
	}
}

type review3LostRole struct {
	versionstore.Store[int64, Role]
	lost  bool
	first int64
}

func (s *review3LostRole) Create(ctx context.Context, k int64, v Role) (versionstore.Versioned[Role], bool, error) {
	got, new, e := s.Store.Create(ctx, k, v)
	if e == nil && new && !s.lost {
		s.lost = true
		s.first = k
		return versionstore.Versioned[Role]{}, false, errors.New("role create reply lost")
	}
	return got, new, e
}
func TestRoleCreateLostReplyCannotLeaveDuplicateNames(t *testing.T) {
	var roles *review3LostRole
	s, _, _ := review3AccountService(t, func(c *Config) { roles = &review3LostRole{Store: c.Roles}; c.Roles = roles })
	ctx := context.Background()
	a, e := s.Login(ctx, Identity{Channel: "test", OpenID: "role-lost", Credential: "good"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.CreateRole(ctx, a.ID, 1, "Hero"); e == nil {
		t.Fatal("expected lost reply")
	}
	second, e := s.CreateRole(ctx, a.ID, 1, "Hero")
	if e != nil {
		t.Fatal(e)
	}
	orphan, found, e := roles.Get(ctx, roles.first)
	if e != nil {
		t.Fatal(e)
	}
	if !found || orphan.Value.PlayerID != second.PlayerID || orphan.Value.CreationID != second.CreationID {
		t.Fatalf("two persisted roles share account/server/name: first=%+v second=%+v", orphan.Value, second)
	}
}

type review3LostSlotCreate struct {
	versionstore.Store[string, Slot]
	lost bool
}

func (s *review3LostSlotCreate) Create(ctx context.Context, k string, v Slot) (versionstore.Versioned[Slot], bool, error) {
	got, new, e := s.Store.Create(ctx, k, v)
	if e == nil && new && !s.lost {
		s.lost = true
		return versionstore.Versioned[Slot]{}, false, errors.New("slot create reply lost")
	}
	return got, new, e
}
func TestInitialSlotUnknownCannotPermanentlyBlockCreate(t *testing.T) {
	s, clock, cfg := review3AccountService(t, func(c *Config) { c.Slots = &review3LostSlotCreate{Store: c.Slots} })
	ctx := context.Background()
	a, e := s.Login(ctx, Identity{Channel: "test", OpenID: "slot-first", Credential: "good"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.CreateRole(ctx, a.ID, 1, "Hero"); e == nil {
		t.Fatal("expected lost reply")
	}
	clock.advance(24 * time.Hour)
	recovered, retryErr := s.CreateRole(ctx, a.ID, 1, "Hero")
	slot, found, e := cfg.Slots.Get(ctx, slotKeyFor(a.ID, 1))
	if e != nil {
		t.Fatal(e)
	}
	if retryErr != nil || !found || slot.Value.PlayerID == 0 || slot.Value.PlayerID != recovered.PlayerID {
		t.Fatalf("empty permanent slot blocks recovered client: slot=%+v retry=%v", slot.Value, retryErr)
	}
}
func TestProfileOutputCannotMutateRoleWithoutCAS(t *testing.T) {
	for _, method := range []string{"update", "validate", "create_retry"} {
		t.Run(method, func(t *testing.T) {
			s, _, cfg := review3AccountService(t)
			ctx := context.Background()
			a, e := s.Login(ctx, Identity{Channel: "test", OpenID: "profile", Credential: "good"})
			if e != nil {
				t.Fatal(e)
			}
			role, e := s.CreateRole(ctx, a.ID, 1, "Hero")
			if e != nil {
				t.Fatal(e)
			}
			out, e := s.UpdateProfile(ctx, a.ID, role.PlayerID, []byte("safe"))
			if e != nil {
				t.Fatal(e)
			}
			if method == "validate" {
				session, e := s.SelectRole(ctx, a.ID, role.PlayerID)
				if e != nil {
					t.Fatal(e)
				}
				out, e = s.ValidateSession(ctx, role.PlayerID, session.Token)
				if e != nil {
					t.Fatal(e)
				}
			}
			if method == "create_retry" {
				out, e = s.CreateRole(ctx, a.ID, 1, "Hero")
				if e != nil {
					t.Fatal(e)
				}
			}
			before, _, e := cfg.Roles.Get(ctx, role.PlayerID)
			if e != nil {
				t.Fatal(e)
			}
			out.Profile[0] = 'X'
			after, _, e := cfg.Roles.Get(ctx, role.PlayerID)
			if e != nil {
				t.Fatal(e)
			}
			if string(after.Value.Profile) != "safe" || after.Version != before.Version {
				t.Fatalf("output mutated stored role: value=%q version=%d/%d", after.Value.Profile, before.Version, after.Version)
			}
		})
	}
}

func TestPendingRoleRecoversAcrossServiceRestart(t *testing.T) {
	var roles *review3LostRole
	s, _, cfg := review3AccountService(t, func(c *Config) { roles = &review3LostRole{Store: c.Roles}; c.Roles = roles })
	ctx := context.Background()
	a := login(t, s, "restart")
	if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
		t.Fatal("expected unknown Create")
	}
	// A pending role is durable recovery state, not a playable published role.
	if _, err := s.SelectRole(ctx, a.ID, roles.first); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending role selected: %v", err)
	}
	token, err := security.SignSessionToken(roles.first, cfg.SessionSecret, time.Hour, cfg.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ValidateSession(ctx, roles.first, token); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending token accepted: %v", err)
	}
	if _, err = s.UpdateProfile(ctx, a.ID, roles.first, []byte("premature")); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending profile updated: %v", err)
	}
	restarted, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := restarted.CreateRole(ctx, a.ID, 1, "Hero")
	if err != nil || recovered.PlayerID != roles.first {
		t.Fatalf("restart lost creation identity: %+v %v", recovered, err)
	}
	if _, err = restarted.SelectRole(ctx, a.ID, recovered.PlayerID); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentCreationRetriesShareOneDurablePlan(t *testing.T) {
	s, _, cfg := review3AccountService(t, func(c *Config) { c.Slots = &review3LostSlotCreate{Store: c.Slots} })
	ctx := context.Background()
	a := login(t, s, "concurrent-retry")
	if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
		t.Fatal("expected lost initial slot reply")
	}
	slot, found, err := cfg.Slots.Get(ctx, slotKeyFor(a.ID, 1))
	if err != nil || !found {
		t.Fatal(slot, found, err)
	}
	var group sync.WaitGroup
	type result struct {
		role Role
		err  error
	}
	results := make(chan result, 16)
	for range 16 {
		group.Go(func() { role, err := s.CreateRole(ctx, a.ID, 1, "Hero"); results <- result{role, err} })
	}
	group.Wait()
	close(results)
	for got := range results {
		if got.err != nil || got.role.PlayerID != slot.Value.Creation.PlayerID || got.role.CreationID != slot.Value.Creation.ID {
			t.Fatalf("retry forked plan: role=%+v err=%v", got.role, got.err)
		}
	}
}

func TestLegacyEmptySlotRequiresExplicitReconciliation(t *testing.T) {
	s, _, cfg := newService(t)
	ctx := context.Background()
	a := login(t, s, "legacy-slot")
	original, _, err := cfg.Slots.Create(ctx, slotKeyFor(a.ID, 1), Slot{AccountID: a.ID, ServerID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateRole(ctx, a.ID, 1, "Hero"); !errors.Is(err, ErrConflict) {
		t.Fatalf("legacy empty slot was guessed away: %v", err)
	}
	current, found, err := cfg.Slots.Get(ctx, slotKeyFor(a.ID, 1))
	if err != nil || !found || current.Version != original.Version {
		t.Fatal(current, found, err)
	}
}

func TestAccountRequiresIdentityCheckedSlotCleanup(t *testing.T) {
	_, _, cfg := newService(t)
	cfg.Slots = struct {
		versionstore.Store[string, Slot]
	}{cfg.Slots}
	if _, err := New(cfg); err == nil {
		t.Fatal("constructor accepted unsafe slot cleanup")
	}
}

// A name COMMITTED to another owner releases the pending slot (the plan can never commit it), while the orphan role the
// lost reply created stays unplayable. A name merely RESERVED by someone else still retains the proof — see
// TestNameReservedElsewhereKeepsThePendingSlot.
func TestPendingRoleNameConflictReleasesSlotButNotTheOrphanRole(t *testing.T) {
	var roles *review3LostRole
	s, clock, cfg := newService(t, func(c *Config) { roles = &review3LostRole{Store: c.Roles}; c.Roles = roles })
	ctx := context.Background()
	a := login(t, s, "name-conflict")
	if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
		t.Fatal("expected lost role reply")
	}
	clock.advance(time.Minute)
	rival, err := cfg.Names.Reserve(ctx, "Hero", "rival", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cfg.Names.Commit(ctx, rival); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateRole(ctx, a.ID, 1, "Hero"); !errors.Is(err, ErrNameTaken) {
		t.Fatal(err)
	}
	slot, found, err := cfg.Slots.Get(ctx, slotKeyFor(a.ID, 1))
	if err != nil || found {
		t.Fatalf("slot retained after its name was committed elsewhere: %+v found=%v err=%v", slot, found, err)
	}
	if _, err = s.SelectRole(ctx, a.ID, roles.first); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicted pending role became playable: %v", err)
	}
	orphan, found, err := cfg.Roles.Get(ctx, roles.first)
	if err != nil || !found || orphan.Value.Name != "Hero" {
		t.Fatalf("orphan role record was deleted or changed: %+v found=%v err=%v", orphan, found, err)
	}
}

func (s *review3LostSlotCreate) DeleteIf(ctx context.Context, key string, expect versionstore.Versioned[Slot], match func(Slot) bool) error {
	return s.Store.(versionstore.ConditionalDeleter[string, Slot]).DeleteIf(ctx, key, expect, match)
}

func (s *review3FailCreate[K, V]) DeleteIf(ctx context.Context, key K, expect versionstore.Versioned[V], match func(V) bool) error {
	return s.Store.(versionstore.ConditionalDeleter[K, V]).DeleteIf(ctx, key, expect, match)
}

func TestCommittedLegacyNameOwnerIsNeverCompensated(t *testing.T) {
	s, _, cfg := newService(t)
	ctx := context.Background()
	a, err := s.Login(ctx, Identity{Channel: "test", OpenID: "legacy-name", Credential: "good"})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an old bare account owner matching the new account/server owner.
	claim, err := cfg.Names.Reserve(ctx, "Held", directory.Owner(slotKeyFor(a.ID, 1)), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cfg.Names.Commit(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateRole(ctx, a.ID, 1, "Held"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("existing name reused: %v", err)
	}
	if _, found, err := cfg.Names.Lookup(ctx, "Held"); !found || err != nil {
		t.Fatal(found, err)
	}
	if _, found, err := cfg.Slots.Get(ctx, slotKeyFor(a.ID, 1)); found || err != nil {
		t.Fatal(found, err)
	}
}

type unknownSlotOutcome struct {
	versionstore.Store[string, Slot]
	failed     bool
	readFailed bool
}

func (s *unknownSlotOutcome) Update(ctx context.Context, key string, m versionstore.Mutate[Slot]) (versionstore.Versioned[Slot], bool, error) {
	v, saved, err := s.Store.Update(ctx, key, m)
	if err == nil && saved && v.Value.PlayerID != 0 {
		s.failed = true
		return versionstore.Versioned[Slot]{}, false, errors.New("lost write reply")
	}
	return v, saved, err
}
func (s *unknownSlotOutcome) Get(ctx context.Context, key string) (versionstore.Versioned[Slot], bool, error) {
	if s.failed && !s.readFailed {
		s.readFailed = true
		return versionstore.Versioned[Slot]{}, false, errors.New("reconciliation unavailable")
	}
	return s.Store.Get(ctx, key)
}
func TestUnknownSlotOutcomePreservesRoleAndName(t *testing.T) {
	store := &unknownSlotOutcome{Store: versionstore.NewMemoryStore[string, Slot]()}
	s, _, cfg := newService(t, func(c *Config) { c.Slots = store })
	ctx := context.Background()
	a, err := s.Login(ctx, Identity{Channel: "test", OpenID: "unknown", Credential: "good"})
	if err != nil {
		t.Fatal(err)
	}
	role, err := s.CreateRole(ctx, a.ID, 1, "Retained")
	if err == nil || role.PlayerID == 0 {
		t.Fatalf("unknown result omitted recovery identity: %+v %v", role, err)
	}
	slot, found, err := store.Get(ctx, slotKeyFor(a.ID, 1))
	if err != nil || !found || slot.Value.PlayerID != role.PlayerID {
		t.Fatal(slot, found, err)
	}
	if _, found, err := cfg.Roles.Get(ctx, role.PlayerID); !found || err != nil {
		t.Fatal(found, err)
	}
	if _, found, err := cfg.Names.Lookup(ctx, "Retained"); !found || err != nil {
		t.Fatal(found, err)
	}
}

func TestLegacyVerifiedIdentityRetainsAccountAndRefusesCollision(t *testing.T) {
	s, _, cfg := newService(t, func(c *Config) {
		c.Verifier = VerifierFunc(func(_ context.Context, i Identity) (Verified, error) {
			if i.Credential == "first" {
				return Verified{Channel: "vendor:a", OpenID: "b"}, nil
			}
			return Verified{Channel: "vendor", OpenID: "a:b"}, nil
		})
	})
	ctx := context.Background()
	legacy := Account{ID: "vendor:a:b", Channel: "vendor:a", OpenID: "b"}
	if _, _, err := cfg.Accounts.Create(ctx, legacy.ID, legacy); err != nil {
		t.Fatal(err)
	}
	first, err := s.Login(ctx, Identity{Channel: "test", OpenID: "first", Credential: "first"})
	if err != nil || first.ID != legacy.ID {
		t.Fatal(first, err)
	}
	if _, err := s.Login(ctx, Identity{Channel: "test", OpenID: "second", Credential: "second"}); !errors.Is(err, ErrIdentityInvalid) {
		t.Fatalf("legacy collision merged: %v", err)
	}
}

func (s *unknownSlotOutcome) DeleteIf(ctx context.Context, key string, expect versionstore.Versioned[Slot], match func(Slot) bool) error {
	return s.Store.(versionstore.ConditionalDeleter[string, Slot]).DeleteIf(ctx, key, expect, match)
}

// 两个换名请求并发释放同一个死计划时，create_role.plan_released 只计一次：只有真正删掉 slot 的那一次计数；输掉的一方不计、不报错，
// 照常按新的 slot 判定。

// slotsDeleteBarrier holds every conditional delete until two callers have
// arrived, so both release the same observed slot version.
type slotsDeleteBarrier struct {
	versionstore.Store[string, Slot]
	pending atomic.Int32 // deletes still to be held at the barrier
	arrived sync.WaitGroup
}

func (s *slotsDeleteBarrier) DeleteIf(ctx context.Context, key string, expect versionstore.Versioned[Slot], match func(Slot) bool) error {
	if s.pending.Add(-1) >= 0 {
		s.arrived.Done()
		s.arrived.Wait()
	}
	return s.Store.(versionstore.ConditionalDeleter[string, Slot]).DeleteIf(ctx, key, expect, match)
}

func TestConcurrentReleasesOfOneDeadPlanCountOnce(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	var slots *slotsDeleteBarrier
	s, cfg, _, a, _ := revn06DeadPlan(t, func(c *Config) {
		c.Metrics = sink
		slots = &slotsDeleteBarrier{Store: c.Slots}
		slots.pending.Store(-1 << 30) // disarmed while the fixture runs
		c.Slots = slots
	})
	slots.arrived.Add(2)
	slots.pending.Store(2)
	ctx := context.Background()

	var wg sync.WaitGroup
	results := make([]error, 2)
	roles := make([]Role, 2)
	for i, name := range []string{"Knight", "Mage"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			roles[i], results[i] = s.CreateRole(ctx, a.ID, 1, name)
		}()
	}
	wg.Wait()

	won := 0
	for i, err := range results {
		switch {
		case err == nil:
			won++
			if slot := mustSlot(t, cfg, a.ID); slot.PlayerID != roles[i].PlayerID {
				t.Fatalf("winner %+v is not the slot's role: %+v", roles[i], slot)
			}
		case errors.Is(err, ErrRoleLimit):
		default:
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if won != 1 {
		t.Fatalf("%d of two concurrent different-name requests created a role, want exactly 1: %v", won, results)
	}
	if got := sink.Count("dropped:create_role.plan_released"); got != 1 {
		t.Fatalf("one dead plan released once was counted %d times; %s", got, sink.Events())
	}
	if got := sink.Count("dropped:rollback.failed"); got != 0 {
		t.Fatalf("losing the release race counted as a failed rollback: %s", sink.Events())
	}
}
