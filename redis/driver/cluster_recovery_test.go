package driver

import (
	"context"
	"errors"
	"net"
	"testing"

	goredis "github.com/redis/go-redis/v9"
)

func TestClusterRecoveryPreservesErrorsWithoutReplayingCommands(t *testing.T) {
	// 已关闭客户端让异步刷新立即退出，断言只关注 hook 的业务契约；真实选主由集成测试覆盖。
	client := goredis.NewClusterClient(&goredis.ClusterOptions{Addrs: []string{"127.0.0.1:1"}})
	_ = client.Close()
	h := clusterRecoveryHook{client: client}
	for _, failure := range []error{&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, context.Canceled, context.DeadlineExceeded, errors.New("business error")} {
		calls := 0
		err := h.ProcessHook(func(context.Context, goredis.Cmder) error { calls++; return failure })(context.Background(), goredis.NewCmd(context.Background(), "eval"))
		if err != failure || calls != 1 {
			t.Fatalf("command calls=%d error=%v want=%v", calls, err, failure)
		}
		calls = 0
		err = h.ProcessPipelineHook(func(context.Context, []goredis.Cmder) error { calls++; return failure })(context.Background(), nil)
		if err != failure || calls != 1 {
			t.Fatalf("pipeline calls=%d error=%v want=%v", calls, err, failure)
		}
	}
}
