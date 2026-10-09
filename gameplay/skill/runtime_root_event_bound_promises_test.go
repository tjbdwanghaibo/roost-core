package skill

// 根事件表的结构性保证（RR-20261006-55 后续，维护者 2026-10-07 同意按推荐处理；后续二加排队任务上限）：RootEventLimit
// 必须大于同时被引用的根数上界 MaxActiveCasts + MaxOwnedSpawns + MaxStopPendingSpawns + MaxQueuedTasks
// （rootEventReferenceBound），违反时 NewRuntime 直接 panic（维护者 F08-H+2 ③ 决定保持）、RestoreRuntime 返回错误，并点出
// 全部选项与当前值。兜底分支只作防御，见 runtime_event_test.go 的 TestCollectHostEventsSkipsEventWhenEveryRootIsPinned；
// 合法配置下不触发见 runtime_queued_task_bound_promises_test.go。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

func newRuntimeRecover(host Host, options RuntimeOptions) (runtime *Runtime, recovered any) {
	defer func() { recovered = recover() }()
	return NewRuntime(host, options), nil
}

func TestNewRuntimeRejectsRootEventLimitNotAboveReferencedRootBound(t *testing.T) {
	cases := []struct {
		name    string
		options RuntimeOptions
		reject  bool
	}{
		{name: "defaults", options: RuntimeOptions{}},
		{name: "explicit small but above bound", options: RuntimeOptions{RootEventLimit: 5, MaxActiveCasts: 1, MaxOwnedSpawns: 1, MaxStopPendingSpawns: 1, MaxQueuedTasks: 1}},
		{name: "equal to bound", options: RuntimeOptions{RootEventLimit: 4, MaxActiveCasts: 1, MaxOwnedSpawns: 1, MaxStopPendingSpawns: 1, MaxQueuedTasks: 1}, reject: true},
		{name: "above the old bound but not counting the default queue", options: RuntimeOptions{RootEventLimit: 4, MaxActiveCasts: 1, MaxOwnedSpawns: 1, MaxStopPendingSpawns: 1}, reject: true},
		{name: "small limit with default casts", options: RuntimeOptions{RootEventLimit: 64}, reject: true},
		{name: "default limit with raised casts", options: RuntimeOptions{MaxActiveCasts: 8192}, reject: true},
		{name: "default limit with raised queue", options: RuntimeOptions{MaxQueuedTasks: 4096}, reject: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			runtime, recovered := newRuntimeRecover(nil, testCase.options)
			if !testCase.reject {
				if recovered != nil || runtime == nil {
					t.Fatalf("NewRuntime(%+v) panicked: %v", testCase.options, recovered)
				}
				return
			}
			if recovered == nil {
				t.Fatalf("NewRuntime accepted RootEventLimit %d with MaxActiveCasts %d / MaxOwnedSpawns %d / MaxStopPendingSpawns %d; a full root table where every root is referenced must be impossible under a legal configuration",
					testCase.options.RootEventLimit, testCase.options.MaxActiveCasts, testCase.options.MaxOwnedSpawns, testCase.options.MaxStopPendingSpawns)
			}
			err, ok := recovered.(error)
			if !ok || !errors.Is(err, ErrRuntimeLimitsInvalid) {
				t.Fatalf("panic value = %#v, want an error wrapping ErrRuntimeLimitsInvalid", recovered)
			}
			// 每个选项连同补齐默认值之后的当前值一起出现。
			effective := newRuntimeCore(nil, testCase.options).options
			for option, value := range map[string]int{"RootEventLimit": effective.RootEventLimit, "MaxActiveCasts": effective.MaxActiveCasts, "MaxOwnedSpawns": effective.MaxOwnedSpawns, "MaxStopPendingSpawns": effective.MaxStopPendingSpawns, "MaxQueuedTasks": effective.MaxQueuedTasks} {
				if want := option + " (" + strconv.Itoa(value) + ")"; !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestRestoreRuntimeRejectsCheckpointWhoseRootEventLimitIsNotAboveTheBound(t *testing.T) {
	program, environment := compileRuntimeFixture(t, "simple_damage.json")
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(checkpoint.Payload, &fields); err != nil {
		t.Fatal(err)
	}
	fields["root_event_limit"] = json.RawMessage("64")
	if checkpoint.Payload, err = json.Marshal(fields); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(checkpoint.Payload)
	checkpoint.Checksum = hex.EncodeToString(digest[:])
	resolver := ProgramResolverFunc(func(string, string) (*Program, error) { return program, nil })
	restored, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, resolver)
	if err == nil || restored != nil {
		t.Fatalf("RestoreRuntime accepted root_event_limit 64 with max_active_casts 4096 (err = %v)", err)
	}
	if !errors.Is(err, ErrRuntimeLimitsInvalid) || !errors.Is(err, ErrCheckpointCorrupt) {
		t.Fatalf("restore error = %v, want ErrRuntimeLimitsInvalid and ErrCheckpointCorrupt", err)
	}
	if !strings.Contains(err.Error(), "RootEventLimit") || !strings.Contains(err.Error(), "MaxActiveCasts") || !strings.Contains(err.Error(), "MaxQueuedTasks") {
		t.Fatalf("restore error %q does not name the options", err)
	}
}
