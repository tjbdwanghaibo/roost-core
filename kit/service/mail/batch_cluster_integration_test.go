//go:build integration

package mail

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	redis "github.com/tjbdwanghaibo/roost-core/redis"
	"github.com/tjbdwanghaibo/roost-core/redis/driver"
)

// RR-20260929-33: use the capability actually published by Init/Provide,
// rather than replacing the Mod's private service after registration.
func TestIntegrationMailModClusterPagination(t *testing.T) {
	addresses := os.Getenv("ROOST_REVIEW_CLUSTER")
	if addresses == "" {
		t.Skip("set ROOST_REVIEW_CLUSTER")
	}
	for _, tagged := range []bool{false, true} {
		t.Run(fmt.Sprintf("tagged_%v", tagged), func(t *testing.T) {
			prefix := fmt.Sprintf("mail-mod-%d", time.Now().UnixNano())
			if tagged {
				prefix = "{" + prefix + "}"
			}
			cfg := viper.New()
			cfg.Set("redis.cluster_addrs", addresses)
			cfg.Set("mail.key_prefix", prefix)
			cfg.Set("mail.send_ttl", time.Hour)
			mod := NewMod(nil, nil)
			if err := mod.Init(cfg); err != nil {
				t.Fatal(err)
			}
			rcfg := redis.DefaultConfig("")
			rcfg.ClusterAddrs = strings.Split(addresses, ",")
			client, err := driver.NewClient(rcfg)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			registry := app.NewRegistry(cfg)
			if err := mods.RegisterAll(registry, mods.Capability{Name: mods.ModRedis, Value: client}); err != nil {
				t.Fatal(err)
			}
			if err := mod.Provide(registry); err != nil {
				t.Fatal(err)
			}
			capability, ok := app.Lookup[Mail](registry, CapabilityName)
			if !ok {
				t.Fatal("Mail capability missing")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			mails := []Envelope{}
			defer func() {
				for _, env := range mails {
					_, _ = client.Del(context.Background(), prefix+":env:"+env.ID)
					_, _ = client.Del(context.Background(), prefix+":send:"+env.SendRequestID)
				}
				_, _ = client.Del(context.Background(), prefix+":box:1")
			}()
			// Verify the untagged fixture really spans different owners.
			raw := goredis.NewClusterClient(&goredis.ClusterOptions{Addrs: rcfg.ClusterAddrs})
			defer raw.Close()
			slots, err := raw.ClusterSlots(ctx).Result()
			if err != nil {
				t.Fatal(err)
			}
			owners := map[string]bool{}
			for i := 0; i < 16; i++ {
				env, err := capability.Send(ctx, SendRequest{Audience: AudienceDirect, Recipients: []int64{1}, Subject: "review", Attachment: []byte{0, 255}, ExpiresInSeconds: 3600, RequestID: fmt.Sprintf("request-%d", i)})
				if err != nil {
					t.Fatal(err)
				}
				mails = append(mails, env)
				slot, err := raw.ClusterKeySlot(ctx, prefix+":env:"+env.ID).Result()
				if err != nil {
					t.Fatal(err)
				}
				for _, partition := range slots {
					if slot >= int64(partition.Start) && slot <= int64(partition.End) {
						owners[partition.Nodes[0].Addr] = true
					}
				}
			}
			if !tagged && len(owners) < 2 {
				t.Fatal("fixture did not span Cluster owners")
			}
			if tagged && len(owners) != 1 {
				t.Fatal("tagged control not on one owner")
			}
			seen := map[string]bool{}
			cursor := ""
			for pageNo := 0; pageNo < 2; pageNo++ {
				page, err := capability.List(ctx, 1, cursor, 8)
				if err != nil || len(page.Items) != 8 || page.Unread != 16 {
					t.Fatalf("page %d: %+v %v", pageNo, page, err)
				}
				for _, item := range page.Items {
					if seen[item.Envelope.ID] {
						t.Fatal("duplicate page item")
					}
					seen[item.Envelope.ID] = true
				}
				cursor = page.NextCursor
			}
			if len(seen) != 16 || cursor != "" {
				t.Fatalf("page completion: %d %q", len(seen), cursor)
			}
			claim, err := capability.ReserveClaim(ctx, 1, mails[0].ID, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := capability.CancelClaim(ctx, 1, mails[0].ID, claim.Token, claim.Attempts); err != nil {
				t.Fatal(err)
			}
			t.Logf("mail_count=16 owners=%d tagged=%v", len(owners), tagged)
		})
	}
}
