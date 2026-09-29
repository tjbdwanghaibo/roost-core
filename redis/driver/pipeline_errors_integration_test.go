//go:build integration

package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	redis "github.com/tjbdwanghaibo/roost-core/redis"
)

func pipelineIntegrationClient(t *testing.T, cluster bool) redis.IRedis {
	t.Helper()
	cfg := redis.DefaultConfig(os.Getenv("ROOST_REDIS_TEST_ADDR"))
	if cluster {
		addresses := os.Getenv("ROOST_REVIEW_CLUSTER")
		if addresses == "" {
			t.Skip("set ROOST_REVIEW_CLUSTER")
		}
		cfg.ClusterAddrs = strings.Split(addresses, ",")
	} else if os.Getenv("ROOST_REDIS_TEST_ADDR") == "" {
		t.Skip("set ROOST_REDIS_TEST_ADDR")
	}
	client, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestIntegrationPipelineDoesNotHideWriteErrors(t *testing.T) {
	for _, cluster := range []bool{false, true} {
		t.Run(fmt.Sprintf("cluster_%v", cluster), func(t *testing.T) {
			client := pipelineIntegrationClient(t, cluster)
			for _, method := range []string{"hset", "rpush", "zadd"} {
				for _, missingPosition := range []string{"first", "middle", "last"} {
					t.Run(method+"/"+missingPosition, func(t *testing.T) {
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						prefix := fmt.Sprintf("{pipeline-errors-%d}", time.Now().UnixNano())
						key, absent := prefix+":string", prefix+":absent"
						t.Cleanup(func() { _, _ = client.Del(context.Background(), key) })
						if err := client.Set(ctx, key, "original", time.Minute); err != nil {
							t.Fatal(err)
						}
						pipe := client.Pipeline()
						defer pipe.Discard()
						var missing *redis.FutureBytes
						if missingPosition == "first" {
							missing = pipe.Get(ctx, absent)
						}
						good := pipe.Get(ctx, key)
						if missingPosition == "middle" {
							missing = pipe.Get(ctx, absent)
						}
						badWrite := func(p redis.IPipeline) {
							switch method {
							case "hset":
								p.HSet(ctx, key, "field", "value")
							case "rpush":
								p.RPush(ctx, key, "value")
							case "zadd":
								p.ZAdd(ctx, key, redis.Z{Score: 1, Member: "value"})
							}
						}
						badWrite(pipe)
						if missingPosition == "last" {
							missing = pipe.Get(ctx, absent)
						}
						if err := pipe.Exec(ctx); err == nil || !strings.Contains(err.Error(), "WRONGTYPE") {
							t.Fatalf("failed %s hidden: %v", method, err)
						}
						if _, err := missing.Result(); !errors.Is(err, redis.ErrNil) {
							t.Fatalf("missing: %v", err)
						}
						if value, err := good.Result(); err != nil || string(value) != "original" {
							t.Fatalf("good future: %q %v", value, err)
						}
						if value, err := client.Get(ctx, key); err != nil || string(value) != "original" {
							t.Fatalf("failed write modified key: %q %v", value, err)
						}
						control := client.Pipeline()
						badWrite(control)
						if err := control.Exec(ctx); err == nil || !strings.Contains(err.Error(), "WRONGTYPE") {
							t.Fatalf("control: %v", err)
						}
					})
				}
			}
		})
	}
}

func TestIntegrationPipelineFuturesAndLifecycle(t *testing.T) {
	for _, cluster := range []bool{false, true} {
		t.Run(fmt.Sprintf("cluster_%v", cluster), func(t *testing.T) {
			client := pipelineIntegrationClient(t, cluster)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			prefix := fmt.Sprintf("{pipeline-futures-%d}", time.Now().UnixNano())
			counter, hash, str := prefix+":counter", prefix+":hash", prefix+":string"
			t.Cleanup(func() {
				for _, key := range []string{counter, hash, str} {
					_, _ = client.Del(context.Background(), key)
				}
			})
			if err := client.HSet(ctx, hash, "field", "value"); err != nil {
				t.Fatal(err)
			}
			if err := client.Set(ctx, str, "original", time.Minute); err != nil {
				t.Fatal(err)
			}
			pipe := client.Pipeline()
			defer pipe.Discard()
			missing := pipe.Get(ctx, prefix+":absent")
			count := pipe.Incr(ctx, counter)
			values := pipe.HGetAll(ctx, hash)
			bad := pipe.HGetAll(ctx, str)
			if err := pipe.Exec(ctx); err == nil || !strings.Contains(err.Error(), "WRONGTYPE") {
				t.Fatalf("map error hidden: %v", err)
			}
			if _, err := missing.Result(); !errors.Is(err, redis.ErrNil) {
				t.Fatal(err)
			}
			if value, err := count.Result(); err != nil || value != 1 {
				t.Fatalf("completed INCR: %d %v", value, err)
			}
			if value, err := values.Result(); err != nil || value["field"] != "value" {
				t.Fatalf("map: %v %v", value, err)
			}
			if _, err := bad.Result(); err == nil || !strings.Contains(err.Error(), "WRONGTYPE") {
				t.Fatalf("bad map: %v", err)
			}
			// Partial execution is not rolled back or replayed by the wrapper.
			next := pipe.Get(ctx, counter)
			if err := pipe.Exec(ctx); err != nil {
				t.Fatal(err)
			}
			if value, err := next.Result(); err != nil || string(value) != "1" {
				t.Fatalf("reuse or replay: %q %v", value, err)
			}
			if value, err := count.Result(); err != nil || value != 1 {
				t.Fatalf("old future changed: %d %v", value, err)
			}
			pipe.Set(ctx, counter, "discarded", time.Minute)
			pipe.Discard()
			if err := pipe.Exec(ctx); err != nil {
				t.Fatal(err)
			}
			if value, err := client.Get(ctx, counter); err != nil || string(value) != "1" {
				t.Fatalf("discarded write applied: %q %v", value, err)
			}
			pipe.Del(ctx, counter)
			gone := pipe.Get(ctx, counter)
			if err := pipe.Exec(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := gone.Result(); !errors.Is(err, redis.ErrNil) {
				t.Fatal(err)
			}
			cancelled, stop := context.WithCancel(ctx)
			stop()
			pipe.Set(cancelled, counter, "cancelled", time.Minute)
			if err := pipe.Exec(cancelled); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled: %v", err)
			}
			if _, err := client.Get(ctx, counter); !errors.Is(err, redis.ErrNil) {
				t.Fatalf("cancelled write applied: %v", err)
			}
		})
	}
}

type pipelineReplyLossHook struct{ err error }

func (h pipelineReplyLossHook) DialHook(next goredis.DialHook) goredis.DialHook          { return next }
func (h pipelineReplyLossHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook { return next }
func (h pipelineReplyLossHook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, commands []goredis.Cmder) error {
		if err := next(ctx, commands); err != nil {
			return err
		}
		return h.err
	}
}

// A post-execution hook models an unknown reply after a real backend write;
// it does not claim to reproduce physical network loss or failover.
func TestIntegrationPipelinePreservesPostWriteUnknownError(t *testing.T) {
	for _, cluster := range []bool{false, true} {
		t.Run(fmt.Sprintf("cluster_%v", cluster), func(t *testing.T) {
			client := pipelineIntegrationClient(t, cluster)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			key := fmt.Sprintf("{pipeline-unknown-%d}", time.Now().UnixNano())
			t.Cleanup(func() { _, _ = client.Del(context.Background(), key) })
			// Warm the key's actual connection before installing the hook;
			// go-redis also pipelines connection initialization commands.
			if err := client.Set(ctx, key, "before", time.Minute); err != nil {
				t.Fatal(err)
			}
			raw := client.(*Client).Raw()
			unknown := errors.New("reply lost after write")
			raw.AddHook(pipelineReplyLossHook{err: unknown})
			pipe := client.Pipeline()
			pipe.Set(ctx, key, "applied", time.Minute)
			future := pipe.Get(ctx, key)
			if err := pipe.Exec(ctx); !errors.Is(err, unknown) {
				t.Fatalf("unknown error lost: %v", err)
			}
			if value, err := client.Get(ctx, key); err != nil || string(value) != "applied" {
				t.Fatalf("actual write: %q %v", value, err)
			}
			if value, err := future.Result(); err != nil || string(value) != "applied" {
				t.Fatalf("completed future: %q %v", value, err)
			}
		})
	}
}
