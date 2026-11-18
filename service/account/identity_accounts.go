package account

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// 一套 account 服务多个游戏（维护者 2026-11-18，FEATURE-2026-11-16 §4.1）：同一个人（渠道确认的 渠道 + open id）在每个
// 游戏里是不同的账号号，账号号只属于那个游戏。lobby、relay、game 与客户端看到的都是这个游戏的账号号；别的游戏的账号号、
// 以及“它们是同一个人”这层关联只在 account 内部的身份记录（IdentityRecord）里，任何总线接口都不返回。
//
// 两层记录：
//
//	身份记录  Identities[身份键]  {游戏: 账号号}、封号、凭证版本、gt2 代数     —— 一个人一条，各游戏共用
//	账号记录  Accounts[账号号]    {游戏, 身份键, 建号与最近登录时刻, 运维备注}   —— 一个人在一个游戏里一条
//
// 账号号由 AccountIDs 发（全局计数器，各游戏不重复，递增的十进制数）；不随机化，随机化可以防按号推算注册先后，留作可选。
// 封号、凭证版本、gt2 代数在身份级：封一个人就是封他所有游戏的账号；在任何一个游戏登录时发现密码变了，所有游戏的 gt2
// 一起失效。按游戏封号以后再加。角色、slot、进服票据、换服、合服、lobby 的房间与被踢名单都按账号号工作，不用分游戏。

// AccountIDAllocator 发账号号。号必须跨进程、跨重启都不重复：Login 用它给新的（身份, 游戏）建账号，号被占用时
// 答 ErrConflict 而不是覆盖（Accounts.Create 只插不改），所以一个重新从头数的计数器不会串号，但会让新账号一直登录失败。
type AccountIDAllocator interface {
	// NextAccountID 返回一个没用过的正整数。出错时不发号：登录可以重试，发出一个可能重复的号不能挽回。
	NextAccountID(ctx context.Context) (int64, error)
}

// MemoryAccountIDs 是进程内的计数器，只给测试与单进程工具用：重启从 1 再数，与持久存储一起用会撞号。
type MemoryAccountIDs struct{ next atomic.Int64 }

// NewMemoryAccountIDs 构造一个从 1 数起的进程内计数器。
func NewMemoryAccountIDs() *MemoryAccountIDs { return &MemoryAccountIDs{} }

// NextAccountID implements AccountIDAllocator.
func (m *MemoryAccountIDs) NextAccountID(context.Context) (int64, error) { return m.next.Add(1), nil }

// pickDefaultGame 定登录没写游戏时落到的游戏：配置写了就必须在 account.games 里；没写而只有一个游戏时就是它；否则没有缺省。
func pickDefaultGame(games map[string]time.Duration, configured string) (string, error) {
	if configured != "" {
		if _, ok := games[configured]; !ok {
			return "", fmt.Errorf("account: default_game %q is not in account.games", configured)
		}
		return configured, nil
	}
	if len(games) == 1 {
		for name := range games {
			return name, nil
		}
	}
	return "", nil
}

// loginGame 是这次登录落到的游戏：写了的必须在 account.games 里；没写的取缺省游戏，没有缺省游戏答 ErrGameInvalid。
func (s *Service) loginGame(requested string) (string, error) {
	if requested == "" {
		if s.defaultGame == "" {
			return "", fmt.Errorf("%w: the login names no game and account.games has no default (set account.default_game, or list exactly one game)", ErrGameInvalid)
		}
		return s.defaultGame, nil
	}
	if _, served := s.games[requested]; !served {
		return "", fmt.Errorf("%w: game %q is not in account.games", ErrGameInvalid, requested)
	}
	return requested, nil
}

// Login verifies an identity with its channel and returns the account it has
// in the requested game, creating it on first sight.
//
// Verification happens before anything else and its result — not the submitted
// identity — decides who logs in. A caller cannot present its own credential
// alongside someone else's open id.
//
// 顺序（每一步停下重试都安全）：
//
//  1. 定游戏（loginGame），交给渠道验证（验证器看得到游戏）。
//  2. 身份记录（recordIdentity）：没有就建；记下凭证版本的变化（变了 gt2 代数加一，各游戏共用）；被封的到此为止，
//     版本变化照样落库。
//  3. 这个游戏还没有账号号（openGameAccount）：发号、insert-only 建账号记录，再把号写进身份记录。同一身份同一游戏的
//     并发登录以先写进身份记录的为准，输家建的那条账号记录没人引用，留着不删（删了也不会更安全：号不再发）。
//  4. 账号记录记最近登录时刻（stampLogin），核对它确实属于这个身份、这个游戏。
//
// 交出去的账号不带身份键（Account.public）。
func (s *Service) Login(ctx context.Context, identity Identity) (Account, error) {
	if err := identity.Validate(); err != nil {
		return Account{}, err
	}
	game, err := s.loginGame(identity.Game)
	if err != nil {
		s.report.Refused("login", "game_invalid")
		return Account{}, err
	}
	identity.Game = game
	verified, err := s.cfg.Verifier.Verify(ctx, identity)
	if err != nil {
		if errors.Is(err, ErrVerifierUnavailable) {
			s.report.Refused("login", "verifier_unavailable")
			// Propagated as-is: a channel outage is not a client error, and
			// answering "denied" would tell the player their credential is
			// bad when it is not.
			return Account{}, err
		}
		s.report.Refused("login", "denied")
		return Account{}, fmt.Errorf("%w: %s", ErrIdentityDenied, err)
	}
	if err := verified.Validate(); err != nil {
		return Account{}, err
	}
	key := identityKey(verified.Channel, verified.OpenID)
	now := s.cfg.Now()

	record, banned, err := s.recordIdentity(ctx, key, verified, now)
	if err != nil {
		return Account{}, err
	}
	if banned {
		s.report.Refused("login", "banned")
		return Account{}, fmt.Errorf("%w: identity is banned", ErrIdentityDenied)
	}
	accountID := record.Accounts[game]
	if accountID == "" {
		if accountID, err = s.openGameAccount(ctx, key, game, now); err != nil {
			return Account{}, err
		}
	}
	account, err := s.stampLogin(ctx, accountID, key, game, now)
	if err != nil {
		return Account{}, err
	}
	s.report.Accepted("login")
	return account.public(), nil
}

// recordIdentity 读写身份记录：没有就建，记下凭证版本的变化，报告是否被封。读改写在同一次 Identities.Update 里：
// 并发登录各自基于最新提交的记录判断（同一个新版本只加一次，不同的新版本各加一次）。
func (s *Service) recordIdentity(ctx context.Context, key string, verified Verified, now time.Time) (IdentityRecord, bool, error) {
	var (
		result IdentityRecord
		banned bool
	)
	// result、banned 由 mutate 每次调用重新设置：Update 冲突时会重读、重跑 mutate，只有最后一次的判断算数。
	_, _, err := s.cfg.Identities.Update(ctx, key, func(current IdentityRecord, found bool) (IdentityRecord, bool, error) {
		next := current
		if !found {
			next = IdentityRecord{Key: key, Channel: verified.Channel, OpenID: verified.OpenID, CreatedAtUnix: now.Unix()}
		} else if !sameVerifiedIdentity(current, verified) {
			return current, false, fmt.Errorf("%w: stored identity does not match verified identity", ErrIdentityInvalid)
		}
		// 被封也照样记下凭证变化：封号期间改了密码，解封之后旧 token 不能复活。
		next, changed := withCredentialVersion(next, verified.CredentialVersion)
		result, banned = next.clone(), next.Banned
		return next, changed || !found, nil
	})
	if err != nil {
		return IdentityRecord{}, false, err
	}
	return result, banned, nil
}

// withCredentialVersion 把渠道这次给的凭证版本记到身份上（交接 C6 第 1 点，维护者 2026-11-18 选 B），
// 返回新记录与它是否变了：
//
//   - 渠道没给（空）：不动，已存的版本不抹掉。
//   - 身份上还没存过：只记录，不加一（第一次见到，无从比较）。
//   - 与存的相同：不动。
//   - 与存的不同：游戏账号 token 的代数加一（与 RevokeGameTokens 是同一个计数、同一次落库），换成新版本。
//
// 代数在身份级，所以在哪个游戏登录发现的变化，这个人所有游戏的 gt2 都失效。Login 结果未知后重试时，新版本已经存下，不会再加。
func withCredentialVersion(identity IdentityRecord, version string) (IdentityRecord, bool) {
	if version == "" || version == identity.CredentialVersion {
		return identity, false
	}
	if identity.CredentialVersion != "" {
		identity.GameTokenGeneration++
	}
	identity.CredentialVersion = version
	return identity, true
}

func sameVerifiedIdentity(identity IdentityRecord, verified Verified) bool {
	return strings.EqualFold(strings.TrimSpace(string(identity.Channel)), strings.TrimSpace(string(verified.Channel))) &&
		strings.TrimSpace(identity.OpenID) == strings.TrimSpace(verified.OpenID)
}

// openGameAccount 给身份 key 在 game 里建账号，返回最终映射下的账号号。
//
// 先建账号记录、后写映射：映射一旦写下就指向一条已经存在、属于这个身份与游戏的账号记录。反过来（先写映射）时，
// 计数器发了一个被占用的号会让映射永远指向别人的账号。中途停下（建了记录、映射没写成）只留一条没人引用的账号记录。
func (s *Service) openGameAccount(ctx context.Context, key, game string, now time.Time) (string, error) {
	number, err := s.cfg.AccountIDs.NextAccountID(ctx)
	if err != nil {
		return "", fmt.Errorf("account: allocate an account id: %w", err)
	}
	if number <= 0 {
		return "", fmt.Errorf("account: the account id allocator returned %d; ids are positive", number)
	}
	candidate := strconv.FormatInt(number, 10)
	_, created, err := s.cfg.Accounts.Create(ctx, candidate, Account{ID: candidate, Game: game, Identity: key, CreatedAtUnix: now.Unix()})
	if err != nil {
		return "", err
	}
	if !created {
		// 号被占用只能是计数器从头再数了（数据被清、换了前缀没搬计数器）。不覆盖别人的账号。
		s.report.Refused("login", "account_id_in_use")
		return "", fmt.Errorf("%w: the account id allocator returned %s, which is already in use", ErrConflict, candidate)
	}
	var bound string
	_, _, err = s.cfg.Identities.Update(ctx, key, func(current IdentityRecord, found bool) (IdentityRecord, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: identity record vanished while opening a %s account", ErrConflict, game)
		}
		if existing := current.Accounts[game]; existing != "" {
			// 并发的另一次登录先写下了：用它的号，这次建的记录没人引用。
			bound = existing
			return current, false, nil
		}
		bound = candidate
		return current.withAccount(game, candidate), true, nil
	})
	if err != nil {
		return "", err
	}
	return bound, nil
}

// stampLogin 给账号记最近登录时刻（业务时间），核对它属于这个身份与游戏。映射指向的账号记录不在、或属于别人，
// 都是存储被手工改过（openGameAccount 先建记录后写映射），答 ErrConflict，不猜。
func (s *Service) stampLogin(ctx context.Context, accountID, key, game string, now time.Time) (Account, error) {
	var result Account
	_, _, err := s.cfg.Accounts.Update(ctx, accountID, func(current Account, found bool) (Account, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: identity maps game %s to account %s, which does not exist", ErrConflict, game, accountID)
		}
		if current.Identity != key || current.Game != game {
			return current, false, fmt.Errorf("%w: account %s does not belong to this identity in game %s", ErrConflict, accountID, game)
		}
		current.LastLoginAtUnix = now.Unix()
		result = current
		return current, true, nil
	})
	return result, err
}

// accountInGoodStanding 读账号与它所属的身份记录，核对账号存在、身份没有被封。票据、游戏账号 token、建角、进服
// 都经它：封号在身份级，对已经签出去的票据当场生效。
func (s *Service) accountInGoodStanding(ctx context.Context, accountID string) (Account, IdentityRecord, error) {
	account, found, err := s.cfg.Accounts.Get(ctx, accountID)
	if err != nil {
		return Account{}, IdentityRecord{}, err
	}
	if !found {
		return Account{}, IdentityRecord{}, fmt.Errorf("%w: %s", ErrAccountMissing, accountID)
	}
	identity, err := s.identityOf(ctx, account.Value)
	if err != nil {
		return Account{}, IdentityRecord{}, err
	}
	if identity.Banned {
		return Account{}, IdentityRecord{}, fmt.Errorf("%w: identity is banned", ErrIdentityDenied)
	}
	return account.Value, identity, nil
}

// identityOf 读账号所属的身份记录。账号记录不带身份键、或身份记录不在，是存储被手工改过：按账号不在回答
// （ErrAccountMissing），不放行一个查不到封号状态的账号。
func (s *Service) identityOf(ctx context.Context, account Account) (IdentityRecord, error) {
	if account.Identity == "" {
		return IdentityRecord{}, fmt.Errorf("%w: account %s names no identity", ErrAccountMissing, account.ID)
	}
	identity, found, err := s.cfg.Identities.Get(ctx, account.Identity)
	if err != nil {
		return IdentityRecord{}, err
	}
	if !found {
		return IdentityRecord{}, fmt.Errorf("%w: the identity of account %s is missing", ErrAccountMissing, account.ID)
	}
	return identity.Value.clone(), nil
}

// updateIdentityOf 由账号号找到身份记录，在它上面跑 mutate（同一次比较后写入）。账号或身份记录不在答 ErrAccountMissing。
func (s *Service) updateIdentityOf(ctx context.Context, accountID string, mutate func(IdentityRecord) IdentityRecord) (IdentityRecord, error) {
	if strings.TrimSpace(accountID) == "" {
		return IdentityRecord{}, fmt.Errorf("%w: account id is empty", ErrAccountMissing)
	}
	account, found, err := s.cfg.Accounts.Get(ctx, accountID)
	if err != nil {
		return IdentityRecord{}, err
	}
	if !found || account.Value.Identity == "" {
		return IdentityRecord{}, fmt.Errorf("%w: %s", ErrAccountMissing, accountID)
	}
	written, _, err := s.cfg.Identities.Update(ctx, account.Value.Identity, func(current IdentityRecord, found bool) (IdentityRecord, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: the identity of account %s is missing", ErrAccountMissing, accountID)
		}
		return mutate(current), true, nil
	})
	if err != nil {
		return IdentityRecord{}, err
	}
	return written.Value.clone(), nil
}

// SetIdentityBanned 封或解封 accountID 所属的人（Admin，维护者 2026-11-18）：封号在身份级，他在所有游戏的账号一起
// 登录不了，已经签出去的账号票据、进服票据、游戏账号 token 当场失效（校验时都读身份记录），建不了角；已签发的角色
// 会话令牌不受影响。按游戏封号以后再加。note 必填，记在身份记录上（系统时间）。返回身份记录：这是运维面，
// 不在总线上，客户端与游戏代码拿不到。账号不在答 ErrAccountMissing。
func (s *Service) SetIdentityBanned(ctx context.Context, accountID string, banned bool, note string) (IdentityRecord, error) {
	note, err := validateAdminNote(note)
	if err != nil {
		return IdentityRecord{}, err
	}
	stamp := s.cfg.SystemNow().Unix()
	record, err := s.updateIdentityOf(ctx, accountID, func(current IdentityRecord) IdentityRecord {
		current.Banned = banned
		current.AdminNote, current.AdminActionAtUnix = note, stamp
		return current
	})
	if err != nil {
		return IdentityRecord{}, err
	}
	s.report.Accepted("set_identity_banned")
	return record, nil
}
