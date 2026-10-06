package driver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/tjbdwanghaibo/roost-core/metrics"
)

// O-M6-5（维护者第十二轮决定）：启动期建索引遇到副本集选举，做有界重试。次数与间隔都有上限，
// 用完失败并点名是选举没有结束；不是选举的错误不重试。
func TestIndexCreationRetriesThroughAnElectionWithinBounds(t *testing.T) {
	stepDown := mongo.CommandError{Code: 11602, Name: "InterruptedDueToReplStateChange", Message: "operation was interrupted"}
	notPrimary := mongo.CommandError{Code: 10107, Name: "NotWritablePrimary", Message: "not primary"}
	policy := electionRetry{attempts: 4, interval: time.Millisecond}

	old := metrics.DefaultRegistry()
	registry := metrics.NewRegistry()
	metrics.SetDefaultRegistry(registry)
	t.Cleanup(func() { metrics.SetDefaultRegistry(old) })

	t.Run("an election that settles", func(t *testing.T) {
		calls := 0
		err := retryDuringElection(context.Background(), policy, func() error {
			calls++
			switch calls {
			case 1:
				return stepDown
			case 2:
				return notPrimary
			}
			return nil
		})
		if err != nil || calls != 3 {
			t.Fatalf("err=%v after %d calls, want success on the 3rd", err, calls)
		}
		if got := registry.Snapshot(); len(got) != 1 || got[0].Name != electionRetryMetric || got[0].Value != 2 {
			t.Fatalf("election retries counted %+v, want %s = 2", got, electionRetryMetric)
		}
	})

	t.Run("an election that does not settle", func(t *testing.T) {
		calls := 0
		err := retryDuringElection(context.Background(), policy, func() error { calls++; return stepDown })
		if calls != policy.attempts {
			t.Fatalf("made %d attempts, want exactly the bound %d", calls, policy.attempts)
		}
		if err == nil || !strings.Contains(err.Error(), "election did not settle after 4 attempts") || !errors.As(err, new(mongo.CommandError)) {
			t.Fatalf("err = %v, want the bound named and the server error kept in the chain", err)
		}
	})

	t.Run("the deadline comes first", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		err := retryDuringElection(ctx, electionRetry{attempts: 100, interval: time.Hour}, func() error {
			calls++
			cancel()
			return stepDown
		})
		if calls != 1 || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "election did not settle before the deadline") {
			t.Fatalf("calls=%d err=%v, want one attempt and the deadline named", calls, err)
		}
	})

	t.Run("other errors are not retried", func(t *testing.T) {
		duplicate := mongo.CommandError{Code: 85, Name: "IndexOptionsConflict"}
		calls := 0
		err := retryDuringElection(context.Background(), policy, func() error { calls++; return duplicate })
		if calls != 1 || !errors.As(err, new(mongo.CommandError)) || strings.Contains(err.Error(), "election") {
			t.Fatalf("calls=%d err=%v, want the error returned as is after one attempt", calls, err)
		}
	})
}
