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
//	账号票据   at1.<过期秒>.<账号>.<签名>            登录之后签发；凭它拿区服清单、要进服票据。寿命 AccountTicketTTL。
//	进服票据   et1.<过期秒>.<服号>.<账号>.<签名>      选定一个服之后签发；握手时出示。寿命 SessionTTL。（enter.go）
//	角色会话令牌（security.SignSessionToken）         SelectRole 签发；“入口自己选角”那条路的握手凭证。寿命 SessionTTL。
//
// 互相不能冒充：前两种的签名输入以各自的域前缀开头（accountTicketDomain / enterTicketDomain），第三种签的是
// "<玩家号>:<过期毫秒>:<随机数>"；形状（版本前缀、段数）也各不相同。轮换 session_secret 同时作废三者。
//
// 都是无状态的签名：有效期内可以重复使用，不能单张吊销。封号不靠吊销票据——校验账号票据与凭票进服时
// 都会再读一次账号，被封的账号当场被拒。

const (
	accountTicketVersion = "at1"
	accountTicketDomain  = "roost.account.account-ticket\n"
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

// IssueAccountTicket 给 accountID 签一张账号票据。调用方（登录入口）刚用 Login 确认过这个身份，accountID 是
// Login 返回的账号——信任模型与 SelectRole 相同。只读。账号不存在答 ErrAccountMissing，被封答 ErrIdentityDenied。
func (s *Service) IssueAccountTicket(ctx context.Context, accountID string) (AccountTicket, error) {
	if strings.TrimSpace(accountID) == "" {
		return AccountTicket{}, fmt.Errorf("%w: account id is empty", ErrAccountMissing)
	}
	if _, err := s.accountInGoodStanding(ctx, accountID); err != nil {
		return AccountTicket{}, err
	}
	// 凭证的寿命是系统时间，不跟业务时间的偏移走。
	expiresAt := s.cfg.SystemNow().Add(s.cfg.AccountTicketTTL).Unix()
	body := accountTicketVersion + "." + strconv.FormatInt(expiresAt, 10) + "." + base64.RawURLEncoding.EncodeToString([]byte(accountID))
	s.report.Accepted("issue_account_ticket")
	return AccountTicket{Ticket: s.signTicket(accountTicketDomain, body), ExpiresAtUnix: expiresAt}, nil
}

// ValidateAccountTicket 校验一张账号票据，返回它绑定的账号。只读。
//
// 票据无效——过期、被改、别的密钥签的、格式不对、拿进服票据或角色会话令牌冒充——一律 ErrSessionInvalid，
// 错误里不带票据也不说差在哪里。票据是好的而账号已经不在或被封：ErrAccountMissing / ErrIdentityDenied
// （封号对已经签出去的票据立即生效）。
//
// 登录入口的每个带票据的端点都先调它，拿到的账号再用于 RoleOnServer / IssueEnterTicket：
// 账号只能从这里来，不能从客户端的请求里读。
func (s *Service) ValidateAccountTicket(ctx context.Context, ticket string) (Account, error) {
	parts, ok := s.verifyTicket(accountTicketDomain, accountTicketVersion, 4, ticket)
	if !ok {
		s.report.Refused("validate_account_ticket", "bad_ticket")
		return Account{}, errTicketInvalid()
	}
	accountID, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(accountID) == 0 {
		s.report.Refused("validate_account_ticket", "bad_ticket")
		return Account{}, errTicketInvalid()
	}
	return s.accountInGoodStanding(ctx, string(accountID))
}

// accountInGoodStanding 读账号并核对它存在且没有被封。
func (s *Service) accountInGoodStanding(ctx context.Context, accountID string) (Account, error) {
	account, found, err := s.cfg.Accounts.Get(ctx, accountID)
	if err != nil {
		return Account{}, err
	}
	if !found {
		return Account{}, fmt.Errorf("%w: %s", ErrAccountMissing, accountID)
	}
	if account.Value.Banned {
		return Account{}, fmt.Errorf("%w: account is banned", ErrIdentityDenied)
	}
	return account.Value, nil
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
	presented, err := base64.RawURLEncoding.DecodeString(parts[segments-1])
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
