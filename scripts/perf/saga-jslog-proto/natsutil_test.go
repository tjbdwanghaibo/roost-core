package jslogproto

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// 原型测试只用私有集群：SAGAJS_URL 指向 nats-cluster.sh 起的三节点（逗号分隔），没设置就跳过。
// 不读共享环境的变量，不连 4222。
func clusterURL(t testing.TB) string {
	t.Helper()
	url := os.Getenv("SAGAJS_URL")
	if url == "" {
		t.Skip("SAGAJS_URL 未设置：先用 scripts/perf/saga-jslog-proto/nats-cluster.sh start 起私有集群")
	}
	return url
}

var testSeq atomic.Int64

// uniqueName 返回本次运行唯一的流名前缀，测试结束删流。
func uniqueName(prefix string) string {
	return fmt.Sprintf("%s%d%d", prefix, time.Now().UnixNano()%1_000_000_000, testSeq.Add(1))
}

func connect(t testing.TB) (*nats.Conn, jetstream.JetStream) {
	t.Helper()
	nc, err := nats.Connect(clusterURL(t), nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	return nc, js
}

func createStream(t testing.TB, js jetstream.JetStream, cfg jetstream.StreamConfig) jetstream.Stream {
	t.Helper()
	if cfg.Replicas == 0 {
		cfg.Replicas = 3
	}
	cfg.Storage = jetstream.FileStorage
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream, err := js.CreateStream(ctx, cfg)
	if err != nil {
		t.Fatalf("create stream %s: %v", cfg.Name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = js.DeleteStream(ctx, cfg.Name)
	})
	return stream
}

// apiErrorCode 取 JetStream API 错误码，非 API 错误返回 0。
func apiErrorCode(err error) jetstream.ErrorCode {
	var apiErr *jetstream.APIError
	if err != nil && errors.As(err, &apiErr) {
		return apiErr.ErrorCode
	}
	return 0
}

func isWrongLastSeq(err error) bool {
	code := apiErrorCode(err)
	return code == jetstream.JSErrCodeStreamWrongLastSequence || code == jetstream.JSErrCodeStreamWrongLastSequenceConstant
}

func lastSeqOf(t testing.TB, stream jetstream.Stream, subject string) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg, err := stream.GetLastMsgForSubject(ctx, subject)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return 0
		}
		t.Fatalf("last msg for %s: %v", subject, err)
	}
	return msg.Sequence
}
