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
	if role.PlayerID != playerID || role.ServerID <= 0 {
		return gateway.Principal{}, gateway.ErrUnauthenticated
	}
	// 两端各自生成候选 SessionID，Game 只采用已通过角色/区服复核的 Gate 绑定 SessionID。
	return gateway.Principal{PlayerID: role.PlayerID, ServerID: role.ServerID, SessionID: gateway.NewBroadcastID()}, nil
}
