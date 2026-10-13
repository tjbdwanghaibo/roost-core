package saga

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func expectErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to contain %q", err, want)
	}
}

// NewEngine 拒绝租约算术不成立的配置；每条规则按它点名的字段断言，运维看到的错误能指向具体旋钮。
func TestNewEngineRefusesEachUnsafeOption(t *testing.T) {
	publisher := PublishFunc(func(context.Context, Command) error { return nil })
	cases := []struct {
		name   string
		mutate func(*Options)
		want   string
	}{
		{"store timeout not below lease", func(o *Options) { o.StoreTimeout = o.LeaseDuration }, "StoreTimeout (15s) must be < LeaseDuration (15s)"},
		{"owner too long", func(o *Options) { o.Owner = strings.Repeat("o", 257) }, "Owner length 257 exceeds 256"},
		{"too many coordinator workers", func(o *Options) { o.CoordinatorWorkers = 1025 }, "CoordinatorWorkers 1025 exceeds 1024"},
		{"too many publisher workers", func(o *Options) { o.PublisherWorkers = 1025 }, "PublisherWorkers 1025 exceeds 1024"},
		{"coordinator batch too large", func(o *Options) { o.CoordinatorBatch = 4097 }, "CoordinatorBatch 4097 exceeds 4096"},
		{"publisher batch too large", func(o *Options) { o.PublisherBatch = 4097 }, "PublisherBatch 4097 exceeds 4096"},
		{"payload cap too large", func(o *Options) { o.MaxPayloadBytes = 4<<20 + 1 }, "MaxPayloadBytes 4194305 exceeds 4194304"},
		{"lease not above publish timeout", func(o *Options) { o.PublishTimeout = o.LeaseDuration }, "LeaseDuration (15s) must be > PublishTimeout (15s)"},
		{"coordinator budget exhausted", func(o *Options) { o.CoordinatorBatch = 5 }, "coordinator budget exhausted"},
		{"publisher budget exhausted by publish timeout", func(o *Options) { o.PublisherBatch = 5 }, "publisher budget exhausted: (LeaseDuration-StoreTimeout)/PublisherBatch = "},
		{"publisher budget exhausted by store timeout", func(o *Options) {
			o.LeaseDuration, o.StoreTimeout, o.PublishTimeout, o.PublisherBatch, o.CoordinatorBatch = 10*time.Second, 3*time.Second, 3*time.Second, 2, 1
		}, "publisher budget exhausted: (LeaseDuration-StoreTimeout)/PublisherBatch - PublishTimeout = "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			options := DefaultOptions()
			tc.mutate(&options)
			_, err := NewEngine(newMemoryStore(), publisher, options)
			expectErr(t, err, tc.want)
		})
	}
	if _, err := NewEngine(newMemoryStore(), publisher, DefaultOptions()); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	if _, err := NewEngine(nil, publisher, DefaultOptions()); err == nil || !strings.Contains(err.Error(), "store and publisher are required") {
		t.Fatalf("nil store = %v", err)
	}
}

func validCommand() Command {
	now := time.Now()
	return Command{
		ID: "cmd-1", IdempotencyKey: "key-1", SagaID: "saga-1", SagaType: "rally", DefinitionVersion: 1,
		BusinessKey: "biz", Step: 0, StepName: "reserve", Phase: PhaseForward, Attempt: 1, Topic: "rally.reserve",
		DeadlineAt: now.Add(time.Minute), CreatedAt: now,
	}
}

// Command / Completion 是协调器与步骤执行方之间的线上格式；每个校验子句用单字段变异断言，丢掉一条子句恰好一例变红。
func TestCommandValidateRefusesEachBrokenField(t *testing.T) {
	if err := validCommand().Validate(); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	cases := map[string]func(*Command){
		"empty id":                func(c *Command) { c.ID = "" },
		"id too long":             func(c *Command) { c.ID = strings.Repeat("i", 193) },
		"empty idempotency key":   func(c *Command) { c.IdempotencyKey = "" },
		"saga id with wildcard":   func(c *Command) { c.SagaID = "saga.>" },
		"saga type padded":        func(c *Command) { c.SagaType = " rally" },
		"saga type empty":         func(c *Command) { c.SagaType = "" },
		"definition version zero": func(c *Command) { c.DefinitionVersion = 0 },
		"business key padded":     func(c *Command) { c.BusinessKey = "biz " },
		"business key too long":   func(c *Command) { c.BusinessKey = strings.Repeat("b", 513) },
		"negative step":           func(c *Command) { c.Step = -1 },
		"step name empty":         func(c *Command) { c.StepName = "" },
		"phase out of range":      func(c *Command) { c.Phase = PhaseCompensate + 1 },
		"attempt zero":            func(c *Command) { c.Attempt = 0 },
		"topic with empty token":  func(c *Command) { c.Topic = "rally..reserve" },
		"payload over 4MiB":       func(c *Command) { c.Payload = make([]byte, 4<<20+1) },
		"zero deadline":           func(c *Command) { c.DeadlineAt = time.Time{} },
		"zero created at":         func(c *Command) { c.CreatedAt = time.Time{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := validCommand()
			mutate(&c)
			if err := c.Validate(); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("Validate = %v, want ErrInvalidRecord", err)
			}
		})
	}
}

func TestCompletionValidateRefusesEachBrokenField(t *testing.T) {
	ok := Completion{CommandID: "cmd-1", IdempotencyKey: "key-1", SagaID: "saga-1", Success: true}
	if err := ok.Validate(); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	failed := Completion{CommandID: "cmd-1", IdempotencyKey: "key-1", SagaID: "saga-1", Success: false, Error: "boom", Retryable: true}
	if err := failed.Validate(); err != nil {
		t.Fatalf("failed baseline rejected: %v", err)
	}
	cases := map[string]Completion{
		"empty command id":         {IdempotencyKey: "k", SagaID: "s", Success: true},
		"empty idempotency key":    {CommandID: "c", SagaID: "s", Success: true},
		"saga id with space":       {CommandID: "c", IdempotencyKey: "k", SagaID: "s a", Success: true},
		"error too long":           {CommandID: "c", IdempotencyKey: "k", SagaID: "s", Error: strings.Repeat("e", 4097)},
		"data over 4MiB":           {CommandID: "c", IdempotencyKey: "k", SagaID: "s", Success: true, Data: make([]byte, 4<<20+1)},
		"success with error text":  {CommandID: "c", IdempotencyKey: "k", SagaID: "s", Success: true, Error: "x"},
		"success marked retryable": {CommandID: "c", IdempotencyKey: "k", SagaID: "s", Success: true, Retryable: true},
		"failure without error":    {CommandID: "c", IdempotencyKey: "k", SagaID: "s", Success: false, Error: "  "},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if err := c.Validate(); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("Validate = %v, want ErrInvalidRecord", err)
			}
		})
	}
}

// 启动 / 完成 effect 是留在 WAL 与 JetStream 上的线上载荷；解码器拒绝的每种形状都断言。
func TestNestEffectCodecsRefuseEachMalformedPayload(t *testing.T) {
	if _, err := DecodeStartEffect(nil); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("empty start payload = %v", err)
	}
	raw, err := json.Marshal(startEffectPayload{Version: WireVersion + 1, Start: StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeStartEffect(raw); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("future start version = %v", err)
	}
	for name, req := range map[string]StartRequest{
		"empty type":         {DefinitionVersion: 1, BusinessKey: "b"},
		"type too long":      {Type: strings.Repeat("t", 129), DefinitionVersion: 1, BusinessKey: "b"},
		"zero definition":    {Type: "rally", BusinessKey: "b"},
		"empty business key": {Type: "rally", DefinitionVersion: 1},
		"data over 4MiB":     {Type: "rally", DefinitionVersion: 1, BusinessKey: "b", Data: make([]byte, 4<<20+1)},
		"unsafe explicit id": {Type: "rally", DefinitionVersion: 1, BusinessKey: "b", ID: "a.b"},
	} {
		t.Run("start/"+name, func(t *testing.T) {
			if _, err := NewStartEffect(req); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("NewStartEffect = %v, want ErrInvalidRecord", err)
			}
			raw, err := json.Marshal(startEffectPayload{Version: WireVersion, Start: req})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeStartEffect(raw); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("DecodeStartEffect = %v, want ErrInvalidRecord", err)
			}
		})
	}
	if _, err := DecodeCompletionEffect(nil); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("empty completion payload = %v", err)
	}
	if _, err := DecodeCompletionEffect([]byte("{not json")); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("undecodable completion payload = %v", err)
	}
	raw, err = json.Marshal(completionEffectPayload{Version: WireVersion + 1, Completion: Completion{CommandID: "c", IdempotencyKey: "k", SagaID: "s", Success: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCompletionEffect(raw); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("future completion version = %v", err)
	}
	raw, err = json.Marshal(completionEffectPayload{Version: WireVersion, Completion: Completion{CommandID: "c", IdempotencyKey: "k", SagaID: "s", Success: false}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCompletionEffect(raw); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("completion failing validation = %v", err)
	}
}

// 收件箱用命令的 JSON 摘要区分同 ID 重投与身份冲突。json.Marshal 对 Command 可能失败（time.Time 年份超出 [0,9999]），
// 而 Validate 只要求时间非零：摘要必须报告这个错误（ErrInvalidRecord），不能退化成同一个 sha256(nil)，
// 否则同 ID 不同载荷的命令会被当成重投、拿到别人的完成结果。
// 历史：U-0107。
func farFutureCommand(id string, payload string) Command {
	now := time.Now()
	return Command{
		ID: id, IdempotencyKey: "op-" + id, SagaID: "saga", SagaType: "rally", DefinitionVersion: 1,
		BusinessKey: "r-" + id, Step: 0, StepName: "reserve", Phase: PhaseForward, Attempt: 1, Topic: "reserve",
		Payload: []byte(payload), CreatedAt: now,
		// 年份 10000：Validate 放行（非零），json.Marshal 拒绝。
		DeadlineAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestCommandDigestReportsUnmarshalableCommands(t *testing.T) {
	command := farFutureCommand("digest-1", "a")
	if err := command.Validate(); err != nil {
		t.Fatalf("fixture must pass Validate, got %v", err)
	}
	digest, err := commandDigest(command)
	if err == nil {
		t.Fatalf("commandDigest accepted an unmarshalable command, digest=%x", digest)
	}
	if !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("commandDigest error=%v, want ErrInvalidRecord", err)
	}
	other := farFutureCommand("digest-2", "b")
	if otherDigest, otherErr := commandDigest(other); otherErr == nil && string(otherDigest) == string(digest) {
		t.Fatal("two different unmarshalable commands shared one digest")
	}

	// 对照：正常命令的摘要稳定且随内容变化。
	normal := farFutureCommand("digest-3", "a")
	normal.DeadlineAt = time.Now().Add(time.Second)
	first, err := commandDigest(normal)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := commandDigest(normal)
	if string(first) != string(again) {
		t.Fatal("digest of the same command is not stable")
	}
	normal.Payload = []byte("b")
	changed, _ := commandDigest(normal)
	if string(first) == string(changed) {
		t.Fatal("payload change did not change the digest")
	}
}

func TestMongoCommandInboxRefusesCommandsWhoseIdentityCannotBeDigested(t *testing.T) {
	inbox, err := NewMongoCommandInbox(newInboxMongoFake(), "game", "")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	handler := func(context.Context, Command) (Completion, error) {
		calls.Add(1)
		return Completion{Success: true, Data: []byte("reserved")}, nil
	}
	first := farFutureCommand("reuse", "a")
	if _, duplicate, err := inbox.Handle(context.Background(), first, handler); !errors.Is(err, ErrInvalidRecord) || duplicate {
		t.Fatalf("first Handle = (duplicate=%v, err=%v), want ErrInvalidRecord", duplicate, err)
	}
	// 同一个 ID、不同载荷：修复前被当成重投递（duplicate=true，返回第一次的结果）。
	second := farFutureCommand("reuse", "b")
	if _, duplicate, err := inbox.Handle(context.Background(), second, handler); err == nil || duplicate {
		t.Fatalf("second Handle = (duplicate=%v, err=%v), want an error and no duplicate verdict", duplicate, err)
	}
	if calls.Load() != 0 {
		t.Fatalf("handler ran %d times for commands whose identity could not be established", calls.Load())
	}
	if _, found, err := inbox.Replay(context.Background(), first); found || !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("Replay = (found=%v, err=%v), want ErrInvalidRecord", found, err)
	}
}
