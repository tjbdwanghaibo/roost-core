package account

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// 票据：客户端手里的短期凭证，全部由 account 签发、由 account 校验。登录入口（manifest）、Gate、game 都不
// 持有密钥，也不自己验——它们把票据原样交回 account。一共三种，用的是同一把会话密钥（Config.SessionSecret），
// 各有各的域：
//
//	账号票据   at2.<过期秒>.<游戏>.<账号>.<签名>     登录之后签发；凭它拿区服清单、要进服票据、刷新本游戏的 gt2。寿命 AccountTicketTTL。
//	进服票据   et1.<过期秒>.<服号>.<账号>.<签名>      选定一个服之后签发；握手时出示。寿命 SessionTTL。（enter.go）
//	角色会话令牌（security.SignSessionToken）         SelectRole 签发；“入口自己选角”那条路的握手凭证。寿命 SessionTTL。
//
// 互相不能冒充：前两种的签名输入以各自的域前缀开头（accountTicketDomain / enterTicketDomain），第三种签的是
// "<玩家号>:<过期毫秒>:<随机数>"；版本前缀也各不相同。轮换 session_secret 同时作废三者。
//
// 账号票据绑定游戏（维护者 2026-11-18）：账号号本来就只属于一个游戏，票据再把游戏写进签名的内容，校验时核对它等于账号
// 记录上的游戏，A 游戏的票据只在 A 游戏的账号上有效。格式因此从 at1（四段）改成 at2（五段），旧的 at1 不再认。
//
// 都是无状态的签名：有效期内可以重复使用，不能单张吊销。封号不靠吊销票据——校验账号票据与凭票进服时
// 都会再读一次账号与它的身份记录，被封的当场被拒（封号在身份级，identity_accounts.go）。

const (
	accountTicketVersion = "at2"
	// accountTicketSegments 是 at2 的段数：版本、过期秒、游戏、账号、签名。
	accountTicketSegments = 5
	accountTicketDomain   = "roost.account.account-ticket\n"
	// MaxTicketBytes 是一张票据的长度上限；超过的不做任何计算，直接按无效处理。
	MaxTicketBytes = 1024
)

// AccountTicket 是 IssueAccountTicket 的结果。
type AccountTicket struct {
	// Ticket 是票据本身，原样交给客户端。
	Ticket string `json:"ticket"`
	// ExpiresAtUnix 是它的过期时刻（系统时间）。
	ExpiresAtUnix int64 `json:"expires_at_unix"`
}

// IssueAccountTicket 给 accountID 签一张账号票据，绑定账号所属的游戏。调用方（登录入口）刚用 Login 确认过这个身份，
// accountID 是 Login 返回的账号——信任模型与 SelectRole 相同。只读。账号不存在答 ErrAccountMissing，被封答 ErrIdentityDenied。
func (s *Service) IssueAccountTicket(ctx context.Context, accountID string) (AccountTicket, error) {
	if strings.TrimSpace(accountID) == "" {
		return AccountTicket{}, fmt.Errorf("%w: account id is empty", ErrAccountMissing)
	}
	account, _, err := s.accountInGoodStanding(ctx, accountID)
	if err != nil {
		return AccountTicket{}, err
	}
	// 凭证的寿命是系统时间，不跟业务时间的偏移走。
	expiresAt := s.cfg.SystemNow().Add(s.cfg.AccountTicketTTL).Unix()
	body := accountTicketVersion + "." + strconv.FormatInt(expiresAt, 10) + "." + account.Game + "." +
		base64.RawURLEncoding.EncodeToString([]byte(accountID))
	s.report.Accepted("issue_account_ticket")
	return AccountTicket{Ticket: s.signTicket(accountTicketDomain, body), ExpiresAtUnix: expiresAt}, nil
}

// ValidateAccountTicket 校验一张账号票据，返回它绑定的账号（不带身份键）。只读。
//
// 票据无效——过期、被改、别的密钥签的、格式不对（含旧的 at1）、拿进服票据或角色会话令牌冒充、票据的游戏不是账号的
// 游戏——一律 ErrSessionInvalid，错误里不带票据也不说差在哪里。票据是好的而账号已经不在或被封：ErrAccountMissing /
// ErrIdentityDenied（封号对已经签出去的票据立即生效）。
//
// 登录入口的每个带票据的端点都先调它，拿到的账号再用于 RoleOnServer / IssueEnterTicket：
// 账号只能从这里来，不能从客户端的请求里读。
func (s *Service) ValidateAccountTicket(ctx context.Context, ticket string) (Account, error) {
	account, _, err := s.accountOfTicket(ctx, ticket)
	if err != nil {
		return Account{}, err
	}
	return account.public(), nil
}

// accountOfTicket 校验账号票据，返回它绑定的账号（带身份键，只给 account 内部用）与身份记录。
func (s *Service) accountOfTicket(ctx context.Context, ticket string) (Account, IdentityRecord, error) {
	parts, ok := s.verifyTicket(accountTicketDomain, accountTicketVersion, accountTicketSegments, ticket)
	if !ok {
		s.report.Refused("validate_account_ticket", "bad_ticket")
		return Account{}, IdentityRecord{}, errTicketInvalid()
	}
	game := parts[2]
	accountID, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || len(accountID) == 0 || !gameNamePattern.MatchString(game) {
		s.report.Refused("validate_account_ticket", "bad_ticket")
		return Account{}, IdentityRecord{}, errTicketInvalid()
	}
	account, identity, err := s.accountInGoodStanding(ctx, string(accountID))
	if err != nil {
		return Account{}, IdentityRecord{}, err
	}
	if account.Game != game {
		// 签名对得上而游戏对不上，只能是存储被改过；与坏票据同一个回答。
		s.report.Refused("validate_account_ticket", "game_mismatch")
		return Account{}, IdentityRecord{}, errTicketInvalid()
	}
	return account, identity, nil
}

// errTicketInvalid 是每一种无效票据的同一个回答。
func errTicketInvalid() error {
	return fmt.Errorf("%w: the ticket is not valid", ErrSessionInvalid)
}

// signTicket 给 body（版本、过期时刻与内容，点号分隔）签名，返回完整的票据：body + "." + base64url(签名)。
func (s *Service) signTicket(domain, body string) string {
	return body + "." + base64.RawURLEncoding.EncodeToString(s.ticketMAC(domain, body))
}

// verifyTicket 校验一张票据的形状、签名与寿命，返回它的各段（最后一段是签名）。
// 顺序是先验签名（定长时间比较）、再看过期：没验过签名的字段不值得解析。第二段总是过期时刻（Unix 秒）。
func (s *Service) verifyTicket(domain, version string, segments int, ticket string) ([]string, bool) {
	parts, ok := splitTicket(version, segments, ticket)
	if !ok {
		return nil, false
	}
	// 严格解码：32 字节的签名编成 43 个字符，最后一个字符有 2 位不用；宽松解码会把这 2 位不同的写法解成同一个签名，
	// 一张票据就有几种拼法都验得过。签名只有一种写法，票据才是“改任何一个字节都无效”。
	presented, err := base64.RawURLEncoding.Strict().DecodeString(parts[segments-1])
	if err != nil {
		return nil, false
	}
	body := ticket[:len(ticket)-len(parts[segments-1])-1]
	if !hmac.Equal(presented, s.ticketMAC(domain, body)) {
		return nil, false
	}
	expiresAt, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || s.cfg.SystemNow().Unix() >= expiresAt {
		return nil, false
	}
	return parts, true
}

// splitTicket 只按形状拆：长度、段数、版本。
func splitTicket(version string, segments int, ticket string) ([]string, bool) {
	if len(ticket) == 0 || len(ticket) > MaxTicketBytes {
		return nil, false
	}
	parts := strings.Split(ticket, ".")
	if len(parts) != segments || parts[0] != version {
		return nil, false
	}
	return parts, true
}

func (s *Service) ticketMAC(domain, body string) []byte {
	mac := hmac.New(sha256.New, []byte(s.cfg.SessionSecret))
	mac.Write([]byte(domain))
	mac.Write([]byte(body))
	return mac.Sum(nil)
}
