package account

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/security"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// U-0142 · C2（空洞测试）· nightly gap map kit `service/account` 8/20。
//
// SelectRole 在 Get 与 Update 之间角色消失 / 换了主人时以 ErrRoleMissing /
// ErrNotPermitted 拒绝而不是写回登录时间；ValidateSession 对签名有效但角色不存在
// 的 token 报 ErrRoleMissing；UpdateProfile 对不存在的角色报 ErrRoleMissing；
// CreateRole 的空账号 id、UpsertServer 的零 id、Identity 的空 open id、Redis 存储的
// 空键前缀各以对应哨兵拒绝。

// swappedRoles forwards to a real store but, once armed, hands every Update
// closure a substitute role state — the role vanished, or changed owner —
// between the caller's Get and its compare-and-set.
type swappedRoles struct {
	versionstore.Store[int64, Role]
	armed   bool
	present bool
	role    Role
}

func (s *swappedRoles) Update(ctx context.Context, key int64, mutate versionstore.Mutate[Role]) (versionstore.Versioned[Role], bool, error) {
	if s.armed {
		_, _, err := mutate(s.role, s.present)
		return versionstore.Versioned[Role]{}, false, err
	}
	return s.Store.Update(ctx, key, mutate)
}

func TestSelectRoleRefusesARoleThatVanishedOrChangedOwnerBeforeTheWrite(t *testing.T) {
	ctx := context.Background()
	roles := &swappedRoles{}
	service, _, _ := newService(t, func(cfg *Config) {
		roles.Store = cfg.Roles
		cfg.Roles = roles
	})
	account := login(t, service, "u1")
	role, err := service.CreateRole(ctx, account.ID, 1, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	roles.armed, roles.present = true, false
	if _, err := service.SelectRole(ctx, account.ID, role.PlayerID); !errors.Is(err, ErrRoleMissing) {
		t.Fatalf("SelectRole with the role vanishing before the write = %v, want ErrRoleMissing", err)
	}
	roles.present, roles.role = true, Role{PlayerID: role.PlayerID, AccountID: "someone-else"}
	if _, err := service.SelectRole(ctx, account.ID, role.PlayerID); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("SelectRole with the role changing owner before the write = %v, want ErrNotPermitted", err)
	}
	roles.armed = false
	if session, err := service.SelectRole(ctx, account.ID, role.PlayerID); err != nil || session.PlayerID != role.PlayerID {
		t.Fatalf("SelectRole with a stable role = (%+v, %v)", session, err)
	}
}

func TestSessionAndProfileOperationsRefuseAMissingRole(t *testing.T) {
	ctx := context.Background()
	service, c, cfg := newService(t)
	token, err := security.SignSessionToken(4242, cfg.SessionSecret, time.Hour, c.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ValidateSession(ctx, 4242, token); !errors.Is(err, ErrRoleMissing) {
		t.Fatalf("ValidateSession with a valid token for a missing role = %v, want ErrRoleMissing", err)
	}
	if _, err := service.UpdateProfile(ctx, "acct", 4242, []byte("{}")); !errors.Is(err, ErrRoleMissing) {
		t.Fatalf("UpdateProfile for a missing role = %v, want ErrRoleMissing", err)
	}
	if _, found, _ := cfg.Roles.Get(ctx, 4242); found {
		t.Fatal("a refused profile update created a role")
	}
}

func TestEntryPointsRefuseBlankIdentifiers(t *testing.T) {
	ctx := context.Background()
	service, _, _ := newService(t)
	if _, err := service.CreateRole(ctx, "  ", 1, "Alice"); !errors.Is(err, ErrAccountMissing) || !strings.Contains(err.Error(), "account id is empty") {
		t.Fatalf("CreateRole with a blank account id = %v", err)
	}
	if _, err := service.UpsertServer(ctx, GameServer{Status: ServerOpen}); !errors.Is(err, ErrServerInvalid) || !strings.Contains(err.Error(), "id is zero") {
		t.Fatalf("UpsertServer with id 0 = %v", err)
	}
	if err := (Identity{Channel: "store", OpenID: "  "}).Validate(); !errors.Is(err, ErrIdentityInvalid) || !strings.Contains(err.Error(), "open id is empty") {
		t.Fatalf("Identity without an open id = %v", err)
	}
	if err := (Identity{Channel: "store", OpenID: "u1"}).Validate(); err != nil {
		t.Fatalf("valid identity refused: %v", err)
	}
	if _, err := NewRedisStores(nil, "  ", time.Minute); err == nil || !strings.Contains(err.Error(), "key prefix is required") {
		t.Fatalf("NewRedisStores with a blank prefix = %v", err)
	}
}
