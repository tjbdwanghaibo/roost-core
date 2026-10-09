//go:build integration

package integration

import accountdomain "github.com/tjbdwanghaibo/roost-core/service/account"

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"

	"github.com/tjbdwanghaibo/roost-core/service/chat"
	"github.com/tjbdwanghaibo/roost-core/service/match"
)

// D-L3 第八轮（维护者决定）：match 票据时间、chat 展示给玩家的消息时间、account 的创建与登录时间是
// 业务时间，跟着 time.logic_offset 走；chat 保留期清理（空间回收）、会话 token 有效期是系统时间，
// 不受偏移影响。这里按部署的样子走：Mod 从配置 Init、对着真实 Redis Provide，偏移 +24h。
// 修前三个 Mod 都不给服务注入业务时钟，服务退回 time.Now，配了偏移也照真实时间打戳。
func TestOffsetMovesMatchChatAndAccountBusinessTimesButNotRetentionOrSessions(t *testing.T) {
	const offset = 24 * time.Hour
	c := client(t)
	cfg := modConfig(t, prefix(t, "dl3b"))
	cfg.Set("time.logic_offset", "24h")
	cfg.Set("chat.retention_age", "1h")
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModRedis, c); err != nil {
		t.Fatal(err)
	}
	for _, mod := range serviceMods(t) {
		if err := mod.Init(cfg); err != nil {
			t.Fatalf("%s Init: %v", mod.Name(), err)
		}
		if err := mod.Provide(registry); err != nil {
			t.Fatalf("%s Provide: %v", mod.Name(), err)
		}
		if err := mod.Start(); err != nil {
			t.Fatalf("%s Start: %v", mod.Name(), err)
		}
		t.Cleanup(mod.Stop)
	}
	ctx := context.Background()
	// shifted 判断一个 Unix 秒是不是“真实时间 + want”（容差 5s，覆盖 Redis 往返）。
	shifted := func(label string, got int64, want time.Duration) {
		t.Helper()
		expected := time.Now().Add(want).Unix()
		if diff := got - expected; diff < -5 || diff > 5 {
			t.Errorf("%s = %s, %s from the wall clock; want %s", label,
				time.Unix(got, 0).Format(time.RFC3339), (time.Duration(got-time.Now().Unix()) * time.Second).Round(time.Minute), want)
		}
	}

	t.Run("match ticket times are business time", func(t *testing.T) {
		matcher := app.MustLookup[match.Matchmaker](registry, mods.ModMatch)
		ticket, err := matcher.Enqueue(ctx, match.Queue{Mode: "ranked", GroupSize: 2, Partition: "dl3b"},
			match.Subject{Kind: "player", ID: 1, Score: 100}, "dl3b-r1")
		if err != nil {
			t.Fatal(err)
		}
		shifted("ticket.CreatedAtUnix", ticket.CreatedAtUnix, offset)
		shifted("ticket.ExpiresAtUnix", ticket.ExpiresAtUnix, offset+match.DefaultTicketTTL)
	})

	t.Run("chat display time is business time, retention is system time", func(t *testing.T) {
		service := app.MustLookup[*chat.Service](registry, chat.LocalCapabilityName)
		message, err := service.Publish(ctx, chat.Sender{RoleID: 1, Name: "role-1"}, chat.PublishRequest{
			Channel: chat.Channel{Kind: chat.ChannelWorld, Target: 8},
			Type:    "text", Body: []byte("hello"), RequestID: "dl3b-c1",
		})
		if err != nil {
			t.Fatal(err)
		}
		shifted("message.SentAtUnix (shown to players)", message.SentAtUnix, offset)
		shifted("message.StoredAtUnix (retention)", message.StoredAtUnix, 0)
		// 保留期 1h：消息按真实时间刚存进去，不该被清理。保留期若按业务时钟比对系统时间戳，
		// 截止线就前移了 24h，这条消息会被当成过期删掉。
		ref, err := service.Resolve(chat.Channel{Kind: chat.ChannelWorld, Target: 8}, 0)
		if err != nil {
			t.Fatal(err)
		}
		pruned, err := service.Prune(ctx, ref, 10)
		if err != nil {
			t.Fatal(err)
		}
		if pruned != 0 {
			t.Errorf("Prune with a 1h retention dropped %d message(s) stored a moment ago; retention must run on the system clock", pruned)
		}
	})

	t.Run("account creation and login are business time, the session is system time", func(t *testing.T) {
		service := app.MustLookup[*accountdomain.Service](registry, accountdomain.LocalCapabilityName)
		acct, err := service.Login(ctx, accountdomain.Identity{Channel: "store", OpenID: "dl3b-u1", Credential: "good"})
		if err != nil {
			t.Fatal(err)
		}
		shifted("account.CreatedAtUnix", acct.CreatedAtUnix, offset)
		shifted("account.LastLoginAtUnix", acct.LastLoginAtUnix, offset)
		if _, err := service.UpsertServer(ctx, accountdomain.GameServer{ID: 9, Name: "s9", Status: accountdomain.ServerOpen}); err != nil {
			t.Fatal(err)
		}
		role, err := service.CreateRole(ctx, acct.ID, 9, "Dl3bRole")
		if err != nil {
			t.Fatal(err)
		}
		shifted("role.CreatedAtUnix", role.CreatedAtUnix, offset)
		session, err := service.SelectRole(ctx, acct.ID, role.PlayerID)
		if err != nil {
			t.Fatal(err)
		}
		shifted("session.ExpiresAtUnix", session.ExpiresAtUnix, accountdomain.DefaultSessionTTL)
		if _, err := service.ValidateSession(ctx, role.PlayerID, session.Token); err != nil {
			t.Errorf("a fresh session token does not validate: %v", err)
		}
	})
}
