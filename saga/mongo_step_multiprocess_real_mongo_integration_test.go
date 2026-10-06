//go:build integration

package saga

// 两个 Mongo 步骤进程处理同一操作实例（真实副本集）。原来的混跑用例（新旧版本两个进程，RR-20261006-15 复核）随收件箱改为
// 每个操作一份状态文档而删除（维护者 2026-10-06：不兼容旧进程，见 docs/feature/SAGA-OPERATION-STATE-DOC-2026-10-06.md）；
// 其中验证契约本身、别处没有覆盖的三个跨进程场景留在这里，两边都是当前代码：
//
//   - 进程在 handler 里被 SIGKILL：另一进程在它的租约内等待，过期后接替执行；被杀尝试的重投不执行；
//   - 进程停在执行事务里：另一进程在租约内等待、过期后接替执行，停住的进程随后提交被 fence；
//   - 两个进程各 4 个尝试同时处理同一操作：恰好一个执行，其余回放它。
//
// “另一进程”是当前测试二进制自己（os.Args[0]），按 ROOST_SAGA_STEP_PROCESS_ROLE 执行一次（或一组并发）Handle，结果按行打印。
// 业务写都在 handler 的 Mongo 事务里、按 CommandID 插一份文档，文档数就是生效次数。
//
//	GOWORK=off go test -tags integration -count=1 -run '^TestRealMongoStepProcesses' ./saga/

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
	stepProcessRoleEnv      = "ROOST_SAGA_STEP_PROCESS_ROLE"
	stepProcessResultPrefix = "STEP_PROCESS_RESULT "
	stepProcessInHandler    = "STEP_PROCESS_IN_HANDLER"
)

// stepProcessAction 是一次（或一组并发的）Handle：Attempts 里的每个尝试各跑一次，Result 决定 handler 的结论。
type stepProcessAction struct {
	Database    string
	Owner       string
	LeaseMillis int64
	Operation   string
	Incarnation uint32
	Attempts    []uint32
	// CreatedAt / DeadlineAt 固定下来，两个进程构造出同一个命令（摘要相同）。
	CreatedAt, DeadlineAt int64
	// Result：success / hang（拿到租约、进了 handler 之后停住：子进程等调用方杀掉，本进程等 release）。
	Result string
	// RetryInFlight：遇到另一次尝试持有有效租约时重试到这个时限（毫秒）。
	RetryInFlight int64
}

type stepProcessResult struct {
	CommandID string
	Completed string // 返回的 completion 属于哪次尝试
	Success   bool
	Duplicate bool
	Kind      string // ok / in_flight / superseded / fenced / expired / error
	Err       string
}

func (a stepProcessAction) command(attempt uint32) Command {
	return Command{
		ID: commandID(a.Operation, a.Incarnation, attempt), IdempotencyKey: a.Operation, SagaID: "gift-1", SagaType: "gift",
		DefinitionVersion: 1, BusinessKey: "g-1", StepName: "deliver", Phase: PhaseForward, Attempt: attempt,
		Topic: "gift.deliver", Payload: []byte("state"),
		CreatedAt: time.Unix(0, a.CreatedAt).UTC(), DeadlineAt: time.Unix(0, a.DeadlineAt).UTC(),
	}
}

func stepProcessErrorKind(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, errOperationAttemptInFlight):
		return "in_flight"
	case errors.Is(err, errAttemptSuperseded):
		return "superseded"
	case errors.Is(err, errAttemptFenced):
		return "fenced"
	case errors.Is(err, ErrCommandExpired):
		return "expired"
	default:
		return "error"
	}
}

// runStepProcessAction 是两个进程共用的执行体。hang 时 entered 收到通知后 handler 阻塞到 release 关闭
// （子进程里 release 为 nil，永远阻塞，等调用方杀进程）。
func runStepProcessAction(ctx context.Context, client fmongo.IMongo, action stepProcessAction, entered func(), release <-chan struct{}) []stepProcessResult {
	inbox, err := NewMongoCommandInbox(client, action.Database, "steps", CommandInboxOptions{
		Owner: action.Owner, LeaseDuration: time.Duration(action.LeaseMillis) * time.Millisecond,
	})
	if err == nil {
		err = inbox.EnsureInfrastructure(ctx)
	}
	if err != nil {
		return []stepProcessResult{{Kind: "error", Err: err.Error()}}
	}
	business := client.Database(action.Database).Collection("business")
	handler := func(txCtx context.Context, c Command) (Completion, error) {
		if action.Result == "hang" {
			// 停在事务的第一条命令之前：被杀的进程不在服务端留下打开的事务（它会占着库直到
			// transactionLifetimeLimitSeconds，清理时 Drop 等不到）。租约与状态文档和停在业务写之后相同。
			entered()
			<-release
		}
		if _, err := business.InsertOne(txCtx, bson.M{"_id": c.ID, "operation": c.IdempotencyKey, "owner": action.Owner}); err != nil {
			return Completion{}, err
		}
		return Completion{Success: true, Data: []byte("by " + action.Owner)}, nil
	}
	results := make([]stepProcessResult, len(action.Attempts))
	var wg sync.WaitGroup
	for i, attempt := range action.Attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			command := action.command(attempt)
			giveUp := time.Now().Add(time.Duration(action.RetryInFlight) * time.Millisecond)
			for {
				completion, duplicate, err := inbox.Handle(ctx, command, handler)
				results[i] = stepProcessResult{
					CommandID: command.ID, Completed: completion.CommandID, Success: completion.Success,
					Duplicate: duplicate, Kind: stepProcessErrorKind(err),
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

// TestStepProcessRole 是另一进程的入口：只在 ROOST_SAGA_STEP_PROCESS_ROLE 设置时运行，结果按行打印到 stdout。
func TestStepProcessRole(t *testing.T) {
	raw := os.Getenv(stepProcessRoleEnv)
	if raw == "" {
		t.Skip(stepProcessRoleEnv + " is not set; this is the child-process entry of TestRealMongoStepProcesses*")
	}
	var action stepProcessAction
	if err := json.Unmarshal([]byte(raw), &action); err != nil {
		t.Fatal(err)
	}
	client, err := driver.NewClient(fmongo.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")), driver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())
	entered := func() { fmt.Println(stepProcessInHandler) }
	for _, result := range runStepProcessAction(context.Background(), client, action, entered, nil) {
		line, _ := json.Marshal(result)
		fmt.Println(stepProcessResultPrefix + string(line))
	}
}

type stepProcessHarness struct {
	t        *testing.T
	client   fmongo.IMongo
	database string
	created  int64
	deadline int64
}

func newStepProcessHarness(t *testing.T) *stepProcessHarness {
	client, database := realSagadirMongo(t)
	now := time.Now().UTC()
	return &stepProcessHarness{t: t, client: client, database: database, created: now.UnixNano(), deadline: now.Add(time.Hour).UnixNano()}
}

func (h *stepProcessHarness) action(owner, operation string, result string, attempts ...uint32) stepProcessAction {
	return stepProcessAction{
		Database: h.database, Owner: owner, LeaseMillis: time.Minute.Milliseconds(), Operation: operation,
		Attempts: attempts, CreatedAt: h.created, DeadlineAt: h.deadline, Result: result,
	}
}

// child 在另一个进程（当前测试二进制）里执行 action，返回它打印的结果。
func (h *stepProcessHarness) child(action stepProcessAction) []stepProcessResult {
	h.t.Helper()
	out, err := h.childCommand(action).Output()
	if err != nil {
		h.t.Fatalf("child process %+v: %v\n%s", action, err, out)
	}
	var results []stepProcessResult
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, stepProcessResultPrefix); ok {
			var result stepProcessResult
			if err := json.Unmarshal([]byte(rest), &result); err != nil {
				h.t.Fatal(err)
			}
			results = append(results, result)
		}
	}
	if len(results) == 0 {
		h.t.Fatalf("child process printed no result:\n%s", out)
	}
	return results
}

func (h *stepProcessHarness) childCommand(action stepProcessAction) *exec.Cmd {
	raw, err := json.Marshal(action)
	if err != nil {
		h.t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run", "^TestStepProcessRole$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), stepProcessRoleEnv+"="+string(raw))
	return cmd
}

// local 在本进程里执行 action。
func (h *stepProcessHarness) local(action stepProcessAction) []stepProcessResult {
	return runStepProcessAction(context.Background(), h.client, action, func() {}, nil)
}

func (h *stepProcessHarness) one(results []stepProcessResult) stepProcessResult {
	h.t.Helper()
	if len(results) != 1 {
		h.t.Fatalf("got %d results, want 1: %+v", len(results), results)
	}
	return results[0]
}

func (h *stepProcessHarness) effects(operation string) int64 {
	h.t.Helper()
	count, err := h.client.Database(h.database).Collection("business").CountDocuments(context.Background(), bson.M{"operation": operation})
	if err != nil {
		h.t.Fatal(err)
	}
	return count
}

func wantStepExecuted(t *testing.T, who string, r stepProcessResult) {
	t.Helper()
	if r.Kind != "ok" || r.Duplicate || r.Completed != r.CommandID || !r.Success {
		t.Fatalf("%s %s: %+v, want it to execute", who, r.CommandID, r)
	}
}

func TestRealMongoStepProcessesFenceAttemptsInFlightAcrossProcesses(t *testing.T) {
	h := newStepProcessHarness(t)
	const lease = 3 * time.Second

	t.Run("process killed in its handler", func(t *testing.T) {
		op := "gift-1:1:2"
		action := h.action("child", op, "hang", 1)
		action.LeaseMillis = lease.Milliseconds()
		cmd := h.childCommand(action)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		lines := bufio.NewScanner(stdout)
		for lines.Scan() && lines.Text() != stepProcessInHandler {
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if r := h.one(h.local(h.action("local", op, "success", 2))); r.Kind != "in_flight" {
			t.Fatalf("attempt 2 within the killed process's lease: %+v, want in_flight", r)
		}
		next := h.action("local", op, "success", 2)
		next.RetryInFlight = (2 * lease).Milliseconds()
		wantStepExecuted(t, "local", h.one(h.local(next)))
		// 被杀尝试的重投（截止未到）：它已被接替，不执行。
		if r := h.one(h.child(h.action("child", op, "success", 1))); r.Kind != "superseded" {
			t.Fatalf("redelivery of the killed attempt: %+v, want superseded", r)
		}
		if n := h.effects(op); n != 1 {
			t.Fatalf("operation %s took effect %d times, want 1", op, n)
		}
	})

	t.Run("process holding its transaction", func(t *testing.T) {
		op := "gift-1:1:3"
		action := h.action("local", op, "hang", 1)
		action.LeaseMillis = lease.Milliseconds()
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		done := make(chan stepProcessResult, 1)
		go func() {
			done <- runStepProcessAction(context.Background(), h.client, action, func() { once.Do(func() { close(entered) }) }, release)[0]
		}()
		<-entered
		if r := h.one(h.child(h.action("child", op, "success", 2))); r.Kind != "in_flight" {
			t.Fatalf("attempt 2 within the local lease: %+v, want in_flight", r)
		}
		next := h.action("child", op, "success", 2)
		next.RetryInFlight = (2 * lease).Milliseconds()
		wantStepExecuted(t, "child", h.one(h.child(next)))
		close(release)
		if r := <-done; r.Kind != "fenced" {
			t.Fatalf("attempt 1 committing after the child process took over: %+v, want fenced", r)
		}
		if r := h.one(h.local(h.action("local", op, "success", 3))); r.Kind != "ok" || !r.Duplicate || r.Completed != commandID(op, 0, 2) {
			t.Fatalf("attempt 3: %+v, want the success of attempt 2 replayed", r)
		}
		if n := h.effects(op); n != 1 {
			t.Fatalf("operation %s took effect %d times, want 1", op, n)
		}
	})
}

func TestRealMongoStepProcessesConcurrentAttemptsTakeEffectOnce(t *testing.T) {
	h := newStepProcessHarness(t)
	for round := 0; round < 5; round++ {
		op := fmt.Sprintf("gift-1:2:%d", round)
		childAction := h.action("child", op, "success", 1, 3, 5, 7)
		localAction := h.action("local", op, "success", 2, 4, 6, 8)
		childAction.RetryInFlight, localAction.RetryInFlight = 20_000, 20_000
		var childResults []stepProcessResult
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); childResults = h.child(childAction) }()
		localResults := h.local(localAction)
		wg.Wait()
		var executed []string
		all := append(childResults, localResults...)
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
