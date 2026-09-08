package redis

import (
	"context"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

func TestRedisIntegerParsesWAITAOFReplyValues(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  int64
	}{
		{name: "int64", value: int64(1), want: 1},
		{name: "int", value: 2, want: 2},
		{name: "string", value: "3", want: 3},
		{name: "bytes", value: []byte("4"), want: 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := redisInteger(test.value)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("redisInteger(%v) = %d, want %d", test.value, got, test.want)
			}
		})
	}
}

func TestRedisIntegerRejectsInvalidWAITAOFReplyValue(t *testing.T) {
	if _, err := redisInteger([]any{1}); err == nil {
		t.Fatal("redisInteger accepted an invalid reply type")
	}
}

func TestEvalDurableRejectsClusterTopologyBeforeIO(t *testing.T) {
	cluster := goredis.NewClusterClient(&goredis.ClusterOptions{Addrs: []string{"127.0.0.1:1"}})
	defer func() { _ = cluster.Close() }()
	client := &redisClient{rdb: cluster}

	_, _, _, err := client.EvalDurable(context.Background(), "return 1", []string{"{probe}:key"}, 1, 0, time.Second)
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("EvalDurable cluster error = %v", err)
	}
}
