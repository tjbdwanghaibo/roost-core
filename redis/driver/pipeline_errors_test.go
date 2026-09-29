package driver

import (
	"context"
	"errors"
	"fmt"
	"testing"

	goredis "github.com/redis/go-redis/v9"
	redis "github.com/tjbdwanghaibo/roost-core/redis"
)

// Fixed command results isolate wrapper error selection from network routing.
// Real-backend tests separately execute the buffered Redis commands.
type pipelineResultsStub struct {
	goredis.Pipeliner
	commands []goredis.Cmder
	execErr  error
}

func (s *pipelineResultsStub) Exec(context.Context) ([]goredis.Cmder, error) {
	return s.commands, s.execErr
}

func TestPipelineExecChecksEveryCommandError(t *testing.T) {
	commandErr := errors.New("command failed")
	wireErr := fmt.Errorf("reply lost: %w", context.DeadlineExceeded)
	for _, tc := range []struct {
		name      string
		aggregate error
		command   error
		want      error
	}{
		{"missing_before_failure", goredis.Nil, commandErr, commandErr},
		{"nil_aggregate_with_failure", nil, commandErr, commandErr},
		{"only_missing", goredis.Nil, goredis.Nil, nil},
		{"all_success", nil, nil, nil},
		{"transport_error_preserved", wireErr, commandErr, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &pipelineResultsStub{execErr: tc.aggregate, commands: []goredis.Cmder{
				goredis.NewStringResult("", goredis.Nil),
				goredis.NewStatusResult("", tc.command),
			}}
			err := newPipeline(stub).Exec(context.Background())
			if !errors.Is(err, tc.want) {
				t.Fatalf("Exec=%v, want %v", err, tc.want)
			}
		})
	}
}

func TestPipelineExecPopulatesAllFuturesOnFailure(t *testing.T) {
	backendErr := errors.New("write rejected")
	str := goredis.NewStringResult("value", nil)
	missing := goredis.NewStringResult("", goredis.Nil)
	bad := goredis.NewStringResult("", backendErr)
	integer := goredis.NewIntResult(42, nil)
	hash := goredis.NewMapStringStringResult(map[string]string{"field": "value"}, nil)
	stub := &pipelineResultsStub{execErr: goredis.Nil, commands: []goredis.Cmder{missing, str, bad, integer, hash}}
	pipe := newPipeline(stub)
	// Use the same tracked command/future pairs Get/Incr/HGetAll establish.
	goodFuture, missingFuture, badFuture := &redis.FutureBytes{}, &redis.FutureBytes{}, &redis.FutureBytes{}
	intFuture, mapFuture := &redis.FutureInt64{}, &redis.FutureStringMap{}
	pipe.bytesFutures = []*pipelineBytesCmd{{cmd: str, future: goodFuture}, {cmd: missing, future: missingFuture}, {cmd: bad, future: badFuture}}
	pipe.int64Futures = []*pipelineInt64Cmd{{cmd: integer, future: intFuture}}
	pipe.stringMapFutures = []*pipelineStringMapCmd{{cmd: hash, future: mapFuture}}
	if err := pipe.Exec(context.Background()); !errors.Is(err, backendErr) {
		t.Fatalf("Exec: %v", err)
	}
	if value, err := goodFuture.Result(); err != nil || string(value) != "value" {
		t.Fatalf("good: %q %v", value, err)
	}
	if _, err := missingFuture.Result(); !errors.Is(err, redis.ErrNil) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := badFuture.Result(); !errors.Is(err, backendErr) {
		t.Fatalf("bad: %v", err)
	}
	if value, err := intFuture.Result(); err != nil || value != 42 {
		t.Fatalf("int: %d %v", value, err)
	}
	if value, err := mapFuture.Result(); err != nil || value["field"] != "value" {
		t.Fatalf("map: %v %v", value, err)
	}
}
