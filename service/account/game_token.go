package account

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"time"
)

// 游戏账号 token（方案 2026-11-16 §4.1.1）：一张带游戏名的短期账号凭证，回答“这是哪个账号、要玩哪个游戏”。
// 开房间类游戏的大厅（lobby）凭它认人；它不带区服、不带角色。在已有的三种凭证（tickets.go）之外另加，
// 不改它们。
//
//	游戏账号 token  gt1.<过期秒>.<游戏>.<账号>.<签名>   凭账号票据 + 游戏名换取；寿命按游戏配置（account.games）。
//
// 与账号票据、进服票据同一把会话密钥，独立的域前缀 gameTokenDomain，段数也不同，三者互相不能冒充。
// 游戏名写在签名的内容里：改了游戏段签名就对不上，A 游戏的 token 不能改成 B 游戏的。
//
// 签发与校验都在 account：
//
//	IssueGameToken(at1, game)   校验账号票据（被封的当场拒绝）、game 在 account.games 里，再签发。
//	ValidateGameToken(gt1)      验签、看过期，再读一次账号：被封的当场拒绝。返回账号、游戏与过期时刻。
//
// 校验不核对“是不是调用方的游戏”：调用方（lobby）拿返回的 Game 与请求里的游戏比较，用 GameAccount.RequireGame。
// 封号是全局的（同一个人在各个游戏里是同一个账号）；按游戏封号不在本期。
//
// 与 at1 一样是无状态的签名：有效期内可以重复使用，不能单张吊销；封号靠校验时再读账号生效。

const (
	gameTokenVersion = "gt1"
	gameTokenDomain  = "roost.account.game-token\n"
	// DefaultGameTokenTTL 是一个游戏没写 token_ttl 时的寿命。
	DefaultGameTokenTTL = 2 * time.Hour
	// MaxGameTokenTTL 是寿命的上限：它是一张不能吊销的凭证，寿命越长，泄露之后能用得越久。
	MaxGameTokenTTL = 24 * time.Hour
	// MaxGameNameBytes 是游戏名与服务名的长度上限。
	MaxGameNameBytes = 32
)

// gameNamePattern 是游戏名（与服务名）的写法：小写字母开头，小写字母、数字、下划线、连字符，至多 32 字节。
// 不能有点号：游戏名原样写在 token 的一段里。配置的键会被转成小写，所以只收小写。
var gameNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// GameSpec 是 account.games 里的一个游戏。
type GameSpec struct {
	Name string `json:"name"`
	// TokenTTL 是这个游戏的 token 寿命；零取 DefaultGameTokenTTL，不超过 MaxGameTokenTTL。
	TokenTTL time.Duration `json:"token_ttl"`
}

// GameToken 是 IssueGameToken 的结果。
type GameToken struct {
	// Token 是 token 本身（gt1. 开头），原样交给客户端。
	Token string `json:"token"`
	// Game 是 token 绑定的游戏。
	Game string `json:"game"`
	// ExpiresAtUnix 是它的过期时刻（系统时间，Unix 秒）。
	ExpiresAtUnix int64 `json:"expires_at_unix"`
}

// GameAccount 是 ValidateGameToken 的结果：token 绑定的账号与游戏。
type GameAccount struct {
	AccountID string `json:"account_id"`
	Game      string `json:"game"`
	// ExpiresAtUnix 是 token 的过期时刻（系统时间，Unix 秒）。
	ExpiresAtUnix int64 `json:"expires_at_unix"`
}

// RequireGame 核对 token 绑定的游戏就是 game（调用方自己服务的游戏，或请求里写的游戏）。不是答 ErrSessionInvalid：
// 拿 A 游戏的 token 进 B 游戏的大厅，与拿一张坏 token 是同一个回答。
func (a GameAccount) RequireGame(game string) error {
	if a.Game == "" || a.Game != game {
		return errTicketInvalid()
	}
	return nil
}

// newGameTable 检查 account.games，返回 游戏名 -> token 寿命。名字不合写法、重复、寿命超范围，一律报错。
func newGameTable(games []GameSpec) (map[string]time.Duration, error) {
	table := make(map[string]time.Duration, len(games))
	for _, game := range games {
		if !gameNamePattern.MatchString(game.Name) {
			return nil, fmt.Errorf("account: game name %q must be lowercase letters, digits, '_' or '-', start with a letter and be at most %d bytes", game.Name, MaxGameNameBytes)
		}
		if _, duplicate := table[game.Name]; duplicate {
			return nil, fmt.Errorf("account: game %q is listed twice", game.Name)
		}
		ttl := game.TokenTTL
		if ttl == 0 {
			ttl = DefaultGameTokenTTL
		}
		if ttl < 0 || ttl > MaxGameTokenTTL {
			return nil, fmt.Errorf("account: game %q token_ttl %s is out of range (0 for %s, at most %s)", game.Name, game.TokenTTL, DefaultGameTokenTTL, MaxGameTokenTTL)
		}
		table[game.Name] = ttl
	}
	return table, nil
}

// Games 返回配置的游戏名，升序。
func (s *Service) Games() []string {
	names := make([]string, 0, len(s.games))
	for name := range s.games {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// IssueGameToken 凭账号票据给它的账号签一张 game 的游戏账号 token。只读。
//
// 账号只取自账号票据：票据无效答 ErrSessionInvalid，账号不在或被封答 ErrAccountMissing / ErrIdentityDenied
// （与 ValidateAccountTicket 相同）。game 不在 account.games 里答 ErrGameInvalid，不签发。
func (s *Service) IssueGameToken(ctx context.Context, accountTicket string, game string) (GameToken, error) {
	account, err := s.ValidateAccountTicket(ctx, accountTicket)
	if err != nil {
		return GameToken{}, err
	}
	ttl, served := s.games[game]
	if !served {
		s.report.Refused("issue_game_token", "game_not_served")
		return GameToken{}, fmt.Errorf("%w: game %q is not in account.games", ErrGameInvalid, game)
	}
	// 凭证的寿命是系统时间，不跟业务时间的偏移走。
	expiresAt := s.cfg.SystemNow().Add(ttl).Unix()
	body := gameTokenVersion + "." + strconv.FormatInt(expiresAt, 10) + "." + game + "." + base64.RawURLEncoding.EncodeToString([]byte(account.ID))
	s.report.Accepted("issue_game_token")
	return GameToken{Token: s.signTicket(gameTokenDomain, body), Game: game, ExpiresAtUnix: expiresAt}, nil
}

// ValidateGameToken 校验一张游戏账号 token，返回它绑定的账号、游戏与过期时刻。只读。
//
// token 无效——过期、被改（含改了游戏段）、别的密钥签的、格式不对、拿账号票据 / 进服票据 / 角色会话令牌冒充——
// 一律 ErrSessionInvalid，错误里不说差在哪里。token 是好的而游戏已经不在 account.games 里：ErrGameInvalid。
// 账号已经不在或被封：ErrAccountMissing / ErrIdentityDenied（封号对已经签出去的 token 立即生效）。
//
// 它不核对调用方的游戏：调用方用 GameAccount.RequireGame 比较。
func (s *Service) ValidateGameToken(ctx context.Context, token string) (GameAccount, error) {
	parts, ok := s.verifyTicket(gameTokenDomain, gameTokenVersion, 5, token)
	if !ok {
		s.report.Refused("validate_game_token", "bad_token")
		return GameAccount{}, errTicketInvalid()
	}
	game := parts[2]
	accountID, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || len(accountID) == 0 || !gameNamePattern.MatchString(game) {
		s.report.Refused("validate_game_token", "bad_token")
		return GameAccount{}, errTicketInvalid()
	}
	if _, served := s.games[game]; !served {
		s.report.Refused("validate_game_token", "game_not_served")
		return GameAccount{}, fmt.Errorf("%w: game %q is no longer in account.games", ErrGameInvalid, game)
	}
	account, err := s.accountInGoodStanding(ctx, string(accountID))
	if err != nil {
		return GameAccount{}, err
	}
	expiresAt, _ := strconv.ParseInt(parts[1], 10, 64) // verifyTicket 已经解析过
	return GameAccount{AccountID: account.ID, Game: game, ExpiresAtUnix: expiresAt}, nil
}
