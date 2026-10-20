package game

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	gameactivity "example.com/planet/game/activity"
	world "example.com/planet/game/entities/world"
	"example.com/planet/game/flags"
	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	coreentity "github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"github.com/tjbdwanghaibo/roost-core/infra/base/featureflag"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/redistest"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
	svcactivity "github.com/tjbdwanghaibo/roost-core/service/activity"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// windowCoordinator is the coordinator as this test needs it: a score per
// window. Everything else is embedded, so a method this test did not think
// about panics instead of quietly answering zero.
type windowCoordinator struct {
	svcactivity.Coordinator
	scores map[string]int64
}

func (c *windowCoordinator) ApplyProgress(_ context.Context, key svcactivity.Key, _ string, _ string, delta svcactivity.ProgressDelta) (svcactivity.Participant, error) {
	if c.scores == nil {
		c.scores = map[string]int64{}
	}
	c.scores[key.ActivityID] += delta.Score
	return svcactivity.Participant{Score: c.scores[key.ActivityID], Progress: c.scores[key.ActivityID]}, nil
}

func (c *windowCoordinator) LookupActivity(_ context.Context, key svcactivity.Key) (svcactivity.Activity, bool, error) {
	return svcactivity.Activity{Key: key}, true, nil
}

func (c *windowCoordinator) LookupParticipant(_ context.Context, key svcactivity.Key, _ string) (svcactivity.Participant, bool, error) {
	score := c.scores[key.ActivityID]
	return svcactivity.Participant{Score: score, Progress: score}, score > 0, nil
}

// boardRedis swallows the board write (Contribute writes it with one Eval:
// ZADD GT); this test is about which window the point went into, not about
// the index beside it.
type boardRedis struct{ fredis.IRedis }

func (boardRedis) Eval(context.Context, string, []string, ...any) (any, error) { return int64(1), nil }

// 一次贡献必须说出它落进了哪个窗口。
//
// 窗口每 gameactivity.WindowSeconds 滚一次，而 Contribute 与 Standing 各自
// 用调用那一刻的时钟算窗口 id。所以"12:14:59 贡献、12:15:01 查询"问的是两个
// 窗口——在此之前调用方无法把这种情况与"这一点没算上"区分开，CI 里的机器人
// 就是这样在整 300 秒边界上红的。
func TestAContributionSaysWhichWindowTookIt(t *testing.T) {
	ctx := context.Background()
	coordinator := &windowCoordinator{}
	// 停在窗口末尾前一秒。
	clock := time.Unix(gameactivity.WindowStart(time.Now().Unix())+gameactivity.WindowSeconds-1, 0)
	runner := &ActivityRunner{
		coordinator: coordinator,
		redis:       boardRedis{},
		prefix:      "test",
		groupID:     "test-group",
		now:         func() time.Time { return clock },
	}

	contributed, participant, err := runner.Contribute(ctx, 900001, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if contributed != gameactivity.ID(clock.Unix()) {
		t.Fatalf("contribution reported window %q, want %q", contributed, gameactivity.ID(clock.Unix()))
	}
	if participant.Score != 1 {
		t.Fatalf("the contribution counted %d, want 1", participant.Score)
	}

	// 同一个窗口内查询：看得见自己那一点。
	sameID, _, sameParticipant, err := runner.Standing(ctx, 900001)
	if err != nil {
		t.Fatal(err)
	}
	if sameID != contributed || sameParticipant.Score != 1 {
		t.Fatalf("within the window the standing is %q score=%d, want %q score=1", sameID, sameParticipant.Score, contributed)
	}

	// 跨过边界再查：是另一个窗口，分数为 0——而调用方**能看出来**这是换了窗口，
	// 因为它手里有贡献时的那个 id。
	clock = clock.Add(2 * time.Second)
	rolledID, _, rolledParticipant, err := runner.Standing(ctx, 900001)
	if err != nil {
		t.Fatal(err)
	}
	if rolledID == contributed {
		t.Fatalf("the window did not roll across the boundary: still %q", rolledID)
	}
	if rolledParticipant.Score != 0 {
		t.Fatalf("a fresh window inherited the point: %q score=%d", rolledID, rolledParticipant.Score)
	}
	// 而那一点仍然在它落进的窗口里，没有丢。
	if coordinator.scores[contributed] != 1 {
		t.Fatalf("the point left the window it landed in: %v", coordinator.scores)
	}
}

// lateOpenCoordinator 只在窗口开过之后接受进度：没开的窗口 ApplyProgress 答 ErrMissing（真实协调方的
// “activity: not found”），重复开窗答 ErrExists。
type lateOpenCoordinator struct {
	svcactivity.Coordinator
	open   map[string]bool
	opened []string
	scores map[string]int64
}

func (c *lateOpenCoordinator) OpenActivity(_ context.Context, key svcactivity.Key, _ []int32) (svcactivity.Activity, error) {
	if c.open[key.ActivityID] {
		return svcactivity.Activity{}, svcactivity.ErrExists
	}
	c.open[key.ActivityID] = true
	c.opened = append(c.opened, key.ActivityID)
	return svcactivity.Activity{Key: key}, nil
}

func (c *lateOpenCoordinator) ApplyProgress(_ context.Context, key svcactivity.Key, _ string, _ string, delta svcactivity.ProgressDelta) (svcactivity.Participant, error) {
	if !c.open[key.ActivityID] {
		return svcactivity.Participant{}, svcactivity.ErrMissing
	}
	c.scores[key.ActivityID] += delta.Score
	return svcactivity.Participant{Score: c.scores[key.ActivityID], Progress: c.scores[key.ActivityID]}, nil
}

// 窗口刚滚动、开窗循环还没跑到时通关，这一点也要算进当前窗口。
//
// 窗口由 runActivity 每 activityLoopInterval（5s）开一次，于是每个 300 秒边界之后最长 5s 里，时钟所在的
// 窗口在协调方那里还不存在：这段时间的通关 ApplyProgress 得到 “activity: not found”，贡献丢掉，
// finish_dungeon 回不出窗口 id，机器人判失败、loadtest 退出码 1（Redis Cluster 演练里 10 个机器人有 5 个
// 落在 23:05:00～23:05:04 这段）。Contribute 遇到“窗口不存在”时自己把它开起来（与开窗循环同一个幂等
// 调用）再记一次；Activity 开关关着时不开窗，与循环一致。
func TestAClearInTheFirstSecondsOfAWindowStillCounts(t *testing.T) {
	ctx := context.Background()
	newRunner := func(coordinator *lateOpenCoordinator, clock time.Time) *ActivityRunner {
		return &ActivityRunner{
			coordinator: coordinator, redis: boardRedis{}, prefix: "test", groupID: "test-group",
			liveness: &liveSource{live: []int32{1300}}, serverType: "game", gameSID: 1300, candidates: []int32{1300},
			now: func() time.Time { return clock },
		}
	}
	// 上一个窗口开着（循环在边界前跑过），时钟已经进了下一个窗口 1 秒。
	previous := gameactivity.WindowStart(time.Now().Unix())
	clock := time.Unix(previous+gameactivity.WindowSeconds+1, 0)
	current := gameactivity.ID(clock.Unix())

	t.Run("activity-on", func(t *testing.T) {
		featureflag.Set(featureflag.Flag{Name: flags.Activity, Enabled: true})
		t.Cleanup(func() { featureflag.Set(featureflag.Flag{Name: flags.Activity, Enabled: false}) })
		coordinator := &lateOpenCoordinator{open: map[string]bool{gameactivity.ID(previous): true}, scores: map[string]int64{}}
		contributed, participant, err := newRunner(coordinator, clock).Contribute(ctx, 900001, "run-1")
		if err != nil {
			t.Fatalf("a clear 1s into window %s was refused: %v", current, err)
		}
		if contributed != current || participant.Score != 1 || coordinator.scores[current] != 1 {
			t.Fatalf("the clear went to %q score=%d (scores %v), want window %s score 1", contributed, participant.Score, coordinator.scores, current)
		}
		if !slices.Equal(coordinator.opened, []string{current}) {
			t.Fatalf("windows opened %v, want exactly the clock's window %s", coordinator.opened, current)
		}
	})
	t.Run("activity-off-opens-nothing", func(t *testing.T) {
		featureflag.Set(featureflag.Flag{Name: flags.Activity, Enabled: false})
		coordinator := &lateOpenCoordinator{open: map[string]bool{}, scores: map[string]int64{}}
		if _, _, err := newRunner(coordinator, clock).Contribute(ctx, 900001, "run-1"); svcactivity.Code(err) != svcactivity.CodeMissing {
			t.Fatalf("with the activity switched off a clear in an unopened window answered %v, want not found", err)
		}
		if len(coordinator.opened) != 0 {
			t.Fatalf("the switched-off activity opened %v", coordinator.opened)
		}
	})
}

// liveSource 是 App 单实例锁的 Live 查询的替身：回答固定的活 sid 集合或一个错误，并记下被问了什么。
type liveSource struct {
	live       []int32
	err        error
	serverType string
	asked      []int32
}

func (source *liveSource) Live(_ context.Context, serverType string, sids []int32) ([]int32, error) {
	source.serverType, source.asked = serverType, append([]int32(nil), sids...)
	if source.err != nil {
		return nil, source.err
	}
	return append([]int32(nil), source.live...), nil
}

// openRecorder 记下 OpenActivity 是否被调用；其余方法嵌入接口，没想到的调用直接 panic。
type openRecorder struct {
	svcactivity.Coordinator
	opened int
}

func (c *openRecorder) OpenActivity(context.Context, svcactivity.Key, []int32) (svcactivity.Activity, error) {
	c.opened++
	return svcactivity.Activity{}, nil
}

// activity 不持有自己的全局租约，协调器的 expected 集合来自 App 锁的 Live 查询。
func TestTheExpectedServersAreTheOnesTheAppLockSeesAlive(t *testing.T) {
	ctx := context.Background()
	newRunner := func(source *liveSource) *ActivityRunner {
		return &ActivityRunner{liveness: source, serverType: "game", gameSID: 1300, candidates: []int32{1300, 1301, 1302}}
	}

	t.Run("the-live-sids", func(t *testing.T) {
		source := &liveSource{live: []int32{1300, 1302}}
		expected, err := newRunner(source).expectedGameSIDs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(expected, []int32{1300, 1302}) {
			t.Fatalf("expected %v, want exactly the live sids [1300 1302]: a candidate the lock does not see must not be waited on", expected)
		}
		if source.serverType != "game" || !slices.Equal(source.asked, []int32{1300, 1301, 1302}) {
			t.Fatalf("Live was asked about %q %v, want this process's server type and every candidate", source.serverType, source.asked)
		}
	})
	t.Run("nobody-live-means-only-this-server", func(t *testing.T) {
		expected, err := newRunner(&liveSource{}).expectedGameSIDs(ctx)
		if err != nil || !slices.Equal(expected, []int32{1300}) {
			t.Fatalf("expected %v (err %v), want [1300]: this server is running whatever the lock store says", expected, err)
		}
	})
	t.Run("an-unanswered-query-opens-no-window", func(t *testing.T) {
		featureflag.Set(featureflag.Flag{Name: flags.Activity, Enabled: true})
		t.Cleanup(func() { featureflag.Set(featureflag.Flag{Name: flags.Activity, Enabled: false}) })
		coordinator := &openRecorder{}
		runner := newRunner(&liveSource{err: errors.New("redis: i/o timeout")})
		runner.coordinator = coordinator
		if _, err := runner.expectedGameSIDs(ctx); err == nil {
			t.Fatal("a Live query that failed produced an expected set")
		}
		runner.openCurrentWindow(ctx)
		if coordinator.opened != 0 {
			t.Fatalf("a window was opened %d times without knowing which servers are alive", coordinator.opened)
		}
	})
}

// singleton.enabled=false 时 App 不登记 app.ModSingleton：activity 启动失败并说明原因，而不是退化成
// 只等自己。
func TestActivityRefusesToStartWithoutTheAppLockLiveness(t *testing.T) {
	_, _, err := startActivity(context.Background(), app.NewRegistry(viper.New()))
	if err == nil {
		t.Fatal("the activity started with no way to tell which game servers are alive")
	}
	if !strings.Contains(err.Error(), string(app.ModSingleton)) || !strings.Contains(err.Error(), "singleton.enabled") {
		t.Fatalf("startActivity error %q does not name the missing capability and the setting that publishes it", err)
	}
}

// writeGroupsFile writes an activity groups file with one group per entry of
// groups, in order, and returns its path.
func writeGroupsFile(t *testing.T, groups map[string][]int64, order ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("groups:\n")
	for _, id := range order {
		fmt.Fprintf(&b, "  - id: %s\n    game_sids: [", id)
		for i, sid := range groups[id] {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprint(&b, sid)
		}
		b.WriteString("]\n")
	}
	path := filepath.Join(t.TempDir(), "activity_groups.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func sidsFrom(first int64, n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = first + int64(i)
	}
	return out
}

// 维护者决定：活动组由 configs/activity_groups.yaml 定义，game 按自己的 sid 找组，组成员就是
// Live 查询的候选。组文件注定让窗口开不出来、或本服不在任何组里，都在启动时、任何远端调用之前，
// 按 activity.groups_file 点名拒绝，而不是每 5 秒记一条 Warn、活动永远不开：
//   - 一组超过 svcactivity.MaxExpectedGames（协调器单个窗口的上限）：同时活着超过上限时每个窗口都被拒；
//   - 组内重复的 sid、一个 sid 属于两个组、超出 int32 的 sid；
//   - 本服的 sid 不在文件的任何组里；没有配置 activity.groups_file。
func TestActivityRefusesAGroupNoWindowCouldOpenWith(t *testing.T) {
	start := func(t *testing.T, groupsFile string) error {
		t.Helper()
		config := viper.New()
		config.Set("sid", 1300)
		config.Set("server_type", "game")
		config.Set("activity.key_prefix", "roost:test:activity")
		if groupsFile != "" {
			config.Set("activity.groups_file", groupsFile)
		}
		registry := app.NewRegistry(config)
		if err := registry.Register(app.ModSingleton, app.SingletonLiveness(&liveSource{})); err != nil {
			t.Fatal(err)
		}
		_, stop, err := startActivity(context.Background(), registry)
		if stop != nil {
			stop()
		}
		return err
	}
	type refusal struct {
		name string
		file string
	}
	refusals := []refusal{
		{"more-than-the-coordinator-takes", writeGroupsFile(t, map[string][]int64{"g": append([]int64{1300}, sidsFrom(2000, svcactivity.MaxExpectedGames)...)}, "g")},
		{"a-repeated-sid", writeGroupsFile(t, map[string][]int64{"g": {1300, 1302, 1301, 1302}}, "g")},
		{"a-sid-in-two-groups", writeGroupsFile(t, map[string][]int64{"g": {1300, 1301}, "h": {1301}}, "g", "h")},
		{"a-sid-beyond-int32", writeGroupsFile(t, map[string][]int64{"g": {1300, int64(math.MaxUint32) + 1 + 1302}}, "g")},
		{"this-server-in-no-group", writeGroupsFile(t, map[string][]int64{"g": {1301, 1302}}, "g")},
		{"no-groups-file", ""},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			err := start(t, tc.file)
			if err == nil || !strings.Contains(err.Error(), "activity.groups_file") && !strings.Contains(err.Error(), "in no group") {
				t.Fatalf("startActivity with %s: error %v does not refuse the group by name; every window it would try to open would fail", tc.name, err)
			}
		})
	}
	// 恰好 MaxExpectedGames 个成员的组通过组检查（随后因为这个夹具没有其他能力而失败，失败原因不是它）。
	t.Run("a-full-group-is-accepted", func(t *testing.T) {
		full := writeGroupsFile(t, map[string][]int64{"g": append([]int64{1300}, sidsFrom(2000, svcactivity.MaxExpectedGames-1)...)}, "g")
		if err := start(t, full); err != nil && (strings.Contains(err.Error(), "activity.groups_file") || strings.Contains(err.Error(), "in no group")) {
			t.Fatalf("a group of %d game servers was refused: %v", svcactivity.MaxExpectedGames, err)
		}
	})
}

// 正常的组经真实协调器（内存存储）开窗——Key 的组 id 是组文件里本服所在的组，expected 是 Live 在组成员里
// 找到的活着的服；组的全部 MaxExpectedGames 个成员都活着时，协调器同样接受。
// 协调器读的是 game 读的同一个组文件（与部署相同），New 没有组就拒绝构造。
func TestAWindowOpensForTheGroupTheFilePutsThisServerIn(t *testing.T) {
	ctx := context.Background()
	newCoordinator := func(t *testing.T, path string) *svcactivity.Service {
		t.Helper()
		groups, err := svcactivity.LoadGroupsFile(path)
		if err != nil {
			t.Fatal(err)
		}
		coordinator, err := svcactivity.New(svcactivity.Config{
			Groups:              &groups,
			Activities:          versionstore.NewMemoryStore[svcactivity.Key, svcactivity.Activity](),
			Participants:        versionstore.NewMemoryStore[svcactivity.ParticipantKey, svcactivity.Participant](),
			Ledger:              versionstore.NewMemoryStore[svcactivity.RequestKey, svcactivity.ProgressReservation](),
			Audits:              versionstore.NewMemoryStore[svcactivity.Key, svcactivity.NotifyAuditLog](),
			Dispatches:          versionstore.NewMemoryStore[svcactivity.DispatchKey, svcactivity.Dispatch](),
			Windows:             versionstore.NewMemoryStore[string, svcactivity.Window](),
			GraceWindow:         30 * time.Second,
			ReservationTTL:      10 * time.Minute,
			DispatchBackoff:     5 * time.Second,
			DispatchMaxAttempts: 3,
		})
		if err != nil {
			t.Fatal(err)
		}
		return coordinator
	}
	runnerFor := func(t *testing.T, path string, live []int32, coordinator svcactivity.Coordinator) *ActivityRunner {
		t.Helper()
		group, err := activityGroup(path, 1300)
		if err != nil {
			t.Fatal(err)
		}
		return &ActivityRunner{coordinator: coordinator, liveness: &liveSource{live: live}, serverType: "game",
			gameSID: 1300, groupID: group.ID, candidates: group.GameSIDs}
	}

	t.Run("the-live-members-of-this-servers-group", func(t *testing.T) {
		path := writeGroupsFile(t, map[string][]int64{"east": {1200, 1201}, "west": {1300, 1301, 1302}}, "east", "west")
		coordinator := newCoordinator(t, path)
		runner := runnerFor(t, path, []int32{1300, 1302}, coordinator)
		if !slices.Equal(runner.candidates, []int32{1300, 1301, 1302}) {
			t.Fatalf("candidates %v, want the members of group west", runner.candidates)
		}
		expected, err := runner.openWindow(ctx, "race-1")
		if err != nil {
			t.Fatal(err)
		}
		opened, found, err := coordinator.LookupActivity(ctx, gameactivity.Key("west", "race-1"))
		if err != nil || !found {
			t.Fatalf("no window in group west (found %v, err %v)", found, err)
		}
		if !slices.Equal(opened.ExpectedGameSIDs, []int32{1300, 1302}) || !slices.Equal(expected, opened.ExpectedGameSIDs) {
			t.Fatalf("the window expects %v (runner said %v), want the live members [1300 1302]", opened.ExpectedGameSIDs, expected)
		}
		if _, found, _ := coordinator.LookupActivity(ctx, gameactivity.Key("east", "race-1")); found {
			t.Fatal("a window opened in a group this server is not in")
		}
		// Opening again is the normal answer on every later pass.
		if _, err := runner.openWindow(ctx, "race-1"); err != nil {
			t.Fatalf("a second pass over an open window failed: %v", err)
		}
	})
	t.Run("a-full-group-all-live", func(t *testing.T) {
		sids := append([]int64{1300}, sidsFrom(2000, svcactivity.MaxExpectedGames-1)...)
		path := writeGroupsFile(t, map[string][]int64{"full": sids}, "full")
		live := make([]int32, len(sids))
		for i, sid := range sids {
			live[i] = int32(sid)
		}
		coordinator := newCoordinator(t, path)
		runner := runnerFor(t, path, live, coordinator)
		if _, err := runner.openWindow(ctx, "race-2"); err != nil {
			t.Fatalf("the coordinator refused a window expecting a whole %d-server group: %v", svcactivity.MaxExpectedGames, err)
		}
		opened, found, err := coordinator.LookupActivity(ctx, gameactivity.Key("full", "race-2"))
		if err != nil || !found || len(opened.ExpectedGameSIDs) != svcactivity.MaxExpectedGames {
			t.Fatalf("window = %d expected (found %v, err %v), want %d", len(opened.ExpectedGameSIDs), found, err, svcactivity.MaxExpectedGames)
		}
	})
}

// keyRecorder 记下 OpenActivity 开的是哪个窗口；其余方法嵌入接口，没想到的调用直接 panic。
type keyRecorder struct {
	svcactivity.Coordinator
	opened []svcactivity.Key
}

func (c *keyRecorder) OpenActivity(_ context.Context, key svcactivity.Key, _ []int32) (svcactivity.Activity, error) {
	c.opened = append(c.opened, key)
	return svcactivity.Activity{}, nil
}

// recordingNest 记下 runner 发给 Nest 的请求并回答一个错误（runner 只记日志），用来看请求里带的是哪个时间。
type recordingNest struct {
	corenest.Client
	requests []corenest.Params
}

func (n *recordingNest) Request(_ context.Context, _ corenest.HandlerName, _ int64, params corenest.Params, _ ...corenest.SendOpt) (any, error) {
	n.requests = append(n.requests, append(corenest.Params(nil), params...))
	return nil, errors.New("recording nest: not served")
}

// 活动窗口、关窗截止与 World 定时器读业务时钟（time.logic_offset）。
// 业务时钟比真实时间快一天时，开的是业务时间所在的窗口，钉到 World 上的“现在”也是业务时间；
// 修前 openCurrentWindow 读 time.Now()，开的是真实时间所在的窗口，与协调器的业务时钟错开一天。
func TestTheWindowFollowsTheBusinessClock(t *testing.T) {
	featureflag.Set(featureflag.Flag{Name: flags.Activity, Enabled: true})
	t.Cleanup(func() { featureflag.Set(featureflag.Flag{Name: flags.Activity, Enabled: false}) })
	business := time.Now().Add(24 * time.Hour)
	coordinator := &keyRecorder{}
	nest := &recordingNest{}
	runner := &ActivityRunner{
		coordinator: coordinator, liveness: &liveSource{live: []int32{1300}}, serverType: "game",
		gameSID: 1300, groupID: "west", candidates: []int32{1300}, nest: nest,
		now: func() time.Time { return business },
	}
	runner.openCurrentWindow(context.Background())
	want := gameactivity.Key("west", gameactivity.ID(business.Unix()))
	if len(coordinator.opened) != 1 || coordinator.opened[0] != want {
		t.Fatalf("opened %v, want the window the business clock is in (%v), not the wall clock's (%v)",
			coordinator.opened, want, gameactivity.Key("west", gameactivity.ID(time.Now().Unix())))
	}
	closeAt, _ := gameactivity.CloseAt(gameactivity.ID(business.Unix()))
	if len(nest.requests) != 1 || fmt.Sprint(nest.requests[0]) != fmt.Sprint(corenest.Params{gameactivity.ID(business.Unix()), closeAt.Unix(), business.Unix()}) {
		t.Fatalf("the World was armed with %v, want the business window, its close time and the business now", nest.requests)
	}
}

// dispatchingNest 记下定时器驱动投递的 Tick（驱动在自己的 goroutine 里投递，所以经通道交给测试）。
type dispatchingNest struct {
	corenest.Client
	ticks chan dispatchedTick
}

type dispatchedTick struct {
	handler string
	id      int64
	params  corenest.Params
}

func (n *dispatchingNest) Dispatch(_ context.Context, name corenest.HandlerName, id int64, params corenest.Params, _ ...corenest.SendOpt) error {
	n.ticks <- dispatchedTick{handler: name.String(), id: id, params: append(corenest.Params(nil), params...)}
	return nil
}

// tickCommitter 接住 RunIsolatedTransaction 的提交记录；测试只关心提交之后驱动收到的最早期限。
type tickCommitter struct{}

func (tickCommitter) Commit(context.Context, dataengine.CommitRecord) error { return nil }

// World 定时器的驱动是框架的 entitytimer.Driver，由 startWorldTimers 手工接上 Nest 与业务时钟（demo 不用
// roost add timer）。业务时钟比真实时间快一天时：到期的关窗节点经 Nest 投给 TimerTickHandler（就是
// handlerTickWorldTimers 的名字），带的“现在”是业务时间，不是墙钟——World 的期限按业务时间登记，墙钟会让
// 节点晚一天才到期。D11 迁到生成器时删掉的 TestTheWorldTickCarriesBusinessTime 守的是同一件事（那时是每秒循环）。
func TestTheWorldTimersRunOnTheBusinessClock(t *testing.T) {
	config := viper.New()
	config.Set("time.logic_offset", "24h")
	registry := app.NewRegistry(config)
	nest := &dispatchingNest{ticks: make(chan dispatchedTick, 4)}
	if err := registry.Register(mods.ModNest, corenest.Client(nest)); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(mods.ModEntityRuntime, coreentity.NewManagerAccess(coreentity.NewEntityManager())); err != nil {
		t.Fatal(err)
	}
	stop, err := startWorldTimers(registry)
	if err != nil {
		t.Fatalf("startWorldTimers: %v", err)
	}
	t.Cleanup(func() {
		_ = stop(context.Background())
		world.TimerDriver.SetClock(clock.Now)
	})
	business := app.BusinessClock(registry).Now()
	if lag := business.Sub(time.Now()); lag < 23*time.Hour {
		t.Fatalf("the registry's business clock is %v ahead of the wall clock, want the configured 24h", lag)
	}
	if drift := world.TimerDriver.Now().Sub(business); drift < -time.Minute || drift > time.Minute {
		t.Fatalf("the World's timer driver reads %v, %v away from the business clock %v", world.TimerDriver.Now(), drift, business)
	}

	world.RegisterEntity()
	id, err := coreentity.BuildEntityID(9101, world.EntityKindWorld)
	if err != nil {
		t.Fatal(err)
	}
	built, err := coreentity.BuildEntity(&coreentity.EntityCreateParam{IsCreate: true, Kind: world.EntityKindWorld, Id: id})
	if err != nil {
		t.Fatalf("build world: %v", err)
	}
	subject := built.(*world.World)
	t.Cleanup(func() { subject.TimerComp().OnDestroy(coreentity.DestroyReasonMemoryUnload) })
	// 已经过了的关窗期限：登记为下一拍到期，提交后驱动立刻看到它。
	if _, err := corenest.RunIsolatedTransaction(context.Background(), tickCommitter{}, "arm", func() (any, error) {
		if _, armed := subject.TimerComp().ScheduleActivityPhase("race-business", business.Add(-time.Minute), business); !armed {
			t.Error("the overdue window close was not armed")
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("arm: %v", err)
	}
	select {
	case tick := <-nest.ticks:
		if tick.handler != world.TimerTickHandler || tick.id != subject.ID() || len(tick.params) != 1 {
			t.Fatalf("the driver delivered %+v, want one %s tick for World %d", tick, world.TimerTickHandler, subject.ID())
		}
		at, ok := tick.params[0].(int64)
		if !ok {
			t.Fatalf("the tick carries %T, want the business time in UnixMilli", tick.params[0])
		}
		if delivered := time.UnixMilli(at); delivered.Before(business) || delivered.Sub(business) > time.Minute {
			t.Fatalf("the tick carries %v, want the business time (%v), not the wall clock (%v)", delivered, business, time.Now())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the driver never delivered the due World tick")
	}
}

// 服务已提交旧请求后延后它的回复，模拟两条正常 RPC 回复乱序；不改协调器的计分或 Redis 行为。
type delayedProgressReply struct {
	svcactivity.Coordinator
	held    chan struct{}
	release chan struct{}
}

func (d *delayedProgressReply) ApplyProgress(ctx context.Context, key svcactivity.Key, participant, request string, delta svcactivity.ProgressDelta) (svcactivity.Participant, error) {
	result, err := d.Coordinator.ApplyProgress(ctx, key, participant, request, delta)
	if err == nil && request == gameactivity.ProgressRequestID("old") {
		close(d.held)
		select {
		case <-d.release:
		case <-ctx.Done():
			return svcactivity.Participant{}, ctx.Err()
		}
	}
	return result, err
}

func TestActivityBoardKeepsTheLatestCumulativeScore(t *testing.T) {
	addr := os.Getenv("ROOST_REVIEW_REDIS")
	if addr == "" {
		t.Skip("isolated Redis required")
	}
	prefix := redistest.Prefix(t, addr, "review:activity-board")
	client, err := driver.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	groups, err := svcactivity.ParseGroups([]byte("groups:\n  - id: review\n    game_sids: [1000]\n"), "review")
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := svcactivity.New(svcactivity.Config{
		Groups:         &groups,
		Activities:     versionstore.NewMemoryStore[svcactivity.Key, svcactivity.Activity](),
		Participants:   versionstore.NewMemoryStore[svcactivity.ParticipantKey, svcactivity.Participant](),
		Ledger:         versionstore.NewMemoryStore[svcactivity.RequestKey, svcactivity.ProgressReservation](),
		Audits:         versionstore.NewMemoryStore[svcactivity.Key, svcactivity.NotifyAuditLog](),
		Dispatches:     versionstore.NewMemoryStore[svcactivity.DispatchKey, svcactivity.Dispatch](),
		Windows:        versionstore.NewMemoryStore[string, svcactivity.Window](),
		ReservationTTL: 10 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	now := time.Now()
	activityID := gameactivity.ID(now.Unix())
	key := gameactivity.Key("review", activityID)
	if _, err := coordinator.OpenActivity(ctx, key, []int32{1000}); err != nil {
		t.Fatal(err)
	}
	delayed := &delayedProgressReply{Coordinator: coordinator, held: make(chan struct{}), release: make(chan struct{})}
	runner := &ActivityRunner{coordinator: delayed, redis: client, prefix: prefix, groupID: "review", now: func() time.Time { return now }}
	oldDone := make(chan error, 1)
	go func() { _, _, err := runner.Contribute(ctx, 4242, "old"); oldDone <- err }()
	select {
	case <-delayed.held:
	case <-ctx.Done():
		t.Fatal("old request did not commit before reply delay")
	}
	_, latest, err := runner.Contribute(ctx, 4242, "new")
	if err != nil || latest.Score != 2 {
		t.Fatalf("second contribution: %+v err=%v", latest, err)
	}
	boardKey := gameactivity.BoardKey(prefix, "review", activityID)
	if score, err := client.ZScore(ctx, boardKey, "4242"); err != nil || score != 2 {
		t.Fatalf("newer score before delayed reply = %v err=%v", score, err)
	}
	close(delayed.release)
	if err := <-oldDone; err != nil {
		t.Fatal(err)
	}
	authority, found, err := coordinator.LookupParticipant(ctx, key, "4242")
	if err != nil || !found || authority.Score != 2 {
		t.Fatalf("authority=%+v found=%v err=%v", authority, found, err)
	}
	score, err := client.ZScore(ctx, boardKey, "4242")
	if err != nil {
		t.Fatal(err)
	}
	if score != 2 {
		t.Fatalf("coordinator score is 2 but settlement board fell from 2 to %v after the delayed score-1 reply", score)
	}
}
