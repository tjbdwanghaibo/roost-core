package account

import (
	"context"
	"errors"
	"testing"
	"time"
)

// D-L3 第八轮（维护者决定）：账号创建、角色创建、登录 / 登出时间是业务时间（Config.Now）；
// 会话 token 的签发与有效期、运维记录时间是系统时间（Config.SystemNow）。业务时钟比系统时钟快
// 一天时，业务时间戳前移一天，token 仍按真实时间签发、按真实时间过期。
func TestAccountTimesAreBusinessTimeAndSessionsAreSystemTime(t *testing.T) {
	ctx := context.Background()
	system := &clock{now: time.Unix(1_700_000_000, 0)}
	business := &clock{now: system.Now().Add(24 * time.Hour)}
	service, _, _ := newService(t, func(cfg *Config) { cfg.Now, cfg.SystemNow = business.Now, system.Now })

	acct, err := service.Login(ctx, Identity{Channel: "store", OpenID: "u1", Credential: "good"})
	if err != nil {
		t.Fatal(err)
	}
	if acct.CreatedAtUnix != business.Now().Unix() || acct.LastLoginAtUnix != business.Now().Unix() {
		t.Fatalf("account created %d, last login %d; want both on the business clock %d", acct.CreatedAtUnix, acct.LastLoginAtUnix, business.Now().Unix())
	}
	role, err := service.CreateRole(ctx, acct.ID, 1, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	if role.CreatedAtUnix != business.Now().Unix() {
		t.Fatalf("role created %d, want the business clock %d", role.CreatedAtUnix, business.Now().Unix())
	}
	server, found, err := service.cfg.Servers.Get(ctx, 1)
	if err != nil || !found || server.Value.UpdatedAtUnix != system.Now().Unix() {
		t.Fatalf("server row updated %d (found %v, %v), want the system clock %d: an operator record", server.Value.UpdatedAtUnix, found, err, system.Now().Unix())
	}

	session, err := service.SelectRole(ctx, acct.ID, role.PlayerID)
	if err != nil {
		t.Fatal(err)
	}
	if want := system.Now().Add(DefaultSessionTTL).Unix(); session.ExpiresAtUnix != want {
		t.Fatalf("session expires %d, want %d: system now + session ttl, whatever the business offset", session.ExpiresAtUnix, want)
	}
	selected, _, err := service.cfg.Roles.Get(ctx, role.PlayerID)
	if err != nil || selected.Value.LastLoginAtUnix != business.Now().Unix() {
		t.Fatalf("role last login %d (%v), want the business clock %d", selected.Value.LastLoginAtUnix, err, business.Now().Unix())
	}

	// 只前拨业务时间：token 不受影响。
	business.advance(7 * 24 * time.Hour)
	if _, err := service.ValidateSession(ctx, role.PlayerID, session.Token); err != nil {
		t.Fatalf("a token validated after only the business clock moved: %v; its life is real time", err)
	}
	// 真实时间过了有效期才失效。
	system.advance(DefaultSessionTTL + time.Minute)
	if _, err := service.ValidateSession(ctx, role.PlayerID, session.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("ValidateSession after the system clock passed the ttl = %v, want ErrSessionInvalid", err)
	}
	if err := service.MarkLogout(ctx, acct.ID, role.PlayerID); err != nil {
		t.Fatal(err)
	}
	loggedOut, _, err := service.cfg.Roles.Get(ctx, role.PlayerID)
	if err != nil || loggedOut.Value.LastLogoutAtUnix != business.Now().Unix() {
		t.Fatalf("role last logout %d (%v), want the business clock %d", loggedOut.Value.LastLogoutAtUnix, err, business.Now().Unix())
	}
}
