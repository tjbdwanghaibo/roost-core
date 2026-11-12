package account

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// 进服：登录入口（manifest）在玩家选定一个服之后向 account 要一张**进服票据**，客户端拿它去连那个服的 Gate；
// Gate（或嵌入 TCP 的 game 进程）的握手认证器再拿票据来问 account——这个账号在这个服上是谁，没有角色就新建。
//
//	登录入口：IssueEnterTicket(accountID, serverID)      -> 票据（只绑定账号与服）
//	握手认证器：EnterServer(ticket, 新建时用的名字)       -> 角色，以及是不是这一次新建的
//
// 为什么是这两步而不是“入口替客户端 CreateRole + SelectRole”：账号与角色的映射是 account 的数据，
// “新号还是旧号”由 account 在握手那一刻按账号与服解析，入口不必知道玩家号，也不在玩家真的连上之前建角。
// 已经自己做选角的工程照旧用 CreateRole / SelectRole / ValidateSession，两条路并存。
//
// 进服票据是无状态的签名，用 account 的会话密钥（Config.SessionSecret）签，有效期用会话的有效期
// （Config.SessionTTL，缺省 30 分钟）——不另加配置键：它与角色会话令牌是同一种东西（“凭它握手”的短期凭证），
// 寿命该由同一个旋钮管；轮换 session_secret 同时作废两者。有效期内可以反复握手（断线重连），不能单张吊销。
//
// 格式（五段，点号分隔，URL 安全）：
//
//	et1.<过期时刻 Unix 秒>.<服号>.<base64url(accountID)>.<base64url(HMAC-SHA256(会话密钥, 域 + 前四段))>
//
// 签名的输入以 enterTicketDomain 开头。角色会话令牌签的是 "<玩家号>:<过期毫秒>:<随机数>"（以数字开头、
// 两段），两种凭证的签名输入不可能相同，形状也不同：拿一种冒充另一种在各自的校验里都过不了。

const (
	enterTicketVersion = "et1"
	// enterTicketDomain 是进服票据签名输入的域前缀（域分隔）。
	enterTicketDomain = "roost.account.enter-ticket\n"
	// MaxEnterTicketBytes 是进服票据的长度上限；超过的不做任何计算，直接按无效处理。
	MaxEnterTicketBytes = 1024
)

// EnterTicket 是 IssueEnterTicket 的结果。
type EnterTicket struct {
	// Ticket 是票据本身，原样交给客户端。
	Ticket string `json:"ticket"`
	// ServerID 是票据绑定的服（玩家选中的服）。
	ServerID int32 `json:"server_id"`
	// ServingServerID 是承载服：ServerID 没有并入别的服时就是它自己，并入了就是那个服。
	// 客户端要连的是承载服的入口；握手时账号在 ServerID 上的角色由承载服的进程服务。
	ServingServerID int32 `json:"serving_server_id"`
	// ExpiresAtUnix 是票据的过期时刻（系统时间）。
	ExpiresAtUnix int64 `json:"expires_at_unix"`
}

// ServerEntry 是 EnterServer 的结果。
type ServerEntry struct {
	// Role 是账号在票据那个服上的角色，照 SelectRole 一样带着承载服、绑定代数与在途的换服。
	Role Role `json:"role"`
	// Created 为 true 表示角色是这一次调用新建（发布）出来的。它只供握手方参考（例如记一条“新号进服”的日志）：
	// 并发的两次握手里只有一个为 true，重放与重连为 false；但建角的应答丢失后重试时，角色可能在上一次调用里
	// 就建好了，这一次看到的是 false。所以“首次发放”这类只能做一次的事不能以它为依据——那是游戏服自己的
	// 持久事实（进入游戏时的首次初始化）。
	Created bool `json:"created,omitempty"`
}

// IssueEnterTicket 给 accountID 签一张进 serverID 的票据。信任模型与 SelectRole 相同：调用方（登录入口）
// 已经认证过这个账号，accountID 是它给的。只读——不建角、不改任何记录。
//
// 拒绝：账号不存在（ErrAccountMissing）、账号被封（ErrIdentityDenied）、服号不是正数或没登记
// （ErrServerInvalid）、承载服解析不了（ErrServerInvalid / ErrServerMergeCycle）。
//
// 不看区服的状态，也不看账号在那个服有没有角色：维护中、已满、关闭的服上已有的角色照常能进，没有角色的
// 由握手时的 EnterServer 按建角的规则拒绝（ErrServerClosed）。已并入别的服的服照实签给它——那里的老角色
// 要用原服号才问得到，ServingServerID 告诉入口该给哪个服的地址。
func (s *Service) IssueEnterTicket(ctx context.Context, accountID string, serverID int32) (EnterTicket, error) {
	if strings.TrimSpace(accountID) == "" {
		return EnterTicket{}, fmt.Errorf("%w: account id is empty", ErrAccountMissing)
	}
	if serverID <= 0 {
		return EnterTicket{}, fmt.Errorf("%w: server id %d is not positive", ErrServerInvalid, serverID)
	}
	if err := s.accountMayEnter(ctx, accountID); err != nil {
		return EnterTicket{}, err
	}
	if _, err := s.mustServer(ctx, serverID); err != nil {
		return EnterTicket{}, err
	}
	serving, err := s.servingServer(ctx, serverID)
	if err != nil {
		return EnterTicket{}, err
	}
	expiresAt := s.cfg.SystemNow().Add(s.cfg.SessionTTL).Unix() // 凭证的寿命是系统时间，不跟业务时间的偏移走
	signed := enterTicketVersion + "." + strconv.FormatInt(expiresAt, 10) + "." + strconv.FormatInt(int64(serverID), 10) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(accountID))
	s.report.Accepted("issue_enter_ticket")
	return EnterTicket{
		Ticket:   signed + "." + base64.RawURLEncoding.EncodeToString(s.enterTicketMAC(signed)),
		ServerID: serverID, ServingServerID: serving, ExpiresAtUnix: expiresAt,
	}, nil
}

// EnterServer 凭进服票据解析“这个账号在这个服上的角色”，没有就新建，并记一次登录。调用方是握手的认证器
// （Gate，或嵌入 TCP 的 game 进程）；它与本接口其余方法一样是可信的内部调用方，票据才是客户端出示的凭证。
// **它会写**：新建角色、最近登录时间、换服的收尾。
//
//   - 票据无效——过期、被改、别的密钥签的、格式不对、拿角色会话令牌冒充：ErrSessionInvalid，什么也不写。
//   - 账号不存在 / 被封：ErrAccountMissing / ErrIdentityDenied。封号在这里立即生效：已经签出去的票据也进不来。
//   - 账号在那个服上有角色（RoleOnServer 的判定）：返回它，Created 为 false；name 被忽略。
//   - 没有角色：用 name 走 CreateRole 的同一条路径新建（同样的名字规则、区服状态、名额、建角计划）。
//     name 为空答 ErrRoleNameRequired——名字的规则不在这里，由握手方给；服不是 open 或已并入别的服答
//     ErrServerClosed；名额被换服占位、被另一个名字的待定计划占着答 ErrRoleLimit；名字被占答 ErrNameTaken。
//   - slot 与角色对不上、建角正在对账：ErrConflict（与 RoleOnServer / SelectRole 相同），稍后重试。
//
// 幂等：同一张票据重放、两次握手并发，只会有一个角色——建角靠的是 slot 的唯一性与建角计划，不是这里的判断。
// 握手方每次给同一个名字（确定性的名字规则）时，应答丢失之后的重试会接上同一个计划把它做完，不会卡住；
// 换了名字则按 CreateRole 的规则，可能得到 ErrRoleLimit。
func (s *Service) EnterServer(ctx context.Context, ticket string, name string) (ServerEntry, error) {
	accountID, serverID, err := s.verifyEnterTicket(ticket)
	if err != nil {
		s.report.Refused("enter_server", "bad_ticket")
		return ServerEntry{}, err
	}
	if err := s.accountMayEnter(ctx, accountID); err != nil {
		s.report.Refused("enter_server", "account")
		return ServerEntry{}, err
	}
	role, err := s.RoleOnServer(ctx, accountID, serverID)
	created := false
	switch {
	case err == nil:
	case errors.Is(err, ErrRoleMissing):
		if strings.TrimSpace(name) == "" {
			s.report.Refused("enter_server", "name_required")
			return ServerEntry{}, fmt.Errorf("%w: %s has no role on server %d and no name was given to create one", ErrRoleNameRequired, accountID, serverID)
		}
		if role, created, err = s.admitAndCreateRole(ctx, accountID, serverID, name); err != nil {
			return ServerEntry{}, err
		}
	default:
		return ServerEntry{}, err
	}
	// 与 SelectRole 相同：登录是换服之后一定会走到的写入口，在这里补完上一次换服的收尾；补不完不挡进服。
	s.settleTransferBestEffort(ctx, role)
	now := s.cfg.Now() // 最近登录时间是业务时间
	stored, _, err := s.cfg.Roles.Update(ctx, role.PlayerID, func(current Role, found bool) (Role, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: player %d", ErrRoleMissing, role.PlayerID)
		}
		if current.AccountID != accountID {
			return current, false, fmt.Errorf("%w: player %d", ErrNotPermitted, role.PlayerID)
		}
		current.LastLoginAtUnix = now.Unix()
		return current, true, nil
	})
	if err != nil {
		return ServerEntry{}, err
	}
	entered, err := s.withServing(ctx, stored.Value)
	if err != nil {
		return ServerEntry{}, err
	}
	s.report.Accepted("enter_server")
	return ServerEntry{Role: entered, Created: created}, nil
}

// accountMayEnter 核对账号存在且没有被封。
func (s *Service) accountMayEnter(ctx context.Context, accountID string) error {
	account, found, err := s.cfg.Accounts.Get(ctx, accountID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrAccountMissing, accountID)
	}
	if account.Value.Banned {
		return fmt.Errorf("%w: account is banned", ErrIdentityDenied)
	}
	return nil
}

// PeekEnterTicket 读出进服票据里写的账号与服，**不验证签名与过期**。它只有一个用途：握手方要在调
// EnterServer 之前按“账号 + 服”算出新建角色时用的名字。读到的值不能当作身份——票据是不是真的只有
// EnterServer 说了算；票据是伪造的时，这里读出什么都不要紧，EnterServer 会拒绝它。形状不对时 ok 为 false。
func PeekEnterTicket(ticket string) (accountID string, serverID int32, ok bool) {
	parts, ok := splitEnterTicket(ticket)
	if !ok {
		return "", 0, false
	}
	return decodeEnterTicketBody(parts)
}

// splitEnterTicket 按形状拆票据：长度、五段、版本。
func splitEnterTicket(ticket string) ([]string, bool) {
	if len(ticket) == 0 || len(ticket) > MaxEnterTicketBytes {
		return nil, false
	}
	parts := strings.Split(ticket, ".")
	if len(parts) != 5 || parts[0] != enterTicketVersion {
		return nil, false
	}
	return parts, true
}

// decodeEnterTicketBody 解出服号与账号；服号必须是正数的规范十进制写法，账号不能为空。
func decodeEnterTicketBody(parts []string) (accountID string, serverID int32, ok bool) {
	sid, err := strconv.ParseInt(parts[2], 10, 32)
	if err != nil || sid <= 0 || strconv.FormatInt(sid, 10) != parts[2] {
		return "", 0, false
	}
	account, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || len(account) == 0 {
		return "", 0, false
	}
	return string(account), int32(sid), true
}

// verifyEnterTicket 校验一张进服票据，返回它绑定的账号与服。任何一种不通过都是同一个 ErrSessionInvalid，
// 错误里不带票据。先验签名（定长时间比较）、再看内容：没验过签名的字段不值得解析。
func (s *Service) verifyEnterTicket(ticket string) (accountID string, serverID int32, err error) {
	invalid := fmt.Errorf("%w: the enter ticket is not valid", ErrSessionInvalid)
	parts, ok := splitEnterTicket(ticket)
	if !ok {
		return "", 0, invalid
	}
	presented, decodeErr := base64.RawURLEncoding.DecodeString(parts[4])
	if decodeErr != nil {
		return "", 0, invalid
	}
	signed := ticket[:len(ticket)-len(parts[4])-1]
	if !hmac.Equal(presented, s.enterTicketMAC(signed)) {
		return "", 0, invalid
	}
	expiresAt, parseErr := strconv.ParseInt(parts[1], 10, 64)
	if parseErr != nil || s.cfg.SystemNow().Unix() >= expiresAt {
		return "", 0, invalid
	}
	accountID, serverID, ok = decodeEnterTicketBody(parts)
	if !ok {
		return "", 0, invalid
	}
	return accountID, serverID, nil
}

func (s *Service) enterTicketMAC(signed string) []byte {
	mac := hmac.New(sha256.New, []byte(s.cfg.SessionSecret))
	mac.Write([]byte(enterTicketDomain))
	mac.Write([]byte(signed))
	return mac.Sum(nil)
}
