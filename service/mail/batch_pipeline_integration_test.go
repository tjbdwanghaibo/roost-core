//go:build integration

package mail

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	redis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
)

// RR-20260929-33: real standalone and Cluster reads, including a missing
// command before WRONGTYPE (the pipeline aggregate may report only Nil).
func TestIntegrationEnvelopeBatchAcrossSlots(t *testing.T) {
	for _, cluster := range []bool{false, true} {
		t.Run(fmt.Sprintf("cluster_%v", cluster), func(t *testing.T) {
			cfg := redis.DefaultConfig(os.Getenv("REDIS_ADDR"))
			if cluster {
				addresses := os.Getenv("ROOST_REVIEW_CLUSTER")
				if addresses == "" {
					t.Skip("set ROOST_REVIEW_CLUSTER")
				}
				cfg.ClusterAddrs = strings.Split(addresses, ",")
			} else if os.Getenv("REDIS_ADDR") == "" {
				t.Skip("set REDIS_ADDR")
			}
			client, err := driver.NewClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			prefix := fmt.Sprintf("mail-batch-%d", time.Now().UnixNano())
			store, err := NewRedisEnvelopes(client, prefix, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			ids := []string{"live-1", "live-2", "empty", "bad-json", "wrong-type", "expired"}
			defer func() {
				for _, id := range ids {
					_, _ = client.Del(context.Background(), prefix+":env:"+id)
				}
			}()
			for _, id := range ids[:2] {
				env := testEnvelope(id, time.Hour)
				env.CreatedAtUnix = time.Now().Unix()
				env.ExpiresAtUnix = time.Now().Add(time.Hour).Unix()
				env.Attachment = []byte{0, 255}
				if created, err := store.Create(ctx, env); err != nil || !created {
					t.Fatalf("create: %v %v", created, err)
				}
			}
			got, err := store.GetMany(ctx, []string{"absent", "live-2", "live-1", "absent", "live-2"})
			if err != nil || len(got) != 2 || got["live-1"].ID != "live-1" || got["live-2"].ID != "live-2" {
				t.Fatalf("mapping: %v %v", got, err)
			}
			for _, shape := range []struct{ id, value string }{{"empty", ""}, {"bad-json", "{invalid"}} {
				t.Run(shape.id, func(t *testing.T) {
					if err := client.Set(ctx, prefix+":env:"+shape.id, shape.value, time.Minute); err != nil {
						t.Fatal(err)
					}
					if _, _, err := store.Get(ctx, shape.id); err == nil {
						t.Fatal("single read accepted malformed data")
					}
					if got, err := store.GetMany(ctx, []string{"absent", "live-1", shape.id}); err == nil || got != nil {
						t.Fatalf("batch silently succeeded: %v %v", got, err)
					}
				})
			}
			if err := client.HSet(ctx, prefix+":env:wrong-type", "field", "value"); err != nil {
				t.Fatal(err)
			}
			if got, err := store.GetMany(ctx, []string{"absent", "wrong-type"}); err == nil || !strings.Contains(err.Error(), "WRONGTYPE") || got != nil {
				t.Fatalf("wrong type hidden: %v %v", got, err)
			}
			// A deterministic Redis expiry, without relying on a scheduling sleep.
			if err := client.Set(ctx, prefix+":env:expired", "{}", time.Minute); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Eval(ctx, `return redis.call('PEXPIRE', KEYS[1], 0)`, []string{prefix + ":env:expired"}); err != nil {
				t.Fatal(err)
			}
			if got, err := store.GetMany(ctx, []string{"expired", "live-1"}); err != nil || len(got) != 1 {
				t.Fatalf("expired: %v %v", got, err)
			}
			cancelled, stop := context.WithCancel(ctx)
			stop()
			if got, err := store.GetMany(cancelled, []string{"live-1"}); !errors.Is(err, context.Canceled) || got != nil {
				t.Fatalf("cancelled: %v %v", got, err)
			}
		})
	}
}
