package account

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// 游戏账号 token（方案 2026-11-16 §4.1.1）：一张带游戏名的短期账号凭证，回答“这是哪个账号、要玩哪个游戏”。
// 开房间类游戏的大厅（lobby）凭它认人；它不带区服、不带角色。在已有的三种凭证（tickets.go）之外另加，
// 不改它们。
//
//	游戏账号 token  gt2.<过期秒>.<游戏>.<账号>.<代数>.<签名>   凭账号票据 + 游戏名换取；寿命按游戏配置（account.games）。
//
// 与账号票据、进服票据同一把会话密钥，独立的域前缀 gameTokenDomain；形状也不同（at1 四段、et1 五段、gt2 六段，
// 版本前缀各不相同），三者互相不能冒充。游戏名与代数都写在签名的内容里：改了游戏段或代数段签名就对不上，
// A 游戏的 token 不能改成 B 游戏的，吊销之前的 token 也不能把代数改成当前的。
//
// 签发与校验都在 account：
//
//	IssueGameToken(at1, game)   校验账号票据（被封的当场拒绝）、game 在 account.games 里，再签发；代数取账号记录上的当前值。
//	ValidateGameToken(gt2)      验签、看过期，再读一次账号：被封的当场拒绝；代数不等于账号上的当前值的拒绝。
//	                            返回账号、游戏与过期时刻。
//	RevokeGameTokens(账号)      把账号上的代数加一并落库：在此之前签发的 gt2 全部失效，之后签发的照常可用。
//
// 校验不核对“是不是调用方的游戏”：调用方（lobby）拿返回的 Game 与请求里的游戏比较，用 GameAccount.RequireGame。
// 封号是全局的（同一个人在各个游戏里是同一个账号）；按游戏封号不在本期。
//
// 吊销（交接 C6，维护者 2026-11-17 选“代数”）：不能单张吊销，只能让一个账号在此之前签发的全部失效。
// 校验本来就要重读账号（为了封号），比较代数不多一次读。代数是账号记录上的一个计数（Account.GameTokenGeneration），
// 只增不减；谁让它加一见 RevokeGameTokens。吊销只拦以后的大厅调用（建房、加入、重连都要再校验一次），
// 不把人从已经连上的 relay 房间里断开。
const (
	gameTokenVersion = "gt2"
	// gameTokenSegments 是 gt2 的段数：版本、过期秒、游戏、账号、代数、签名。
	gameTokenSegments = 6
	gameTokenDomain   = "roost.account.game-token\n"
	// DefaultGameTokenTTL 是一个游戏没写 token_ttl 时的寿命。
	DefaultGameTokenTTL = 2 * time.Hour
	// MaxGameTokenTTL 是寿命的上限：它不能单张吊销（只能按账号整代吊销，RevokeGameTokens），寿命越长，
	// 泄露之后、吊销之前能用得越久。
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
	// Token 是 token 本身（gt2. 开头），原样交给客户端。
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
	// 代数取刚读到的账号记录：与吊销并发时，在吊销落库之前读到的是旧代数，签出来的 token 随即失效——吊销赢。
	body := gameTokenVersion + "." + strconv.FormatInt(expiresAt, 10) + "." + game + "." +
		base64.RawURLEncoding.EncodeToString([]byte(account.ID)) + "." + strconv.FormatUint(account.GameTokenGeneration, 10)
	s.report.Accepted("issue_game_token")
	return GameToken{Token: s.signTicket(gameTokenDomain, body), Game: game, ExpiresAtUnix: expiresAt}, nil
}

// ValidateGameToken 校验一张游戏账号 token，返回它绑定的账号、游戏与过期时刻。只读。
//
// token 无效——过期、被改（含改了游戏段、代数段）、别的密钥签的、格式不对（含旧的 gt1）、拿账号票据 / 进服票据 /
// 角色会话令牌冒充、已经被吊销（代数不是账号上的当前值）——一律 ErrSessionInvalid，错误里不说差在哪里。token 是好的而游戏已经不在 account.games 里：ErrGameInvalid。
// 账号已经不在或被封：ErrAccountMissing / ErrIdentityDenied（封号对已经签出去的 token 立即生效）。
//
// 它不核对调用方的游戏：调用方用 GameAccount.RequireGame 比较。
func (s *Service) ValidateGameToken(ctx context.Context, token string) (GameAccount, error) {
	parts, ok := s.verifyTicket(gameTokenDomain, gameTokenVersion, gameTokenSegments, token)
	if !ok {
		s.report.Refused("validate_game_token", "bad_token")
		return GameAccount{}, errTicketInvalid()
	}
	game := parts[2]
	accountID, err := base64.RawURLEncoding.DecodeString(parts[3])
	generation, generationErr := strconv.ParseUint(parts[4], 10, 64)
	if err != nil || generationErr != nil || len(accountID) == 0 || !gameNamePattern.MatchString(game) {
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
	// 吊销：代数不是账号上的当前值，就是吊销之前签发的。回答与坏 token 相同，不说是被吊销了。
	if generation != account.GameTokenGeneration {
		s.report.Refused("validate_game_token", "revoked")
		return GameAccount{}, errTicketInvalid()
	}
	expiresAt, _ := strconv.ParseInt(parts[1], 10, 64) // verifyTicket 已经解析过
	return GameAccount{AccountID: account.ID, Game: game, ExpiresAtUnix: expiresAt}, nil
}

// RevokeGameTokens 吊销 accountID 在此之前签发的全部游戏账号 token：把账号上的代数加一并落库，返回新的代数。
// 之后签发的 token 带新代数，照常可用；账号的封号状态、账号票据、进服票据都不受影响。
//
// 谁调它（维护者 2026-11-17）：
//   - 运维：accountctl 的 revoke-game-tokens 命令（login 模块的 cmd/accountctl）。
//   - 凭证变更：账号的登录凭证改了（改密码、换绑渠道身份、怀疑 token 泄露）时调它。account 本身没有改密码的路径
//     ——身份由渠道确认（Verifier），密码在渠道那一侧——所以由凭证变更发生的那一方（渠道服务、工程自己的账号中心）
//     经总线调这个接口。
//
// 账号不存在答 ErrAccountMissing。被封的账号照样可以吊销（封号本来就让它的 token 全部失效，解封之后旧 token
// 也不会复活）。存储结果未知（versionstore.ErrOutcomeUnknown）原样返回：调用方可以直接重试，重试可能让代数
// 多加一次，后果只是这期间新签的 token 也失效，不会让任何旧 token 复活。
func (s *Service) RevokeGameTokens(ctx context.Context, accountID string) (uint64, error) {
	if strings.TrimSpace(accountID) == "" {
		return 0, fmt.Errorf("%w: account id is empty", ErrAccountMissing)
	}
	written, _, err := s.cfg.Accounts.Update(ctx, accountID, func(current Account, found bool) (Account, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: %s", ErrAccountMissing, accountID)
		}
		next := current
		next.GameTokenGeneration++
		return next, true, nil
	})
	if err != nil {
		s.report.Refused("revoke_game_tokens", "failed")
		return 0, err
	}
	s.report.Accepted("revoke_game_tokens")
	return written.Value.GameTokenGeneration, nil
}
