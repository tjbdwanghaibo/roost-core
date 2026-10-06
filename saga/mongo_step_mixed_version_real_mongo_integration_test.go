//go:build integration

package saga

// RR-20261006-15 的滚动升级验证：修前（5ca32611^）与修后的 Mongo 步骤进程同时处理同一操作实例。
//
// 做法：“旧进程”是修前源码编译的测试二进制，作为子进程按 ROOST_SAGA_MIXED_ROLE 执行一次（或一组并发）Handle；
// “新进程”是当前测试进程。两份代码不能编进同一个二进制（同一个包），在测试里手抄一份“旧路径”又只是对旧代码的
// 转述，所以用修前源码真编出来的进程。本文件只用两个版本都有的符号，原样复制进修前源码的 saga/ 就能编出旧进程：
//
//	git worktree add --detach <old> 5ca32611^
//	cp saga/mongo_step_mixed_version_real_mongo_integration_test.go <old>/saga/
//	(cd <old> && GOWORK=off go test -c -tags integration -o <old-bin> ./saga/)
//	source <私有副本集 env.sh>
//	ROOST_SAGA_MIXED_OLD_BINARY=<old-bin> GOWORK=off go test -tags integration -count=1 \
//	  -run '^TestRealMongoMixedVersion' ./saga/
//
// 没有设 ROOST_SAGA_MIXED_OLD_BINARY 时跳过。业务写都在 handler 的 Mongo 事务里、按 CommandID 插一份文档，
// 文档数就是生效次数。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/driver"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	mixedRoleEnv      = "ROOST_SAGA_MIXED_ROLE"
	mixedOldBinaryEnv = "ROOST_SAGA_MIXED_OLD_BINARY"
	mixedResultPrefix = "MIXED_RESULT "
	mixedInHandler    = "MIXED_IN_HANDLER"
)

// mixedStepAction 是一次（或一组并发的）Handle：Attempts 里的每个尝试各跑一次，Result 决定 handler 的结论。
type mixedStepAction struct {
	Database    string
	Owner       string
	LeaseMillis int64
	Operation   string
	Incarnation uint32
	Attempts    []uint32
	// CreatedAt / DeadlineAt 固定下来，两个进程构造出同一个命令（摘要相同）。
	CreatedAt, DeadlineAt int64
	// Result：success / refused / retryable / crash / hang（拿到租约、进了 handler 之后停住：子进程等调用方杀掉，本进程等 release）。
	Result string
	// RetryInFlight：遇到另一次尝试持有有效租约时重试到这个时限（毫秒），并发场景用。
	RetryInFlight int64
}

type mixedStepResult struct {
	CommandID string
	Completed string // 返回的 completion 属于哪次尝试
	Success   bool
	Retryable bool
	Duplicate bool
	Kind      string // ok / in_flight / superseded / fenced / conflict / expired / error
	Err       string
}

func (a mixedStepAction) command(attempt uint32) Command {
	return Command{
		ID: commandID(a.Operation, a.Incarnation, attempt), IdempotencyKey: a.Operation, SagaID: "gift-1", SagaType: "gift",
		DefinitionVersion: 1, BusinessKey: "g-1", StepName: "deliver", Phase: PhaseForward, Attempt: attempt,
		Topic: "gift.deliver", Payload: []byte("state"),
		CreatedAt: time.Unix(0, a.CreatedAt).UTC(), DeadlineAt: time.Unix(0, a.DeadlineAt).UTC(),
	}
}

func mixedErrorKind(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, errOperationAttemptInFlight):
		return "in_flight"
	case errors.Is(err, errAttemptSuperseded):
		return "superseded"
	case errors.Is(err, errAttemptFenced):
		return "fenced"
	case errors.Is(err, ErrConflict):
		return "conflict"
	case errors.Is(err, ErrCommandExpired):
		return "expired"
	default:
		return "error"
	}
}

// runMixedStepAction 是两个版本共用的执行体：旧进程在子进程里调它，新进程在测试进程里调它。
// hang 时 entered 收到通知后 handler 阻塞到 release 关闭（子进程里 release 为 nil，永远阻塞，等调用方杀进程）。
func runMixedStepAction(ctx context.Context, client fmongo.IMongo, action mixedStepAction, entered func(), release <-chan struct{}) []mixedStepResult {
	inbox, err := NewMongoCommandInbox(client, action.Database, "steps", CommandInboxOptions{
		Owner: action.Owner, LeaseDuration: time.Duration(action.LeaseMillis) * time.Millisecond,
	})
	if err == nil {
		err = inbox.EnsureInfrastructure(ctx)
	}
	if err != nil {
		return []mixedStepResult{{Kind: "error", Err: err.Error()}}
	}
	business := client.Database(action.Database).Collection("business")
	handler := func(txCtx context.Context, c Command) (Completion, error) {
		switch action.Result {
		case "success", "hang":
			if action.Result == "hang" {
				// 停在事务的第一条命令之前：被杀的进程不在服务端留下打开的事务（它会占着库直到
				// transactionLifetimeLimitSeconds，清理时 Drop 等不到）。租约与 claim 的状态和停在业务写之后相同。
				entered()
				<-release
			}
			if _, err := business.InsertOne(txCtx, bson.M{"_id": c.ID, "operation": c.IdempotencyKey, "owner": action.Owner}); err != nil {
				return Completion{}, err
			}
			return Completion{Success: true, Data: []byte("by " + action.Owner)}, nil
		case "refused":
			return Completion{Error: "refused by " + action.Owner}, nil
		case "retryable":
			return Completion{Retryable: true, Error: "dependency down"}, nil
		default:
			return Completion{}, errors.New("handler crashed")
		}
	}
	results := make([]mixedStepResult, len(action.Attempts))
	var wg sync.WaitGroup
	for i, attempt := range action.Attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			command := action.command(attempt)
			giveUp := time.Now().Add(time.Duration(action.RetryInFlight) * time.Millisecond)
			for {
				completion, duplicate, err := inbox.Handle(ctx, command, handler)
				results[i] = mixedStepResult{
					CommandID: command.ID, Completed: completion.CommandID, Success: completion.Success, Retryable: completion.Retryable,
					Duplicate: duplicate, Kind: mixedErrorKind(err),
				}
				if err != nil {
					results[i].Err = err.Error()
				}
				if results[i].Kind != "in_flight" || time.Now().After(giveUp) {
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()
	}
	wg.Wait()
	return results
}

// TestMixedVersionStepRole 是旧进程的入口：只在 ROOST_SAGA_MIXED_ROLE 设置时运行，结果按行打印到 stdout。
func TestMixedVersionStepRole(t *testing.T) {
	raw := os.Getenv(mixedRoleEnv)
	if raw == "" {
		t.Skip(mixedRoleEnv + " is not set; this is the old-process entry of TestRealMongoMixedVersion*")
	}
	var action mixedStepAction
	if err := json.Unmarshal([]byte(raw), &action); err != nil {
		t.Fatal(err)
	}
	client, err := driver.NewClient(fmongo.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")), driver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())
	entered := func() { fmt.Println(mixedInHandler) }
	for _, result := range runMixedStepAction(context.Background(), client, action, entered, nil) {
		line, _ := json.Marshal(result)
		fmt.Println(mixedResultPrefix + string(line))
	}
}

type mixedHarness struct {
	t        *testing.T
	binary   string
	client   fmongo.IMongo
	database string
	created  int64
	deadline int64
}

func newMixedHarness(t *testing.T) *mixedHarness {
	binary := os.Getenv(mixedOldBinaryEnv)
	if binary == "" {
		t.Skip(mixedOldBinaryEnv + " is not set; build the pre-fix test binary first (see the file comment)")
	}
	client, database := realSagadirMongo(t)
	now := time.Now().UTC()
	return &mixedHarness{t: t, binary: binary, client: client, database: database,
		created: now.UnixNano(), deadline: now.Add(time.Hour).UnixNano()}
}

func (h *mixedHarness) action(owner, operation string, incarnation uint32, result string, attempts ...uint32) mixedStepAction {
	return mixedStepAction{
		Database: h.database, Owner: owner, LeaseMillis: time.Minute.Milliseconds(), Operation: operation, Incarnation: incarnation,
		Attempts: attempts, CreatedAt: h.created, DeadlineAt: h.deadline, Result: result,
	}
}

// old 在修前二进制的子进程里执行 action，返回它打印的结果。
func (h *mixedHarness) old(action mixedStepAction) []mixedStepResult {
	h.t.Helper()
	cmd := h.oldCommand(action)
	out, err := cmd.Output()
	if err != nil {
		h.t.Fatalf("old process %+v: %v\n%s", action, err, out)
	}
	return parseMixedResults(h.t, string(out))
}

func (h *mixedHarness) oldCommand(action mixedStepAction) *exec.Cmd {
	raw, err := json.Marshal(action)
	if err != nil {
		h.t.Fatal(err)
	}
	cmd := exec.Command(h.binary, "-test.run", "^TestMixedVersionStepRole$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), mixedRoleEnv+"="+string(raw))
	return cmd
}

func parseMixedResults(t *testing.T, out string) []mixedStepResult {
	t.Helper()
	var results []mixedStepResult
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, mixedResultPrefix); ok {
			var result mixedStepResult
			if err := json.Unmarshal([]byte(rest), &result); err != nil {
				t.Fatal(err)
			}
			results = append(results, result)
		}
	}
	if len(results) == 0 {
		t.Fatalf("old process printed no result:\n%s", out)
	}
	return results
}

// new 在本进程（修后代码）里执行 action。
func (h *mixedHarness) new(action mixedStepAction) []mixedStepResult {
	return runMixedStepAction(context.Background(), h.client, action, func() {}, nil)
}

func (h *mixedHarness) one(results []mixedStepResult) mixedStepResult {
	h.t.Helper()
	if len(results) != 1 {
		h.t.Fatalf("got %d results, want 1: %+v", len(results), results)
	}
	return results[0]
}

func (h *mixedHarness) claim(operation string, incarnation, attempt uint32) bson.M {
	h.t.Helper()
	var doc bson.M
	id := stepClaimID(commandID(operation, incarnation, attempt))
	if err := h.client.Database(h.database).Collection("steps"+mongoInboxClaimSuffix).FindOne(context.Background(), bson.M{"_id": id}, &doc); err != nil {
		h.t.Fatalf("claim %s: %v", id, err)
	}
	return doc
}

func (h *mixedHarness) effects(operation string) int64 {
	h.t.Helper()
	count, err := h.client.Database(h.database).Collection("business").CountDocuments(context.Background(), bson.M{"operation": operation})
	if err != nil {
		h.t.Fatal(err)
	}
	return count
}

func wantExecuted(t *testing.T, who string, r mixedStepResult, success bool) {
	t.Helper()
	if r.Kind != "ok" || r.Duplicate || r.Completed != r.CommandID || r.Success != success {
		t.Fatalf("%s %s: %+v, want it to execute (success=%v)", who, r.CommandID, r, success)
	}
}

func wantReplayed(t *testing.T, who string, r mixedStepResult, of string, success bool) {
	t.Helper()
	if r.Kind != "ok" || !r.Duplicate || r.Completed != of || r.Success != success {
		t.Fatalf("%s %s: %+v, want the completion of %s replayed (success=%v)", who, r.CommandID, r, of, success)
	}
}

func wantOutcome(t *testing.T, who string, claim bson.M, outcome string) {
	t.Helper()
	got, has := claim["outcome"]
	if outcome == "" && has || outcome != "" && got != outcome {
		t.Fatalf("%s claim %v: outcome=%v (present=%v), want %q", who, claim["_id"], got, has, outcome)
	}
}

// 交替：旧进程的可重试失败、新进程 handler 出错后被旧进程接替、新进程读旧 claim 后执行成功、两边回放、Resume 后回放。
func TestRealMongoMixedVersionAlternatingAttemptsTakeEffectOnce(t *testing.T) {
	h := newMixedHarness(t)
	op := "gift-1:1:0"
	wantExecuted(t, "old", h.one(h.old(h.action("old", op, 0, "retryable", 1))), false)
	wantOutcome(t, "old", h.claim(op, 0, 1), "")
	if r := h.one(h.new(h.action("new", op, 0, "crash", 2))); r.Kind != "error" {
		t.Fatalf("new attempt 2: %+v, want the handler error", r)
	}
	// 新进程交还了租约（过期的 pending）：旧进程按全部 claim 读到它、接替并执行。
	wantExecuted(t, "old", h.one(h.old(h.action("old", op, 0, "retryable", 3))), false)
	if status := h.claim(op, 0, 2)["status"]; status != claimStatusSuperseded {
		t.Fatalf("new attempt 2 claim status %v after the old process took over, want superseded", status)
	}
	// 新进程读两份不带 outcome 的旧 claim（可重试失败），照常执行。
	wantExecuted(t, "new", h.one(h.new(h.action("new", op, 0, "success", 4))), true)
	wantOutcome(t, "new", h.claim(op, 0, 4), "success")
	success := commandID(op, 0, 4)
	// 旧进程读到带 outcome 的成功 claim：解码不出错，回放它。
	wantReplayed(t, "old", h.one(h.old(h.action("old", op, 0, "success", 5))), success, true)
	wantReplayed(t, "new", h.one(h.new(h.action("new", op, 0, "success", 6))), success, true)
	// Resume 之后的新一生：两边都回放旧一生的成功。
	wantReplayed(t, "old", h.one(h.old(h.action("old", op, 1, "success", 1))), success, true)
	wantReplayed(t, "new", h.one(h.new(h.action("new", op, 1, "success", 2))), success, true)
	// 同一命令的重投（新进程写的成功被旧进程按自己的回执读）。
	wantReplayed(t, "old", h.one(h.old(h.action("old", op, 0, "success", 4))), success, true)
	if n := h.effects(op); n != 1 {
		t.Fatalf("operation %s took effect %d times, want 1", op, n)
	}
}

// 拒绝按代际回放：新进程写的第 0 代拒绝（不带 incarnation）被旧进程回放；旧进程写的第 1 代拒绝（不带 outcome）被新进程
// 在同一生里回放、在下一生里不回放。
func TestRealMongoMixedVersionRefusalsReplayWithinTheirLife(t *testing.T) {
	h := newMixedHarness(t)
	op := "gift-1:1:1"
	wantExecuted(t, "new", h.one(h.new(h.action("new", op, 0, "refused", 1))), false)
	wantOutcome(t, "new", h.claim(op, 0, 1), "refused")
	refusal0 := commandID(op, 0, 1)
	wantReplayed(t, "old", h.one(h.old(h.action("old", op, 0, "success", 2))), refusal0, false)
	wantExecuted(t, "old", h.one(h.old(h.action("old", op, 1, "refused", 1))), false)
	wantOutcome(t, "old", h.claim(op, 1, 1), "")
	refusal1 := commandID(op, 1, 1)
	wantReplayed(t, "new", h.one(h.new(h.action("new", op, 1, "success", 2))), refusal1, false)
	wantReplayed(t, "old", h.one(h.old(h.action("old", op, 1, "success", 3))), refusal1, false)
	wantExecuted(t, "new", h.one(h.new(h.action("new", op, 2, "success", 1))), true)
	success := commandID(op, 2, 1)
	wantReplayed(t, "old", h.one(h.old(h.action("old", op, 2, "success", 2))), success, true)
	wantReplayed(t, "new", h.one(h.new(h.action("new", op, 0, "success", 3))), success, true)
	if n := h.effects(op); n != 1 {
		t.Fatalf("operation %s took effect %d times, want 1", op, n)
	}
}

// 在途与接替跨版本：旧进程在 handler 里被杀，新进程在它的租约内等待、过期后接替执行；新进程停在 handler 里时旧进程
// 等待、过期后接替执行，新进程随后的提交被 fence。
func TestRealMongoMixedVersionInFlightAttemptsAreFencedAcrossVersions(t *testing.T) {
	h := newMixedHarness(t)
	const lease = 3 * time.Second

	t.Run("old process killed in its handler", func(t *testing.T) {
		op := "gift-1:1:2"
		action := h.action("old", op, 0, "hang", 1)
		action.LeaseMillis = lease.Milliseconds()
		cmd := h.oldCommand(action)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		lines := bufio.NewScanner(stdout)
		for lines.Scan() && lines.Text() != mixedInHandler {
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if r := h.one(h.new(h.action("new", op, 0, "success", 2))); r.Kind != "in_flight" {
			t.Fatalf("new attempt 2 within the old lease: %+v, want in_flight", r)
		}
		next := h.action("new", op, 0, "success", 2)
		next.RetryInFlight = (2 * lease).Milliseconds()
		wantExecuted(t, "new", h.one(h.new(next)), true)
		if status := h.claim(op, 0, 1)["status"]; status != claimStatusSuperseded {
			t.Fatalf("killed old attempt claim status %v, want superseded", status)
		}
		if r := h.one(h.old(h.action("old", op, 0, "success", 1))); r.Kind != "superseded" {
			t.Fatalf("old redelivery of the superseded attempt: %+v, want superseded", r)
		}
		if n := h.effects(op); n != 1 {
			t.Fatalf("operation %s took effect %d times, want 1", op, n)
		}
	})

	t.Run("new process holding its transaction", func(t *testing.T) {
		op := "gift-1:1:3"
		action := h.action("new", op, 0, "hang", 1)
		action.LeaseMillis = lease.Milliseconds()
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		done := make(chan mixedStepResult, 1)
		go func() {
			done <- runMixedStepAction(context.Background(), h.client, action, func() { once.Do(func() { close(entered) }) }, release)[0]
		}()
		<-entered
		if r := h.one(h.old(h.action("old", op, 0, "success", 2))); r.Kind != "in_flight" {
			t.Fatalf("old attempt 2 within the new lease: %+v, want in_flight", r)
		}
		next := h.action("old", op, 0, "success", 2)
		next.RetryInFlight = (2 * lease).Milliseconds()
		wantExecuted(t, "old", h.one(h.old(next)), true)
		close(release)
		if r := <-done; r.Kind != "fenced" {
			t.Fatalf("new attempt 1 committing after the old process took over: %+v, want fenced", r)
		}
		wantReplayed(t, "new", h.one(h.new(h.action("new", op, 0, "success", 3))), commandID(op, 0, 2), true)
		if n := h.effects(op); n != 1 {
			t.Fatalf("operation %s took effect %d times, want 1", op, n)
		}
	})
}

// 并发：两个进程各 4 个尝试同时 Reserve 同一操作，生效恰好一次，其余都回放它。
func TestRealMongoMixedVersionConcurrentAttemptsTakeEffectOnce(t *testing.T) {
	h := newMixedHarness(t)
	for round := 0; round < 5; round++ {
		op := fmt.Sprintf("gift-1:2:%d", round)
		oldAction := h.action("old", op, 0, "success", 1, 3, 5, 7)
		newAction := h.action("new", op, 0, "success", 2, 4, 6, 8)
		oldAction.RetryInFlight, newAction.RetryInFlight = 20_000, 20_000
		var oldResults []mixedStepResult
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); oldResults = h.old(oldAction) }()
		newResults := h.new(newAction)
		wg.Wait()
		var executed []string
		all := append(oldResults, newResults...)
		for _, r := range all {
			if r.Kind != "ok" || !r.Success {
				t.Fatalf("round %d %s: %+v, want executed or replayed", round, r.CommandID, r)
			}
			if !r.Duplicate {
				executed = append(executed, r.CommandID)
			}
		}
		if len(executed) != 1 {
			t.Fatalf("round %d: %d attempts executed %v, want exactly 1; results %+v", round, len(executed), executed, all)
		}
		for _, r := range all {
			if r.Completed != executed[0] {
				t.Fatalf("round %d %s returned the completion of %s, want %s", round, r.CommandID, r.Completed, executed[0])
			}
		}
		if n := h.effects(op); n != 1 {
			t.Fatalf("round %d: operation %s took effect %d times, want 1", round, op, n)
		}
	}
}

// 上限只挡旧进程：一个操作累积 4100 份带 outcome 的可重试失败后，旧进程按全部 claim 计数报 ErrConflict 且什么都不写，
// 新进程照常执行；之后旧进程仍报 ErrConflict（回放不了），新进程回放。
func TestRealMongoMixedVersionAttemptCapOnlyStopsTheOldProcess(t *testing.T) {
	h := newMixedHarness(t)
	op := "gift-1:1:4"
	wantExecuted(t, "new", h.one(h.new(h.action("new", op, 0, "retryable", 1))), false)
	cloneClaims(t, h.client.Database(h.database).Collection("steps"+mongoInboxClaimSuffix), h.claim(op, 0, 1), op, 4100, false)
	claims := h.client.Database(h.database).Collection("steps" + mongoInboxClaimSuffix)
	before, err := claims.CountDocuments(context.Background(), bson.M{})
	if err != nil {
		t.Fatal(err)
	}
	if r := h.one(h.old(h.action("old", op, 5, "success", 1))); r.Kind != "conflict" || !strings.Contains(r.Err, "more than 4096 attempts") {
		t.Fatalf("old process with 4100 claims: %+v, want its ErrConflict", r)
	}
	if after, err := claims.CountDocuments(context.Background(), bson.M{}); err != nil || after != before {
		t.Fatalf("old process at the cap changed the claims collection: %d -> %d (err=%v), want its transaction to write nothing", before, after, err)
	}
	wantExecuted(t, "new", h.one(h.new(h.action("new", op, 5, "success", 2))), true)
	if r := h.one(h.old(h.action("old", op, 5, "success", 3))); r.Kind != "conflict" {
		t.Fatalf("old process after the new process took effect: %+v, want its ErrConflict", r)
	}
	wantReplayed(t, "new", h.one(h.new(h.action("new", op, 5, "success", 4))), commandID(op, 5, 2), true)
	if n := h.effects(op); n != 1 {
		t.Fatalf("operation %s took effect %d times, want 1", op, n)
	}
}

// 旧进程在上限边上还能再写一份（它看到 4096 份时放行），这样留下的 4097 份不带 outcome 的 claim 不能挡住新进程
// （RR-20261006-16 在混跑下的形态）。
func TestRealMongoMixedVersionOldProcessAtTheCapDoesNotStopTheNewProcess(t *testing.T) {
	h := newMixedHarness(t)
	op := "gift-1:1:5"
	wantExecuted(t, "old", h.one(h.old(h.action("old", op, 0, "retryable", 1))), false)
	cloneClaims(t, h.client.Database(h.database).Collection("steps"+mongoInboxClaimSuffix), h.claim(op, 0, 1), op, 4096, true)
	wantExecuted(t, "old", h.one(h.old(h.action("old", op, 5, "retryable", 1))), false)
	if r := h.one(h.old(h.action("old", op, 5, "success", 2))); r.Kind != "conflict" {
		t.Fatalf("old process with 4097 claims: %+v, want its ErrConflict", r)
	}
	wantExecuted(t, "new", h.one(h.new(h.action("new", op, 5, "success", 3))), true)
	wantReplayed(t, "new", h.one(h.new(h.action("new", op, 6, "success", 1))), commandID(op, 5, 3), true)
	if n := h.effects(op); n != 1 {
		t.Fatalf("operation %s took effect %d times, want 1", op, n)
	}
}

// cloneClaims 把 template 复制到 total 份（含 template 本身），分摊在第 0～4 代；stripOutcome 时去掉 outcome，
// 等于修前进程写的 claim。
func cloneClaims(t *testing.T, claims fmongo.ICollection, template bson.M, operation string, total int, stripOutcome bool) {
	t.Helper()
	copies := make([]any, 0, total-1)
	for n := 1; n < total; n++ {
		incarnation, attempt := uint32(n/820), uint32(n%820)+1
		id := commandID(operation, incarnation, attempt)
		clone := bson.M{}
		for key, value := range template {
			clone[key] = value
		}
		if stripOutcome {
			delete(clone, "outcome")
		}
		clone["_id"], clone["command_id"], clone["incarnation"] = stepClaimID(id), id, incarnation
		copies = append(copies, clone)
	}
	if _, err := claims.InsertMany(context.Background(), copies); err != nil {
		t.Fatal(err)
	}
}
