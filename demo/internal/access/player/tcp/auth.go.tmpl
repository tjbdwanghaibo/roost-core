package tcp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	svcaccount "github.com/tjbdwanghaibo/roost-core/service/account"
)

// The credential on the socket is a session ticket:
//
//	session:<player_id>:<token>
//
// The token was issued by the account service (Login → CreateRole →
// SelectRole) and is validated there; the principal is built from the role
// the account service returns, never from the id the socket claimed. There
// is no debug shortcut: a credential the server does not verify is not a
// credential, and the load test logs in for real (cmd/loadtest -account-nats).
const sessionTokenPrefix = "session:"

type accountsHandle struct{ accounts svcaccount.Accounts }

// applicationAuthenticator is application-owned: codegen writes it once and
// never rewrites it, so editing it here is the intended workflow.
//
// It reaches the account service through RegistryBound: Init sees only the
// configuration, so the client is bound in Provide, after the account
// ClientMod has published it and before the listener starts.
type applicationAuthenticator struct {
	accounts atomic.Pointer[accountsHandle]
}

func newApplicationAuthenticator(_ *viper.Viper) (Authenticator, error) {
	return &applicationAuthenticator{}, nil
}

func (authenticator *applicationAuthenticator) BindRegistry(registry *app.Registry) error {
	accounts, ok := app.Lookup[svcaccount.Accounts](registry, svcaccount.CapabilityName)
	if !ok || accounts == nil {
		return fmt.Errorf("demo authenticator: capability %q not found; the game service must list account under services.<game>.uses", svcaccount.CapabilityName)
	}
	authenticator.accounts.Store(&accountsHandle{accounts: accounts})
	return nil
}

var _ RegistryBound = (*applicationAuthenticator)(nil)

func (authenticator *applicationAuthenticator) Authenticate(ctx context.Context, token string, _ net.Addr) (gateway.Principal, error) {
	token = strings.TrimSpace(token)
	if rest, ok := strings.CutPrefix(token, sessionTokenPrefix); ok {
		return authenticator.authenticateSession(ctx, rest)
	}
	return gateway.Principal{}, gateway.ErrUnauthenticated
}

// authenticateSession asks the account service whether the token names this
// player. The role it returns — not the id the socket claimed — is what the
// principal is built from.
func (authenticator *applicationAuthenticator) authenticateSession(ctx context.Context, rest string) (gateway.Principal, error) {
	claimedID, sessionToken, ok := strings.Cut(rest, ":")
	if !ok || sessionToken == "" {
		return gateway.Principal{}, gateway.ErrUnauthenticated
	}
	playerID, err := strconv.ParseInt(claimedID, 10, 64)
	if err != nil || playerID <= 0 {
		return gateway.Principal{}, gateway.ErrUnauthenticated
	}
	handle := authenticator.accounts.Load()
	if handle == nil {
		return gateway.Principal{}, fmt.Errorf("demo authenticator: account client is not bound (the transport Mod binds it in Provide)")
	}
	role, err := handle.accounts.ValidateSession(ctx, playerID, sessionToken)
	if err != nil {
		// The reason stays in the log; the socket only learns "no".
		slog.Debug("session ticket rejected", "player_id", playerID, "err", err)
		return gateway.Principal{}, gateway.ErrUnauthenticated
	}
	return newPrincipal(role.PlayerID, role.ServerID), nil
}

// newPrincipal mints a SessionID unique per connection: the access layer uses
// it to tell two logins of the same player apart.
//
// serverID 是 account 校验会话时返回的角色 ServerID（建角时写入、之后不变），是权威值，不是客户端
// 声称的值；认证器只负责记下它，不负责拒绝——握手失败只能断开连接，客户端得不到原因，拒绝由登录端点
// 以 player_elsewhere 给出。键是 svcaccount.ServerIDClaim：登录端点（game/controllers/player 的
// BoundServerID）用同一个常量读，auth_test.go 钉住两边能对上。
func newPrincipal(playerID int64, serverID int32) gateway.Principal {
	return gateway.Principal{
		PlayerID:  playerID,
		SessionID: fmt.Sprintf("demo-%d-%d", playerID, time.Now().UnixNano()),
		Claims:    map[string]string{svcaccount.ServerIDClaim: strconv.FormatInt(int64(serverID), 10)},
	}
}
