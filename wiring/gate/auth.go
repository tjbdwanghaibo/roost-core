package gate

import (
	"context"
	"net"
	"strconv"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	"github.com/tjbdwanghaibo/roost-core/service/account"
)

// AccountAuthenticator 使用正式 Accounts 能力校验角色票据。PlayerID 提示没有权限，
// 只有 ValidateSession 返回的 Role 决定 PlayerID 与固定区服，不接受 accountId 代替角色。
//
// 固定区服是承载服（Role.ServingServerID）：原服已并入另一服（别名合服，service/account 的 MergeServer）时
// 是那个服，没有合服时等于原服。Gate.BeforeACK 按 Principal.ServerID 选 game，所以合服后 A 的角色连到 B 的
// game，Gate 不需要任何合服配置。Claims 另记两项：ServerIDClaim（承载服，与 Principal.ServerID 相同）与
// OriginServerIDClaim（原服，供 Gate 侧的业务钩子显示原服标识）；Gate 转交给 game 的绑定不带 Claims，game
// 侧需要原服时从自己的角色数据取。
type AccountAuthenticator struct{ Accounts account.Accounts }

func (auth AccountAuthenticator) Authenticate(ctx context.Context, ticket string, _ net.Addr) (gateway.Principal, error) {
	if auth.Accounts == nil || len(ticket) > 8192 {
		return gateway.Principal{}, gateway.ErrUnauthenticated
	}
	parts := strings.SplitN(ticket, ":", 3)
	if len(parts) != 3 || parts[0] != "session" || parts[2] == "" {
		return gateway.Principal{}, gateway.ErrUnauthenticated
	}
	playerID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || playerID == 0 {
		return gateway.Principal{}, gateway.ErrUnauthenticated
	}
	role, err := auth.Accounts.ValidateSession(ctx, playerID, parts[2])
	if err != nil {
		return gateway.Principal{}, err
	}
	if role.PlayerID != playerID || role.ServerID <= 0 || role.ServingServerID <= 0 {
		return gateway.Principal{}, gateway.ErrUnauthenticated
	}
	// 两端各自生成候选 SessionID，Game 只采用已通过角色/区服复核的 Gate 绑定 SessionID。
	return gateway.Principal{
		PlayerID: role.PlayerID, ServerID: role.ServingServerID, SessionID: gateway.NewBroadcastID(),
		Claims: map[string]string{
			account.ServerIDClaim:       strconv.FormatInt(int64(role.ServingServerID), 10),
			account.OriginServerIDClaim: strconv.FormatInt(int64(role.ServerID), 10),
		},
	}, nil
}
