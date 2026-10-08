package remoteflow

// Mirror 第 6 步的本机替代（roost-core docs/feature/MIRROR-STEP-6-LOCAL-2026-10-06.md）：私有依赖进程上的
// 两进程故障场景。只由 scripts/mirror-local.sh test 驱动（ROOST_MIRROR_LOCAL=1，依赖地址来自它自起的私有环境），
// 默认不运行。
//
// 进程：
//   - 编排 = 本测试进程（TestGeneratedRemoteMirrorLocal），只发命令、注入故障、判定；
//   - owner = 子进程（ROOST_MIRROR_LOCAL_ROLE=owner）：Managed Guild 经正式链路（Nest → WAL → 投影 → Remote 提交 →
//     发布），WAL 目录由编排给出，强杀后用同一目录重启即 WAL 重放补发；
//   - 只读服务 = 子进程（ROOST_MIRROR_LOCAL_ROLE=reader）：kit SyncBusMod + RemoteMirrorMod + 生成的 reader，
//     后台每 5ms 做一次 Cached 读，自己核对不变量（只在本进程里比较，避免跨进程时钟以外的假设）：
//       1. 不回退：读到的版本不低于此前读到的最大版本；
//       2. 不复活：确认删除、并且本进程已读到“不存在”之后，再读到存在即违例；
//       3. 有界收敛：owner 确认版本 v（或删除）的时刻 T 之后，超过 T + cached_max_staleness + 1.5s 的读仍旧
//          低于 v（或仍存在）即违例。陈旧上限内读到旧值是契约允许的。
//     违例打印 "MIRROR VIOLATION ..."，编排在每个场景结束时要求 0 条。
//
// 收敛时间 = owner 报告确认（Request 返回成功）的时刻到只读方第一次读到它的时刻，同一台机器的墙钟。

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	kitremote "github.com/tjbdwanghaibo/roost-core/kit/remoteentity"
	kitsyncbus "github.com/tjbdwanghaibo/roost-core/kit/syncbus"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/mongo/driver"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	natsdriver "github.com/tjbdwanghaibo/roost-core/nats/driver"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/remoteentity"
	syncdriver "github.com/tjbdwanghaibo/roost-core/sync/syncbus/driver"
)

// 只读方的 cached_max_staleness：故障场景用 3s（检验“推送中断期间按陈旧上限回源”）；toxiproxy 场景用 60s，
// 这样在分区 / 丢包恢复后读到新版本只能来自推送续投（DeliverNew durable），而不是陈旧上限回源。
const (
	localShortStaleness = 3 * time.Second
	localLongStaleness  = time.Minute
	localBoundGrace     = 1500 * time.Millisecond
)

// ---------------- 编排 ----------------

type localEnv struct {
	script     string
	toxiproxy  string
	nats       []string // 直连三个节点
	natsProxy  []string // 经 toxiproxy 的三个节点
	redis      string   // 单机：经 toxiproxy 的固定入口（切主时改指新主）
	cluster    string
	mongoURI   string
	database   string
	workDir    string
	results    *localResults
	childCount atomic.Int64
}

// localResults 收集每个场景的度量，最后按行打印（"MIRROR6 <场景> <键>=<值>"），文档的场景矩阵由它填写。
type localResults struct {
	mu    sync.Mutex
	lines []string
}

func (r *localResults) add(t *testing.T, format string, args ...any) {
	line := fmt.Sprintf("MIRROR6 %s %s", t.Name(), fmt.Sprintf(format, args...))
	r.mu.Lock()
	r.lines = append(r.lines, line)
	r.mu.Unlock()
	t.Log(line)
}

func TestGeneratedRemoteMirrorLocal(t *testing.T) {
	if os.Getenv("ROOST_MIRROR_CHILD") == "1" {
		t.Skip("parent-only test")
	}
	if os.Getenv("ROOST_MIRROR_LOCAL") != "1" {
		t.Skip("run scripts/mirror-local.sh test (private dependency processes)")
	}
	env := &localEnv{
		script:    os.Getenv("ROOST_MIRROR_LOCAL_SCRIPT"),
		toxiproxy: os.Getenv("ROOST_DATAENGINE_IT_TOXIPROXY_URL"),
		nats:      strings.Split(os.Getenv("ROOST_DATAENGINE_IT_NATS_URL"), ","),
		natsProxy: strings.Split(os.Getenv("ROOST_DATAENGINE_IT_NATS_PROXIED_URL"), ","),
		redis:     os.Getenv("ROOST_DATAENGINE_IT_REDIS_PROXIED_ADDR"),
		cluster:   os.Getenv("ROOST_MIRROR_LOCAL_REDIS_CLUSTER"),
		mongoURI:  os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI"),
		database:  NewBalanceDao().DbName(),
		workDir:   t.TempDir(),
		results:   &localResults{},
	}
	if env.script == "" || env.toxiproxy == "" || len(env.nats) != 3 || len(env.natsProxy) != 3 || env.redis == "" || env.cluster == "" {
		t.Fatal("scripts/mirror-local.sh test exports the private environment; some addresses are missing")
	}
	if !strings.HasPrefix(env.database, "roost_remote_generated_") || env.database == "roost_remote_generated_placeholder" {
		t.Fatal("isolated database required")
	}
	t.Cleanup(func() {
		mongo, err := mongodriver.NewClient(fmongo.DefaultConfig(env.mongoURI), mongodriver.IndexMigrationPolicy{})
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = mongo.Database(env.database).Drop(ctx)
			cancel()
			mongo.Close(context.Background())
		}
		t.Logf("results:\n%s", strings.Join(env.results.lines, "\n"))
	})
	for _, scenario := range []struct {
		name string
		run  func(*testing.T, *localEnv)
	}{
		{"S1_owner_kill_restart_wal_replay", localOwnerKillRestart},
		{"S2_nats_node_kill_and_sigstop", localNatsNodeFaults},
		{"S3_toxiproxy_latency_partition_loss", localToxiproxyFaults},
		{"S4a_redis_standalone_failover", func(t *testing.T, env *localEnv) { localRedisFailover(t, env, false) }},
		{"S4b_redis_cluster_failover", func(t *testing.T, env *localEnv) { localRedisFailover(t, env, true) }},
		{"S5_mongo_stepdown", localMongoStepDown},
		{"S6_owner_transfer", localOwnerTransfer},
		{"S7_reader_kill_restart", localReaderKillRestart},
	} {
		if !localScenarioSelected(scenario.name, os.Getenv("ROOST_MIRROR_LOCAL_ONLY")) {
			continue
		}
		t.Run(scenario.name, func(t *testing.T) { scenario.run(t, env) })
	}
}

// localOwnerLockTTL 是 owner 子进程的共享锁 TTL：被强杀的 owner 持有的锁要这么久才过期；同 sid 重启的 owner
// 带进程代际，第一次取锁就接管，不等它（S1，O-M6-6）。
const localOwnerLockTTL = 3 * time.Second

// localScenarioSelected：ROOST_MIRROR_LOCAL_ONLY 为空时全跑；否则是逗号分隔的子串列表（例如 S1,S7），场景名含其一即跑。
func localScenarioSelected(name, only string) bool {
	if only == "" {
		return true
	}
	for _, part := range strings.Split(only, ",") {
		if part = strings.TrimSpace(part); part != "" && strings.Contains(name, part) {
			return true
		}
	}
	return false
}

// localScenario 是一个场景的隔离单元：自己的总线前缀（因而自己的 JetStream 流）、L2 前缀与公会。
type localScenario struct {
	t        *testing.T
	env      *localEnv
	ctx      context.Context
	prefix   string
	l2Prefix string
	guild    int64
	redis    string // "" = 单机（toxiproxy 入口），否则 Cluster 地址
	children []*localChild
	started  map[uint64]int64 // 版本 → owner 开始提交的时刻
}

func (env *localEnv) scenario(t *testing.T, raw int64, cluster bool) *localScenario {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	name := strings.ToLower(strings.NewReplacer("/", "_", "TestGeneratedRemoteMirrorLocal_", "").Replace(t.Name()))
	id, err := entity.BuildEntityID(raw, EntityKindGuild)
	if err != nil {
		t.Fatal(err)
	}
	s := &localScenario{t: t, env: env, ctx: ctx, prefix: env.database + ".m6." + name, l2Prefix: env.database + ":m6:" + name, guild: id}
	if cluster {
		s.redis = env.cluster
	}
	t.Cleanup(func() {
		for _, child := range s.children {
			if t.Failed() {
				t.Logf("child %s diagnostics:\n%s", child.name, child.output())
			}
			child.kill()
		}
	})
	return s
}

func (s *localScenario) commonEnv(sid int32) []string {
	redis := "ROOST_MIRROR_REDIS=" + s.env.redis
	if s.redis != "" {
		redis = "ROOST_MIRROR_REDIS_CLUSTER=" + s.redis
	}
	return []string{
		"ROOST_MIRROR_SID=" + strconv.Itoa(int(sid)), "ROOST_MIRROR_GUILD=" + strconv.FormatInt(s.guild, 10),
		"ROOST_MIRROR_PREFIX=" + s.prefix, "ROOST_MIRROR_L2_PREFIX=" + s.l2Prefix, "ROOST_MIRROR_DATABASE=" + s.env.database,
		"ROOST_MIRROR_MONGO=" + s.env.mongoURI, redis,
	}
}

// owner 子进程：create=true 时新建公会并建立所有权（共享模式）；否则从 Mongo 冷加载并认领。
func (s *localScenario) startOwner(sid int32, walDir string, create bool, natsURL string) *localChild {
	s.t.Helper()
	env := append(s.commonEnv(sid), "ROOST_MIRROR_LOCAL_ROLE=owner", "ROOST_MIRROR_WAL="+walDir,
		"ROOST_MIRROR_CREATE="+strconv.FormatBool(create), "ROOST_MIRROR_NATS="+natsURL)
	child := s.env.startChild(s.t, s.ctx, fmt.Sprintf("owner-%d", sid), env)
	s.children = append(s.children, child)
	child.expect("READY", 60*time.Second)
	return child
}

// reader 子进程：nats 是逗号分隔的地址；ignore=true 时不用发现的地址（经 toxiproxy 时必须这样）。
func (s *localScenario) startReader(sid int32, nats string, ignore bool, staleness time.Duration, extra ...string) *localChild {
	s.t.Helper()
	env := append(append(s.commonEnv(sid), "ROOST_MIRROR_LOCAL_ROLE=reader", "ROOST_MIRROR_NATS="+nats,
		"ROOST_MIRROR_NATS_IGNORE_DISCOVERED="+strconv.FormatBool(ignore), "ROOST_MIRROR_STALENESS="+staleness.String()), extra...)
	child := s.env.startChild(s.t, s.ctx, fmt.Sprintf("reader-%d", sid), env)
	s.children = append(s.children, child)
	if got := child.expect("READY", 60*time.Second); len(got) != 1 || got[0] != "push=true" {
		s.t.Fatalf("reader %d ready %v, want push=true on JetStream", sid, got)
	}
	return child
}

// commit 让 owner 提交一笔并返回（版本，确认时刻）。
func (s *localScenario) commit(owner *localChild, name string) (uint64, int64) {
	s.t.Helper()
	owner.send(fmt.Sprintf("COMMIT %s 10000", name))
	got := owner.expectAny(30*time.Second, "COMMITTED", "COMMITERR")
	if got[0] != "COMMITTED" {
		s.t.Fatalf("owner commit %s: %v", name, got)
	}
	return s.committed(got)
}

// committed 解析 "COMMITTED <版本> <确认时刻> <开始时刻> ..."，记下开始时刻（推送端到端从提交开始计时）。
func (s *localScenario) committed(got []string) (uint64, int64) {
	s.t.Helper()
	version, at := parseUint(s.t, got[1]), parseInt(s.t, got[2])
	if s.started == nil {
		s.started = map[uint64]int64{}
	}
	s.started[version] = parseInt(s.t, got[3])
	return version, at
}

// commitRetry 在故障窗口里重试提交直到成功（每次 2s 截止），返回版本、确认时刻与尝试次数、owner 不可写的时长。
func (s *localScenario) commitRetry(owner *localChild, name string, deadline time.Duration) (uint64, int64, string) {
	s.t.Helper()
	owner.send(fmt.Sprintf("RETRY %s %d", name, deadline.Milliseconds()))
	got := owner.expectAny(deadline+30*time.Second, "COMMITTED", "COMMITERR")
	if got[0] != "COMMITTED" {
		s.t.Fatalf("owner retry commit %s: %v", name, got)
	}
	version, at := s.committed(got)
	return version, at, strings.Join(got[4:], " ")
}

// waitVersion 让只读方等到版本 v（并以确认时刻登记有界收敛的断言），返回收敛时长（毫秒）：owner 经 Request 提交的
// 版本从开始提交计时（推送端到端：提交 → 只读方读到，含 Nest / WAL / Mongo 提交）；没有开始时刻的（重放补发）从
// confirmedAt 计时。只读方可能在 owner 的 Request 返回之前就读到（发布先于回复），所以不从确认时刻计时。
func (s *localScenario) waitVersion(reader *localChild, version uint64, confirmedAt int64, timeout time.Duration) int64 {
	s.t.Helper()
	reader.send(fmt.Sprintf("WAIT %d %d %d", version, confirmedAt, timeout.Milliseconds()))
	got := reader.expectAny(timeout+10*time.Second, "CONVERGED", "TIMEOUT")
	if got[0] != "CONVERGED" {
		s.t.Fatalf("reader never converged to version %d: %v\n%s", version, got, reader.output())
	}
	seenAt := parseInt(s.t, got[3])
	if started, ok := s.started[version]; ok {
		return time.Duration(seenAt - started).Milliseconds()
	}
	return time.Duration(seenAt - confirmedAt).Milliseconds()
}

// remove 让 owner 删除公会，返回（确认时刻，开始时刻）。先告诉在跑的只读方“要删除了”：推送可能先于 owner 的
// 确认到达，此时读到“不存在”不是违例。
func (s *localScenario) remove(owner *localChild, readers ...*localChild) (int64, int64) {
	s.t.Helper()
	for _, r := range readers {
		r.send("PREDEL")
		r.expect("PREDEL", 10*time.Second)
	}
	owner.send("DELETE 10000")
	got := owner.expectAny(30*time.Second, "DELETED", "DELETEERR")
	if got[0] != "DELETED" {
		s.t.Fatalf("delete: %v", got)
	}
	return parseInt(s.t, got[1]), parseInt(s.t, got[2])
}

// waitDeleted 等只读方读到“不存在”（并登记删除的有界收敛与不复活断言），返回从开始删除起的毫秒数。
func (s *localScenario) waitDeleted(reader *localChild, confirmedAt, startedAt int64, timeout time.Duration) int64 {
	s.t.Helper()
	reader.send(fmt.Sprintf("WAITDEL %d %d", confirmedAt, timeout.Milliseconds()))
	got := reader.expectAny(timeout+10*time.Second, "CONVERGED", "TIMEOUT")
	if got[0] != "CONVERGED" {
		s.t.Fatalf("reader never observed the delete: %v\n%s", got, reader.output())
	}
	return time.Duration(parseInt(s.t, got[3]) - startedAt).Milliseconds()
}

// stats 取只读方的计数；违例数必须为 0。
func (s *localScenario) stats(reader *localChild) map[string]string {
	s.t.Helper()
	reader.send("STATS")
	got := reader.expect("STATS", 10*time.Second)
	out := map[string]string{}
	for _, field := range got {
		if k, v, ok := strings.Cut(field, "="); ok {
			out[k] = v
		}
	}
	if out["violations"] != "0" {
		s.t.Errorf("reader reported %s violations:\n%s", out["violations"], reader.output())
	}
	return out
}

func (s *localScenario) fault(args ...string) string {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "bash", append([]string{s.env.script, "fault"}, args...)...).CombinedOutput()
	if err != nil {
		s.t.Fatalf("fault %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// toxiproxy 的 HTTP API（私有 toxiproxy，不是共享环境的）。
func (s *localScenario) tox(method, path, body string) {
	s.t.Helper()
	req, err := http.NewRequestWithContext(s.ctx, method, s.env.toxiproxy+path, strings.NewReader(body))
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		s.t.Fatalf("toxiproxy %s %s: %s %s", method, path, resp.Status, data)
	}
}

func (s *localScenario) toxAll(fn func(proxy string)) {
	for i := 1; i <= 3; i++ {
		fn(fmt.Sprintf("nats-%d", i))
	}
}

func (s *localScenario) stopChild(child *localChild) {
	s.t.Helper()
	child.send("STOP")
	child.expect("STOPPED", 30*time.Second)
	child.wait()
}

// ---------------- 场景 ----------------

// S1：owner 强杀后重启。第二笔写在投影前被拖住（WAL 已持久、Mongo 未提交、未发布），owner 被 SIGKILL；
// 用同一 WAL 目录重启后，投影器重放补发：Mongo 提交 → 发布 → 只读方读到。之后 owner 冷加载公会照常写。
func localOwnerKillRestart(t *testing.T, env *localEnv) {
	s := env.scenario(t, 96001, false)
	wal := filepath.Join(env.workDir, "s1-wal")
	owner := s.startOwner(1101, wal, true, env.nats[0])
	v1, at1 := s.commit(owner, "a")
	reader := s.startReader(3101, env.nats[1], false, localShortStaleness)
	env.results.add(t, "first_read_ms=%d", s.waitVersion(reader, v1, at1, 30*time.Second))

	owner.send("HOLD")
	owner.expect("HELD", 10*time.Second)
	owner.send("COMMIT b 500")
	if got := owner.expectAny(30*time.Second, "COMMITTED", "COMMITERR"); got[0] != "COMMITERR" {
		t.Fatalf("the held commit returned %v, want a deadline (WAL durable, projection held)", got)
	}
	owner.kill()
	killedAt := time.Now()
	time.Sleep(500 * time.Millisecond)
	reader.send("PEEK")
	if got := reader.expect("PEEK", 10*time.Second); got[0] != strconv.FormatUint(v1, 10) {
		t.Fatalf("while the owner is down the reader reads %v, want the confirmed v%d (the held write was never confirmed)", got, v1)
	}
	restarted := s.startOwner(1101, wal, false, env.nats[0])
	readyAt := time.Now()
	// 重放补发的那笔不经 Request 返回，没有确认时刻；从重启就绪起计时。
	replayed := s.waitVersion(reader, v1+1, readyAt.UnixNano(), 60*time.Second)
	env.results.add(t, "owner_restart_ready_ms=%d replayed_v%d_converge_ms=%d", readyAt.Sub(killedAt).Milliseconds(), v1+1, replayed)
	// 被强杀的旧进程还持有共享锁。O-M6-6 之前重启后的第一笔写要等它过期（锁的重试预算用完报 versioned lock not
	// acquired，Request 截止则是结果未知，朴素重试会多写一笔 v4），上一轮这里先等旧锁过期再写；现在同 sid 的新进程
	// （owner 子进程带进程代际，代替 App 单实例锁）第一次取锁就接管上一代的锁，立即写。
	v3, at3 := s.commit(restarted, "c")
	if v3 != v1+2 {
		t.Fatalf("commit after the restart got version %d, want %d (the replayed write is v%d)", v3, v1+2, v1+1)
	}
	// commit_ms 是这笔写本身的耗时；converge_ms 从开始写计时、含它在内；visible_after_confirm_ms 是确认之后只读方
	// 还要等多久（负数：推送先于回复到达）——O-M6-1 前后对照看它。
	commitMs := (at3 - s.started[v3]) / int64(time.Millisecond)
	if commitMs >= localOwnerLockTTL.Milliseconds()/2 {
		t.Fatalf("the first write after the restart took %dms (lock_ttl %v): it waited for the killed owner's lease instead of taking it over (O-M6-6)", commitMs, localOwnerLockTTL)
	}
	// 这笔写开始时距强杀多久：小于 lock_ttl（旧进程的异步续期还可能把租约延到 2×lock_ttl）才说明它撞上了旧租约、靠接管写成。
	env.results.add(t, "first_write_started_after_kill_ms=%d (lock_ttl %v)", (s.started[v3]-killedAt.UnixNano())/int64(time.Millisecond), localOwnerLockTTL)
	converge := s.waitVersion(reader, v3, at3, 30*time.Second)
	env.results.add(t, "after_restart_commit_v%d commit_ms=%d converge_ms=%d visible_after_confirm_ms=%d", v3, commitMs, converge, converge-commitMs)
	stats := s.stats(reader)
	env.results.add(t, "reader_stats loads=%s errors=%s reads=%s", stats["loads"], stats["errors"], stats["reads"])
	// 没有多写的 v4：重启后只写了一笔，只读方停在 v3。
	reader.send("PEEK")
	if got := reader.expect("PEEK", 10*time.Second); got[0] != strconv.FormatUint(v3, 10) {
		t.Fatalf("after the restart the reader reads %v, want v%d (no duplicate write)", got, v3)
	}
	s.stopChild(reader)
	s.stopChild(restarted)
}

// S2：NATS 节点故障。只读方连 nats-2（开着发现）；强杀 nats-2 → 自动连到别的节点，durable 续投；
// 之后另起一个只读方连 nats-3，SIGSTOP nats-3 制造静默断线（ping 20s 才能发现）：推送停住，按陈旧上限 3s 回源。
func localNatsNodeFaults(t *testing.T, env *localEnv) {
	s := env.scenario(t, 96002, false)
	owner := s.startOwner(1102, filepath.Join(env.workDir, "s2-wal"), true, env.nats[0])
	v, at := s.commit(owner, "a")
	reader := s.startReader(3102, env.nats[1], false, localShortStaleness)
	s.waitVersion(reader, v, at, 30*time.Second)
	v, at = s.commit(owner, "b")
	env.results.add(t, "baseline_push_ms=%d", s.waitVersion(reader, v, at, 30*time.Second))

	s.fault("nats-kill", "2")
	v, at, attempts := s.commitRetry(owner, "c", 30*time.Second)
	env.results.add(t, "nats2_killed owner_commit %s converge_ms=%d", attempts, s.waitVersion(reader, v, at, 30*time.Second))
	v, at = s.commit(owner, "d")
	env.results.add(t, "after_reconnect_push_ms=%d", s.waitVersion(reader, v, at, 30*time.Second))
	s.fault("nats-start", "2")

	silent := s.startReader(3112, env.nats[2], false, localShortStaleness)
	s.waitVersion(silent, v, at, 30*time.Second)
	v, at = s.commit(owner, "e")
	env.results.add(t, "silent_reader_baseline_push_ms=%d", s.waitVersion(silent, v, at, 30*time.Second))
	s.fault("nats-stop", "3")
	t.Cleanup(func() { s.fault("nats-cont", "3") })
	stoppedAt := time.Now()
	v, at, attempts = s.commitRetry(owner, "f", 30*time.Second)
	env.results.add(t, "nats3_sigstop owner_commit %s", attempts)
	env.results.add(t, "nats3_sigstop_converge_ms=%d (staleness %v)", s.waitVersion(silent, v, at, 30*time.Second), localShortStaleness)
	env.results.add(t, "nats3_sigstop_other_reader_converge_ms=%d", s.waitVersion(reader, v, at, 30*time.Second))
	time.Sleep(time.Until(stoppedAt.Add(8 * time.Second)))
	s.fault("nats-cont", "3")
	v, at = s.commit(owner, "g")
	env.results.add(t, "after_sigcont_converge_ms=%d", s.waitVersion(silent, v, at, 60*time.Second))
	for _, r := range []*localChild{reader, silent} {
		stats := s.stats(r)
		env.results.add(t, "reader_%s_stats loads=%s errors=%s maxErrStreakMs=%s", r.name, stats["loads"], stats["errors"], stats["maxErrStreakMs"])
		s.stopChild(r)
	}
	s.stopChild(owner)
}

// S3：toxiproxy。只读方只经三个代理连 NATS（不用发现的地址），陈旧上限 60s：读到新版本只能来自推送。
// 延迟 200ms；全部代理断开 5s（分区）期间提交，恢复后经 DeliverNew durable 续投；timeout 毒（丢数据，
// 连接被破坏后重连）期间提交，恢复后续投。
func localToxiproxyFaults(t *testing.T, env *localEnv) {
	s := env.scenario(t, 96003, false)
	s.tox("POST", "/reset", "")
	t.Cleanup(func() { s.tox("POST", "/reset", "") })
	owner := s.startOwner(1103, filepath.Join(env.workDir, "s3-wal"), true, env.nats[0])
	v, at := s.commit(owner, "a")
	reader := s.startReader(3103, strings.Join(env.natsProxy, ","), true, localLongStaleness)
	s.waitVersion(reader, v, at, 30*time.Second)
	v, at = s.commit(owner, "b")
	env.results.add(t, "baseline_push_ms=%d", s.waitVersion(reader, v, at, 30*time.Second))

	s.toxAll(func(p string) {
		s.tox("POST", "/proxies/"+p+"/toxics", `{"name":"lat","type":"latency","stream":"downstream","attributes":{"latency":200,"jitter":20}}`)
	})
	v, at = s.commit(owner, "c")
	env.results.add(t, "latency200_push_ms=%d", s.waitVersion(reader, v, at, 30*time.Second))
	s.toxAll(func(p string) { s.tox("DELETE", "/proxies/"+p+"/toxics/lat", "") })

	s.toxAll(func(p string) { s.tox("POST", "/proxies/"+p, `{"enabled":false}`) })
	v, at = s.commit(owner, "d")
	time.Sleep(5 * time.Second)
	reader.send("PEEK")
	if got := reader.expect("PEEK", 10*time.Second); parseUint(t, got[0]) >= v {
		t.Fatalf("during the partition the reader already reads %v: the read did not need the push", got)
	}
	s.toxAll(func(p string) { s.tox("POST", "/proxies/"+p, `{"enabled":true}`) })
	healedAt := time.Now()
	converged := s.waitVersion(reader, v, at, 50*time.Second)
	env.results.add(t, "partition5s_converge_ms=%d after_heal_ms=%d (staleness %v: only the durable redelivery can explain it)", converged, time.Since(healedAt).Milliseconds()-0, localLongStaleness)

	s.toxAll(func(p string) {
		s.tox("POST", "/proxies/"+p+"/toxics", `{"name":"drop","type":"timeout","stream":"downstream","attributes":{"timeout":0}}`)
	})
	v, at = s.commit(owner, "e")
	time.Sleep(3 * time.Second)
	s.toxAll(func(p string) { s.tox("DELETE", "/proxies/"+p+"/toxics/drop", "") })
	env.results.add(t, "drop3s_converge_ms=%d", s.waitVersion(reader, v, at, 55*time.Second))
	v, at = s.commit(owner, "f")
	env.results.add(t, "after_faults_push_ms=%d", s.waitVersion(reader, v, at, 30*time.Second))
	stats := s.stats(reader)
	env.results.add(t, "reader_stats loads=%s errors=%s maxErrStreakMs=%s", stats["loads"], stats["errors"], stats["maxErrStreakMs"])
	s.stopChild(reader)
	s.stopChild(owner)
}

// S4：Redis 切主。owner 与只读方共用 L2。复制追平后切主（单机：副本提升 + 代理改指；Cluster：SIGKILL 负责
// 该键的主节点），之后提交与删除；删除之后再切一次主，另起一个全新的只读方（L1 为空）读：墓碑不回退、不复活。
// 单机另做一次“未复制即切主”（副本暂停并断开复制连接，墓碑只写在旧主上）：这是 Redis 异步复制的已知丢写，
// 记录只读方的实际表现作为边界，不作为通过条件（见文档 §3.4）。
func localRedisFailover(t *testing.T, env *localEnv, cluster bool) {
	raw := int64(96041)
	if cluster {
		raw = 96042
	}
	s := env.scenario(t, raw, cluster)
	owner := s.startOwner(1104, filepath.Join(env.workDir, fmt.Sprintf("s4-%v-wal", cluster)), true, env.nats[0])
	v, at := s.commit(owner, "a")
	reader := s.startReader(3104, env.nats[1], false, localShortStaleness)
	s.waitVersion(reader, v, at, 30*time.Second)
	v, at = s.commit(owner, "b")
	s.waitVersion(reader, v, at, 30*time.Second)
	l2Key := s.l2Key()

	failover := func() string {
		time.Sleep(300 * time.Millisecond) // 让复制追平（单机 graceful 自己还会核对偏移量）
		if cluster {
			return s.fault("redis-cluster-kill-master", l2Key)
		}
		return s.fault("redis-standalone-failover", "graceful")
	}
	env.results.add(t, "failover1 %q", failover())
	v, at, attempts := s.commitRetry(owner, "c", 30*time.Second)
	env.results.add(t, "after_failover_commit %s converge_ms=%d", attempts, s.waitVersion(reader, v, at, 30*time.Second))

	deletedAt, deleteStarted := s.remove(owner, reader)
	env.results.add(t, "delete_converge_ms=%d", s.waitDeleted(reader, deletedAt, deleteStarted, 30*time.Second))
	env.results.add(t, "failover2 %q", failover())
	fresh := s.startReader(3114, env.nats[2], false, localShortStaleness, "ROOST_MIRROR_DELETED_AT="+strconv.FormatInt(deletedAt, 10))
	fresh.send("PEEK")
	if got := fresh.expect("PEEK", 10*time.Second); got[0] != "absent" {
		t.Fatalf("a fresh reader after the failover reads %v, want absent (the tombstone survived the replicated failover)", got)
	}
	s.waitDeleted(fresh, deletedAt, deleteStarted, 10*time.Second)
	time.Sleep(localShortStaleness + localBoundGrace) // 两个只读方都越过一次陈旧上限（重新确认 L2 / 权威）
	for _, r := range []*localChild{reader, fresh} {
		stats := s.stats(r)
		env.results.add(t, "reader_%s_stats loads=%s errors=%s maxErrStreakMs=%s", r.name, stats["loads"], stats["errors"], stats["maxErrStreakMs"])
		s.stopChild(r)
	}
	s.stopChild(owner)
	if !cluster {
		localRedisLossyFailover(t, env)
	}
}

// localRedisLossyFailover：未复制即切主的边界记录（单机）。删除的墓碑只写在旧主上，旧主被 SIGKILL、副本未追平即被提升，
// L2 回到删除前的版本。只读方：原进程（L1 里有墓碑）与新起的进程（L1 空）各读一段时间，记录是否读到“存在”。
func localRedisLossyFailover(t *testing.T, env *localEnv) {
	s := env.scenario(t, 96043, false)
	owner := s.startOwner(1105, filepath.Join(env.workDir, "s4-lossy-wal"), true, env.nats[0])
	v, at := s.commit(owner, "a")
	reader := s.startReader(3105, env.nats[1], false, localShortStaleness)
	s.waitVersion(reader, v, at, 30*time.Second)
	time.Sleep(500 * time.Millisecond)
	s.fault("redis-standalone-pause-replica")
	deletedAt, deleteStarted := s.remove(owner, reader)
	s.waitDeleted(reader, deletedAt, deleteStarted, 30*time.Second)
	reader.send("BOUNDARY")
	reader.expect("BOUNDARY", 10*time.Second)
	env.results.add(t, "lossy_failover %q", s.fault("redis-standalone-failover", "lossy"))
	fresh := s.startReader(3115, env.nats[2], false, localShortStaleness, "ROOST_MIRROR_DELETED_AT="+strconv.FormatInt(deletedAt, 10), "ROOST_MIRROR_BOUNDARY=1")
	fresh.send("OBSERVE 6000")
	obs := fresh.expect("OBSERVE", 20*time.Second)
	reader.send("OBSERVE 6000")
	old := reader.expect("OBSERVE", 20*time.Second)
	env.results.add(t, "boundary fresh_reader %s", strings.Join(obs, " "))
	env.results.add(t, "boundary old_reader %s", strings.Join(old, " "))
	for _, r := range []*localChild{reader, fresh} {
		r.kill()
	}
	s.stopChild(owner)
}

// l2Key 是公会快照在 L2 里的完整键（Cluster 切主按它的槽位找主节点）。
func (s *localScenario) l2Key() string {
	key := GuildSummaryMirrorSpec
	return fmt.Sprintf("%s:remote_entity:snapshot:%d:%d:%d:%d:%d", s.l2Prefix, key.Tenant, key.Kind, s.guild, key.Scope, key.Policy)
}

// S5：Mongo stepDown。owner 连续提交、只读方同时直接做权威回源（只读 Mongo loader），期间主节点让位：
// 记录 owner 提交与权威回源在选举期间的错误与恢复时间；最后只读方收敛到 owner 最后确认的版本。
func localMongoStepDown(t *testing.T, env *localEnv) {
	s := env.scenario(t, 96005, false)
	owner := s.startOwner(1106, filepath.Join(env.workDir, "s5-wal"), true, env.nats[0])
	v, at := s.commit(owner, "a")
	reader := s.startReader(3106, env.nats[1], false, localShortStaleness)
	s.waitVersion(reader, v, at, 30*time.Second)
	reader.send("AUTH start")
	reader.expect("AUTH", 10*time.Second)
	owner.send("BURST 60 100")
	time.Sleep(1500 * time.Millisecond)
	env.results.add(t, "stepdown new_primary=%s", s.fault("mongo-stepdown"))
	done := owner.expect("BURSTDONE", 2*time.Minute)
	reader.send("AUTH stop")
	auth := reader.expect("AUTH", 10*time.Second)
	env.results.add(t, "owner_burst %s", strings.Join(done, " "))
	env.results.add(t, "reader_authority %s", strings.Join(auth, " "))
	fields := map[string]string{}
	for _, f := range done {
		if k, val, ok := strings.Cut(f, "="); ok {
			fields[k] = val
		}
	}
	last, lastAt := parseUint(t, fields["lastVersion"]), parseInt(t, fields["lastAt"])
	env.results.add(t, "final_v%d_converge_ms=%d", last, s.waitVersion(reader, last, lastAt, 60*time.Second))
	stats := s.stats(reader)
	env.results.add(t, "reader_stats loads=%s errors=%s maxErrStreakMs=%s", stats["loads"], stats["errors"], stats["maxErrStreakMs"])
	s.stopChild(reader)
	s.stopChild(owner)
	// mongo-1 优先级最高，让位期过后会再选回来（第二次选举）；等它稳定，别让下一个场景的启动撞上。
	settleStarted := time.Now()
	s.fault("mongo-settle")
	env.results.add(t, "priority_takeback_settled_ms=%d", time.Since(settleStarted).Milliseconds())
}

// S6：owner 切换。A 把所有权转给 B（另一个 sid、另一个 WAL），B 冷加载公会后提交；A 之后的写被 fence。
func localOwnerTransfer(t *testing.T, env *localEnv) {
	s := env.scenario(t, 96006, false)
	a := s.startOwner(1107, filepath.Join(env.workDir, "s6-wal-a"), true, env.nats[0])
	v, at := s.commit(a, "a")
	reader := s.startReader(3107, env.nats[1], false, localShortStaleness)
	s.waitVersion(reader, v, at, 30*time.Second)
	v, at = s.commit(a, "b")
	s.waitVersion(reader, v, at, 30*time.Second)
	a.send("TRANSFER 1117")
	if got := a.expectAny(30*time.Second, "TRANSFERRED", "TRANSFERERR"); got[0] != "TRANSFERRED" {
		t.Fatalf("transfer: %v", got)
	}
	transferredAt := time.Now()
	b := s.startOwner(1117, filepath.Join(env.workDir, "s6-wal-b"), false, env.nats[2])
	vb, atb := s.commit(b, "c")
	if vb != v+1 {
		t.Fatalf("the new owner committed version %d, want %d", vb, v+1)
	}
	env.results.add(t, "takeover_ready_ms=%d new_owner_v%d_converge_ms=%d", time.Unix(0, atb).Sub(transferredAt).Milliseconds(), vb, s.waitVersion(reader, vb, atb, 30*time.Second))
	a.send("COMMIT stale 3000")
	if got := a.expectAny(30*time.Second, "COMMITTED", "COMMITERR"); got[0] != "COMMITERR" {
		t.Fatalf("the old owner still commits after the transfer: %v", got)
	} else {
		env.results.add(t, "old_owner_write_refused %q", strings.Join(got[2:], " "))
	}
	vb, atb = s.commit(b, "d")
	env.results.add(t, "new_owner_second_commit_converge_ms=%d", s.waitVersion(reader, vb, atb, 30*time.Second))
	stats := s.stats(reader)
	env.results.add(t, "reader_stats loads=%s errors=%s", stats["loads"], stats["errors"])
	s.stopChild(reader)
	s.stopChild(b)
	s.stopChild(a)
}

// S7：只读服务强杀后用同一身份重启。停机期间 owner 提交的那笔留在 DeliverNew durable 里；重启后首读经
// L2 / 权威，同时 durable 续投的消息进首载缓冲；兴趣以新 generation 续租，之后的提交照常推送。
func localReaderKillRestart(t *testing.T, env *localEnv) {
	s := env.scenario(t, 96007, false)
	owner := s.startOwner(1108, filepath.Join(env.workDir, "s7-wal"), true, env.nats[0])
	v, at := s.commit(owner, "a")
	reader := s.startReader(3108, env.nats[1], false, localShortStaleness)
	s.waitVersion(reader, v, at, 30*time.Second)
	v, at = s.commit(owner, "b")
	s.waitVersion(reader, v, at, 30*time.Second)
	reader.kill()
	killedAt := time.Now()
	v, at = s.commit(owner, "c")
	restarted := s.startReader(3108, env.nats[1], false, localShortStaleness)
	env.results.add(t, "restart_ready_ms=%d first_read_v%d_ms=%d", time.Since(killedAt).Milliseconds(), v, s.waitVersion(restarted, v, at, 30*time.Second))
	v, at = s.commit(owner, "d")
	env.results.add(t, "after_restart_push_ms=%d", s.waitVersion(restarted, v, at, 30*time.Second))
	stats := s.stats(restarted)
	env.results.add(t, "reader_stats loads=%s buffered=%s overflow=%s replayed=%s", stats["loads"], stats["buffered"], stats["overflow"], stats["replayed"])
	s.stopChild(restarted)
	s.stopChild(owner)
}

// ---------------- 子进程管理 ----------------

type localChild struct {
	t     *testing.T
	name  string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan string
	mu    sync.Mutex
	log   bytes.Buffer
	done  chan struct{}
}

func (env *localEnv) startChild(t *testing.T, ctx context.Context, name string, extra []string) *localChild {
	t.Helper()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGeneratedRemoteMirrorLocalProcess$", "-test.v", "-test.count=1", "-test.timeout=10m")
	cmd.Env = append(append(os.Environ(), "ROOST_MIRROR_CHILD=1"), extra...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	child := &localChild{t: t, name: name, cmd: cmd, stdin: stdin, lines: make(chan string, 256), done: make(chan struct{})}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	env.childCount.Add(1)
	go func() {
		defer close(child.lines)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			child.mu.Lock()
			child.log.WriteString(line + "\n")
			child.mu.Unlock()
			if rest, ok := strings.CutPrefix(line, "MIRROR "); ok {
				child.lines <- rest
			}
		}
	}()
	go func() {
		_ = cmd.Wait()
		close(child.done)
	}()
	return child
}

func (c *localChild) output() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.log.String()
	if len(out) > 8000 {
		out = "..." + out[len(out)-8000:]
	}
	return out
}

func (c *localChild) expect(event string, timeout time.Duration) []string {
	c.t.Helper()
	got := c.expectAny(timeout, event)
	return got[1:]
}

// expectAny 等子进程报告 events 之一，返回整行字段（第一个是事件名）；VIOLATION 行记为失败但继续等待。
func (c *localChild) expectAny(timeout time.Duration, events ...string) []string {
	c.t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case line, ok := <-c.lines:
			if !ok {
				c.t.Fatalf("%s exited before %v:\n%s", c.name, events, c.output())
			}
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			if fields[0] == "VIOLATION" {
				c.t.Errorf("%s: %s", c.name, line)
				continue
			}
			for _, event := range events {
				if fields[0] == event {
					return fields
				}
			}
		case <-deadline:
			c.t.Fatalf("%s never reported %v:\n%s", c.name, events, c.output())
		}
	}
}

func (c *localChild) send(command string) {
	c.t.Helper()
	if _, err := fmt.Fprintln(c.stdin, command); err != nil {
		c.t.Fatalf("send %q to %s: %v\n%s", command, c.name, err, c.output())
	}
}

// kill 是 SIGKILL（强杀，不走停机）。
func (c *localChild) kill() {
	select {
	case <-c.done:
		return
	default:
	}
	_ = c.cmd.Process.Kill()
	<-c.done
}

func (c *localChild) wait() {
	c.t.Helper()
	_ = c.stdin.Close()
	select {
	case <-c.done:
	case <-time.After(30 * time.Second):
		c.t.Fatalf("%s did not exit:\n%s", c.name, c.output())
	}
	if code := c.cmd.ProcessState.ExitCode(); code != 0 {
		c.t.Errorf("%s exited %d:\n%s", c.name, code, c.output())
	}
}

func parseUint(t *testing.T, s string) uint64 {
	t.Helper()
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

func parseInt(t *testing.T, s string) int64 {
	t.Helper()
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

// ---------------- 子进程 ----------------

func TestGeneratedRemoteMirrorLocalProcess(t *testing.T) {
	if os.Getenv("ROOST_MIRROR_CHILD") != "1" || os.Getenv("ROOST_MIRROR_LOCAL_ROLE") == "" {
		t.Skip("started by TestGeneratedRemoteMirrorLocal")
	}
	switch os.Getenv("ROOST_MIRROR_LOCAL_ROLE") {
	case "owner":
		runLocalOwner(t)
	case "reader":
		runLocalReader(t)
	default:
		t.Fatal("unknown role")
	}
}

type localChildConfig struct {
	sid      int32
	guild    int64
	prefix   string
	l2Prefix string
	database string
	mongo    string
	redis    string
	cluster  []string
	nats     string
	ignore   bool
}

func readLocalChildConfig(t *testing.T) localChildConfig {
	t.Helper()
	sid, err := strconv.Atoi(os.Getenv("ROOST_MIRROR_SID"))
	if err != nil {
		t.Fatal(err)
	}
	guild, err := strconv.ParseInt(os.Getenv("ROOST_MIRROR_GUILD"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	cfg := localChildConfig{
		sid: int32(sid), guild: guild, prefix: os.Getenv("ROOST_MIRROR_PREFIX"), l2Prefix: os.Getenv("ROOST_MIRROR_L2_PREFIX"),
		database: os.Getenv("ROOST_MIRROR_DATABASE"), mongo: os.Getenv("ROOST_MIRROR_MONGO"), redis: os.Getenv("ROOST_MIRROR_REDIS"),
		nats: os.Getenv("ROOST_MIRROR_NATS"), ignore: os.Getenv("ROOST_MIRROR_NATS_IGNORE_DISCOVERED") == "true",
	}
	if cluster := os.Getenv("ROOST_MIRROR_REDIS_CLUSTER"); cluster != "" {
		cfg.cluster = strings.Split(cluster, ",")
	}
	return cfg
}

func (c localChildConfig) redisClient(t *testing.T) fredis.IRedis {
	t.Helper()
	if len(c.cluster) > 0 {
		cfg := fredis.DefaultConfig("")
		cfg.ClusterAddrs = c.cluster
		return redisdriver.NewRedisClient(cfg)
	}
	client, err := redisdriver.NewClient(fredis.DefaultConfig(c.redis))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func (c localChildConfig) natsClient(t *testing.T) *natsdriver.Client {
	t.Helper()
	client, err := natsdriver.NewClient(fnats.DefaultConfig(c.nats), natsdriver.ClientOptions{IgnoreDiscoveredServers: c.ignore})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// localJetStreamConfig 是 owner 与只读方建同一条流的参数（必须一致）：三副本、内存存储。
func localJetStreamConfig(sid int32, prefix string) syncdriver.JetStreamSyncConfig {
	return syncdriver.JetStreamSyncConfig{LocalSid: sid, Prefix: prefix, Storage: fnats.JetStreamStorageMemory, StreamMaxAge: 10 * time.Minute, AckWait: 2 * time.Second, Replicas: 3}
}

func localReport(format string, args ...any) { fmt.Printf("MIRROR "+format+"\n", args...) }

// holdingStore 在投影前拖住记录（S1：WAL 已持久、Mongo 未提交、未发布时强杀 owner）。
type holdingStore struct {
	*engine.MongoStore
	hold atomic.Bool
	held chan struct{}
}

func (h *holdingStore) Project(ctx context.Context, record coredata.CommitRecord) error {
	_, err := h.ProjectFenced(ctx, record)
	return err
}

func (h *holdingStore) ProjectFenced(ctx context.Context, record coredata.CommitRecord) (bool, error) {
	if h.hold.Load() {
		select {
		case h.held <- struct{}{}:
		default:
		}
		<-ctx.Done() // 进程会被强杀；放行只能来自重启后的重放
		return false, ctx.Err()
	}
	return h.MongoStore.ProjectFenced(ctx, record)
}

func runLocalOwner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := readLocalChildConfig(t)
	create := os.Getenv("ROOST_MIRROR_CREATE") == "true"
	RegisterEntity()
	mongo, err := mongodriver.NewClient(fmongo.DefaultConfig(cfg.mongo), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer mongo.Close(context.Background())
	redis := cfg.redisClient(t)
	defer redis.Close()
	natsClient := cfg.natsClient(t)
	defer natsClient.Close()
	js, err := natsdriver.NewJetStreamClient(natsClient)
	if err != nil {
		t.Fatal(err)
	}
	bus, err := syncdriver.NewJetStreamSyncBus(ctx, js, localJetStreamConfig(cfg.sid, cfg.prefix))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bus.StopWithContext(context.Background()) }()

	// owner 的 L2 键与只读方同一前缀：scopedRedis 给每个键加 "<前缀>:"；锁键带 hash tag（Cluster 下多键 Lua 同槽）。
	scoped := &scopedRedis{IRedis: redis, prefix: cfg.l2Prefix + ":"}
	access := entity.NewManagerAccess(entity.NewEntityManager())
	backend, err := remoteentity.NewBackend(access, remoteentity.NewMongoCommitter(mongo, cfg.database, cfg.sid, 0))
	if err != nil {
		t.Fatal(err)
	}
	rcfg := remoteentity.DefaultConfig()
	rcfg.LockTTL = localOwnerLockTTL
	rcfg.LockKey = "{m6}"
	// 进程代际代替 App 单实例锁（O-M6-6）：编排只在 SIGKILL 之后用同一 sid 重启 owner，旧进程一定已死——正是单实例锁
	// 给出的保证——每次启动一个新 token，重启后的第一次取锁接管上一代留下的共享锁。
	incarnation := &remoteentity.ProcessIncarnation{Holder: fmt.Sprintf("mirror-local:%s:owner:%d", cfg.prefix, cfg.sid), Token: fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())}
	assembly, err := remoteentity.Assemble(remoteentity.AssemblyDeps{Redis: scoped, Backend: backend, Incarnation: incarnation}, rcfg, cfg.sid, remoteentity.MongoBackendConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := assembly.Start(ctx, bus); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = assembly.Stop(context.Background()) }()
	store, err := engine.NewMongoStore(mongo, engine.MongoStoreConfig{DefaultDatabase: cfg.database})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRemoteProjection(backend, assembly.Manager); err != nil {
		t.Fatal(err)
	}
	holding := &holdingStore{MongoStore: store, held: make(chan struct{}, 1)}
	opts := nestwal.DefaultOptions(os.Getenv("ROOST_MIRROR_WAL"))
	wal, err := nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	projector, err := engine.NewProjector(wal, holding, engine.ProjectorOptions{CloseWAL: true})
	if err != nil {
		t.Fatal(err)
	}
	outboxStore, err := engine.NewMongoOutboxStore(store)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := engine.NewOutboxWorker(outboxStore, noEffects{}, engine.OutboxWorkerOptions{Owner: fmt.Sprintf("mirror-local-%d", cfg.sid)})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := engine.NewRuntime(store, wal, projector, outbox, access, assembly.Manager, func(err error) { localReport("FATAL %v", err) }, engine.PipelinedRuntimeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runtime.Shutdown(context.Background()) }()
	rename, remove := nest.NewHandlerName("mirror_local_rename"), nest.NewHandlerName("mirror_local_delete")
	scheduler := nest.NewEngine(append(runtime.NestOptions(), nest.NestOptionWithGetter(access), nest.NestOptionWithRemoteEntityManager(assembly.Manager),
		nest.NestOptionWithWorkerNumAndMsgCap(2, 64),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 2, QueueCap: 64}, nest.WorkerPoolConfig{Workers: 2, QueueCap: 64}))...)
	scheduler.MustRegisterHandlerWithMeta(rename, func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		guild := es[0].(*Guild)
		guild.dao.SetName(params[0].(string))
		guild.dao.SetMembers(guild.dao.GetMembers() + 1)
		return nil, nil
	}, nest.HandlerMeta{Rollback: nest.RollbackState, Durability: nest.DurabilityStrict})
	scheduler.MustRegisterHandlerWithMeta(remove, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		return nil, access.Destroy(context.Background(), es[0], entity.DestroyReasonCommon, true)
	}, nest.HandlerMeta{Rollback: nest.RollbackState, Durability: nest.DurabilityStrict})
	if err := scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = scheduler.Shutdown(context.Background()) }()

	manager := assembly.Manager
	if create {
		if _, err := access.Create(&entity.EntityCreateParam{IsCreate: true, Kind: EntityKindGuild, Id: cfg.guild}); err != nil {
			t.Fatal(err)
		}
	} else if _, err := access.LoadRemoteEntity(ctx, cfg.guild, EntityKindGuild); err != nil {
		t.Fatalf("cold load the guild from Mongo: %v", err)
	}
	lease, err := manager.ClaimRemoteOwnership(ctx, cfg.guild)
	if err != nil {
		t.Fatalf("claim ownership: %v", err)
	}
	if !lease.Shared {
		if _, err := manager.EnterRemoteSharedMode(ctx, cfg.guild); err != nil {
			t.Fatalf("enter shared mode: %v", err)
		}
	}
	version := func() uint64 {
		if value := access.Manager().Get(cfg.guild); value != nil {
			if remote, ok := value.(entity.IThreadSafeRemoteEntity); ok {
				return remote.RemoteVersionVector().StateVersion
			}
		}
		return 0
	}
	request := func(handler nest.HandlerName, timeout time.Duration, params nest.Params) error {
		requestCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		_, err := scheduler.Request(requestCtx, handler, cfg.guild, params)
		return err
	}
	oneLine := func(err error) string { return strings.Join(strings.Fields(err.Error()), " ") }
	localReport("READY version=%d", version())

	commands := bufio.NewScanner(os.Stdin)
	for commands.Scan() {
		fields := strings.Fields(commands.Text())
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "HOLD":
			holding.hold.Store(true)
			localReport("HELD")
		case "COMMIT":
			ms, _ := strconv.Atoi(fields[2])
			started := time.Now()
			if err := request(rename, time.Duration(ms)*time.Millisecond, nest.Params{fields[1]}); err != nil {
				localReport("COMMITERR %d %s", time.Now().UnixNano(), oneLine(err))
				continue
			}
			localReport("COMMITTED %d %d %d", version(), time.Now().UnixNano(), started.UnixNano())
		case "RETRY":
			ms, _ := strconv.Atoi(fields[2])
			deadline := time.Now().Add(time.Duration(ms) * time.Millisecond)
			started := time.Now()
			var lastErr error
			attempts, firstErr := 0, ""
			for {
				attempts++
				lastErr = request(rename, 2*time.Second, nest.Params{fields[1]})
				if lastErr == nil || time.Now().After(deadline) {
					break
				}
				if firstErr == "" {
					firstErr = oneLine(lastErr)
				}
				time.Sleep(100 * time.Millisecond)
			}
			if lastErr != nil {
				localReport("COMMITERR %d %s", time.Now().UnixNano(), oneLine(lastErr))
				continue
			}
			localReport("COMMITTED %d %d %d attempts=%d write_ms=%d first_err=%q", version(), time.Now().UnixNano(), started.UnixNano(), attempts, time.Since(started).Milliseconds(), firstErr)
		case "BURST":
			n, _ := strconv.Atoi(fields[1])
			interval, _ := strconv.Atoi(fields[2])
			var ok, failed int
			var maxMs, lastAt int64
			var lastVersion uint64
			var firstErr string
			var errStart, maxErrWindow time.Duration
			began := time.Now()
			for i := 0; i < n; i++ {
				started := time.Now()
				err := request(rename, 2*time.Second, nest.Params{fmt.Sprintf("burst%d", i)})
				if ms := time.Since(started).Milliseconds(); ms > maxMs {
					maxMs = ms
				}
				if err != nil {
					failed++
					if firstErr == "" {
						firstErr = oneLine(err)
					}
					if errStart == 0 {
						errStart = started.Sub(began)
					}
				} else {
					ok++
					lastVersion, lastAt = version(), time.Now().UnixNano()
					if errStart != 0 {
						if w := time.Since(began) - errStart; w > maxErrWindow {
							maxErrWindow = w
						}
						errStart = 0
					}
				}
				time.Sleep(time.Duration(interval) * time.Millisecond)
			}
			localReport("BURSTDONE ok=%d err=%d lastVersion=%d lastAt=%d maxLatencyMs=%d errWindowMs=%d firstErr=%q", ok, failed, lastVersion, lastAt, maxMs, maxErrWindow.Milliseconds(), firstErr)
		case "DELETE":
			ms, _ := strconv.Atoi(fields[1])
			started := time.Now()
			if err := request(remove, time.Duration(ms)*time.Millisecond, nil); err != nil {
				localReport("DELETEERR %d %s", time.Now().UnixNano(), oneLine(err))
				continue
			}
			localReport("DELETED %d %d", time.Now().UnixNano(), started.UnixNano())
		case "TRANSFER":
			to, _ := strconv.Atoi(fields[1])
			if _, err := manager.TransferRemoteOwnership(ctx, cfg.guild, int32(to)); err != nil {
				localReport("TRANSFERERR %s", oneLine(err))
				continue
			}
			localReport("TRANSFERRED")
		case "STOP":
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
			_ = scheduler.Shutdown(stopCtx)
			_ = runtime.Shutdown(stopCtx)
			_ = assembly.Stop(stopCtx)
			stopCancel()
			localReport("STOPPED")
			return
		}
	}
}

// localReaderState 是只读方后台读循环的观察结果与不变量核对。
type localReaderState struct {
	mu            sync.Mutex
	reads, errors int64
	maxVersion    uint64
	lastFound     bool
	lastVersion   uint64
	lastAt        time.Time
	deleteAt      time.Time // owner 确认删除的时刻（0 = 未删除）
	deletePending bool      // 编排已宣布要删除（推送可能先于确认到达）
	sawAbsent     bool      // 删除之后本进程读到过“不存在”
	boundary      bool      // 边界记录模式：只计数，不判违例（未复制即切主，文档 §3.4）
	bound         uint64    // 最近一次确认的版本
	boundAt       time.Time
	staleness     time.Duration
	errStreak     time.Time
	maxErrStreak  time.Duration
	violations    int64
	firstSeen     map[uint64]time.Time
	firstAbsentAt time.Time
}

func (s *localReaderState) observe(found bool, version uint64, err error, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if err != nil {
		s.errors++
		if s.errStreak.IsZero() {
			s.errStreak = now
		}
		if d := now.Sub(s.errStreak); d > s.maxErrStreak {
			s.maxErrStreak = d
		}
		return
	}
	s.errStreak = time.Time{}
	s.lastFound, s.lastVersion, s.lastAt = found, version, now
	violate := func(format string, args ...any) {
		if s.boundary {
			return
		}
		s.violations++
		if s.violations <= 5 {
			localReport("VIOLATION "+format, args...)
		}
	}
	if found {
		if version < s.maxVersion {
			violate("regression read v%d after v%d", version, s.maxVersion)
		}
		if version > s.maxVersion {
			s.maxVersion = version
		}
		if _, ok := s.firstSeen[version]; !ok {
			s.firstSeen[version] = now
		}
		if !s.deleteAt.IsZero() && s.sawAbsent {
			violate("resurrected v%d after the delete was observed", version)
		}
		if !s.deleteAt.IsZero() && now.After(s.deleteAt.Add(s.staleness+localBoundGrace)) {
			violate("still found v%d %v after the delete was confirmed", version, now.Sub(s.deleteAt))
		}
		if s.bound > 0 && version < s.bound && now.After(s.boundAt.Add(s.staleness+localBoundGrace)) {
			violate("read v%d %v after v%d was confirmed (staleness %v)", version, now.Sub(s.boundAt), s.bound, s.staleness)
		}
		return
	}
	if !s.deleteAt.IsZero() || s.deletePending {
		if !s.sawAbsent {
			s.firstAbsentAt = now
		}
		s.sawAbsent = true
	} else if s.maxVersion > 0 {
		violate("absent after v%d without a delete", s.maxVersion)
	}
}

func runLocalReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := readLocalChildConfig(t)
	staleness, err := time.ParseDuration(os.Getenv("ROOST_MIRROR_STALENESS"))
	if err != nil {
		t.Fatal(err)
	}
	vcfg := viper.New()
	vcfg.Set("server_type", "guildview")
	vcfg.Set("sid", int(cfg.sid))
	vcfg.Set("syncbus.transport", "jetstream")
	vcfg.Set("syncbus.prefix", cfg.prefix)
	vcfg.Set("syncbus.storage", "memory")
	vcfg.Set("syncbus.stream_max_age", "10m")
	vcfg.Set("syncbus.ack_wait", "2s")
	vcfg.Set("syncbus.replicas", 3)
	vcfg.Set("remote_entity.mongo.database", cfg.database)
	vcfg.Set("remote_entity.snapshot_l2_key_prefix", cfg.l2Prefix)
	vcfg.Set("remote_entity.snapshot_cache_ttl", "10m")
	vcfg.Set("remote_entity.cached_max_staleness", staleness.String())
	if err := app.ValidateServiceConfig(vcfg); err != nil {
		t.Fatal(err)
	}
	registry := app.NewRegistry(vcfg)
	redis := cfg.redisClient(t)
	defer redis.Close()
	mongo, err := mongodriver.NewClient(fmongo.DefaultConfig(cfg.mongo), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer mongo.Close(context.Background())
	natsClient := cfg.natsClient(t)
	defer natsClient.Close()
	js, err := natsdriver.NewJetStreamClient(natsClient)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[app.ModName]any{
		mods.ModRedis: redis, mods.ModMongo: fmongo.IMongo(mongo),
		mods.ModNats: fnats.IClient(natsClient), mods.ModNatsJetStream: fnats.IJetStream(js),
	} {
		if err := registry.Register(name, value); err != nil {
			t.Fatal(err)
		}
	}
	// 权威回源计数：正式的只读 Mongo loader 外面包一层计数（WithMirrorLoader 是 kit 的正式选项）。
	authority := remoteentity.NewMongoSnapshotLoader(mongo, cfg.database)
	var loads atomic.Int64
	counted := func(ctx context.Context, key entity.RemoteSnapshotKey, consistency entity.RemoteReadConsistency, minVersion uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
		loads.Add(1)
		return authority(ctx, key, consistency, minVersion)
	}
	busMod := kitsyncbus.NewSyncBusMod(0)
	mirrorMod := kitremote.NewRemoteMirrorMod(0, kitremote.WithMirrorLoader(counted, false))
	for _, mod := range []app.Mod{busMod, mirrorMod} {
		if err := mod.Init(vcfg); err != nil {
			t.Fatal(err)
		}
		if err := mod.Provide(registry); err != nil {
			t.Fatal(err)
		}
	}
	for _, mod := range []app.Mod{busMod, mirrorMod} {
		if err := mod.Start(); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		_ = mirrorMod.StopWithContext(context.Background())
		_ = busMod.StopWithContext(context.Background())
	}()
	source, err := kitremote.MirrorSource(registry)
	if err != nil {
		t.Fatal(err)
	}
	client, _ := source.(*remoteentity.SnapshotClient)
	reader, err := NewGuildSummaryReader(source)
	if err != nil {
		t.Fatal(err)
	}
	state := &localReaderState{staleness: staleness, firstSeen: map[uint64]time.Time{}}
	// 删除之后才起的只读方：从一开始就知道删除的确认时刻（有界收敛与不复活照样核对）。
	if deleted, err := strconv.ParseInt(os.Getenv("ROOST_MIRROR_DELETED_AT"), 10, 64); err == nil {
		state.deleteAt = time.Unix(0, deleted)
	}
	state.boundary = os.Getenv("ROOST_MIRROR_BOUNDARY") == "1"
	read := func() (bool, uint64, error) {
		readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		got, found, err := reader.Read(readCtx, cfg.guild, entity.RemoteReadCached, entity.RemoteObservation{})
		return found, got.Observation.StateVersion, err
	}
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		for ctx.Err() == nil {
			found, version, err := read()
			state.observe(found, version, err, time.Now())
			time.Sleep(5 * time.Millisecond)
		}
	}()
	push := client != nil && client.Stats().PushEnabled
	localReport("READY push=%v", push)

	// 权威回源循环（S5）：直接调正式的只读 Mongo loader，记录选举期间的错误与最长不可用时间。
	var authStop chan struct{}
	var authDone chan string
	commands := bufio.NewScanner(os.Stdin)
	for commands.Scan() {
		fields := strings.Fields(commands.Text())
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "PREDEL":
			state.mu.Lock()
			state.deletePending = true
			state.mu.Unlock()
			localReport("PREDEL")
		case "BOUNDARY":
			state.mu.Lock()
			state.boundary = true
			state.mu.Unlock()
			localReport("BOUNDARY")
		case "PEEK":
			found, version, err := read()
			switch {
			case err != nil:
				localReport("PEEK error %s", strings.Join(strings.Fields(err.Error()), " "))
			case !found:
				localReport("PEEK absent")
			default:
				localReport("PEEK %d", version)
			}
		case "WAIT", "WAITDEL":
			var version uint64
			arg := 1
			if fields[0] == "WAIT" {
				version, _ = strconv.ParseUint(fields[1], 10, 64)
				arg = 2
			}
			confirmed, _ := strconv.ParseInt(fields[arg], 10, 64)
			timeoutMs, _ := strconv.Atoi(fields[arg+1])
			confirmedAt := time.Unix(0, confirmed)
			state.mu.Lock()
			if fields[0] == "WAIT" {
				state.bound, state.boundAt = version, confirmedAt
			} else {
				state.deleteAt = confirmedAt
			}
			state.mu.Unlock()
			deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
			for {
				state.mu.Lock()
				seen, ok := state.firstSeen[version], false
				if fields[0] == "WAIT" {
					for v, at := range state.firstSeen {
						if v >= version && (seen.IsZero() || at.Before(seen)) {
							seen = at
						}
					}
					ok = !seen.IsZero()
				} else {
					seen, ok = state.firstAbsentAt, state.sawAbsent
				}
				last := state.lastVersion
				state.mu.Unlock()
				if ok {
					localReport("CONVERGED %d %d %d", version, seen.Sub(confirmedAt).Milliseconds(), seen.UnixNano())
					break
				}
				if time.Now().After(deadline) {
					localReport("TIMEOUT %d last=%d", version, last)
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
		case "OBSERVE":
			// 边界记录：在给定时长里统计读到“存在 / 不存在 / 错误”的次数（不判违例）。
			ms, _ := strconv.Atoi(fields[1])
			var present, absent, failed int
			var presentVersion uint64
			end := time.Now().Add(time.Duration(ms) * time.Millisecond)
			for time.Now().Before(end) {
				found, version, err := read()
				switch {
				case err != nil:
					failed++
				case found:
					present++
					presentVersion = version
				default:
					absent++
				}
				time.Sleep(20 * time.Millisecond)
			}
			localReport("OBSERVE present=%d absent=%d errors=%d presentVersion=%d loads=%d", present, absent, failed, presentVersion, loads.Load())
		case "AUTH":
			if fields[1] == "start" {
				authStop, authDone = make(chan struct{}), make(chan string, 1)
				key := reader.Key(cfg.guild)
				go func(stop chan struct{}, done chan string) {
					var ok, failed int
					var streakStart time.Time
					var maxStreak time.Duration
					var firstErr string
					for {
						select {
						case <-stop:
							done <- fmt.Sprintf("ok=%d err=%d maxUnavailableMs=%d firstErr=%q", ok, failed, maxStreak.Milliseconds(), firstErr)
							return
						case <-time.After(50 * time.Millisecond):
						}
						loadCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
						_, _, err := authority(loadCtx, key, entity.RemoteReadMonotonic, 0)
						cancel()
						now := time.Now()
						if err != nil {
							failed++
							if firstErr == "" {
								firstErr = strings.Join(strings.Fields(err.Error()), " ")
							}
							if streakStart.IsZero() {
								streakStart = now
							}
							if d := now.Sub(streakStart); d > maxStreak {
								maxStreak = d
							}
							continue
						}
						ok++
						streakStart = time.Time{}
					}
				}(authStop, authDone)
				localReport("AUTH started")
			} else {
				close(authStop)
				localReport("AUTH %s", <-authDone)
			}
		case "STATS":
			var stats remoteentity.SnapshotClientStats
			if client != nil {
				stats = client.Stats()
			}
			state.mu.Lock()
			localReport("STATS reads=%d errors=%d loads=%d maxv=%d push=%v interestRefused=%d buffered=%d overflow=%d replayed=%d maxErrStreakMs=%d violations=%d",
				state.reads, state.errors, loads.Load(), state.maxVersion, stats.PushEnabled, stats.InterestRejected,
				stats.Bootstrap.Buffered, stats.Bootstrap.Overflows, stats.Bootstrap.Replayed, state.maxErrStreak.Milliseconds(), state.violations)
			state.mu.Unlock()
		case "STOP":
			cancel()
			<-monitorDone
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := mirrorMod.StopWithContext(stopCtx)
			stopCancel()
			if err != nil {
				t.Fatalf("stop: %v", err)
			}
			localReport("STOPPED")
			return
		}
	}
}
