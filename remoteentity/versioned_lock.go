package remoteentity

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/metrics"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

var (
	ErrVersionedLockNotAcquired = errors.New("versioned lock not acquired")
	ErrVersionedLockNotOwned    = errors.New("versioned lock not owned")
	ErrVersionedLockExpired     = errors.New("versioned lock expired")
	ErrVersionedLockConfig      = errors.New("versioned lock invalid configuration")
)

// versionedLock implements fredis.IVersionedLock.
type versionedLock struct {
	redis     fredis.IRedis
	key       string
	token     string
	ttl       time.Duration
	opts      fredis.VersionedLockOptions
	initErr   error
	id        int64
	authority WriteAuthority

	mu       sync.Mutex
	acquired bool
	version  int64
	fence    uint64
	grant    WriteGrant

	// 取锁 token 按锁对象分代（RR-20261004-01）：token = tokenPrefix + 十进制 tokenSeq。tokenPrefix 在创建锁对象时随机生成、
	// 只有这个锁对象会铸造带它的 token；tokenSeq 每次 TryLock 加一（l.mu 下），所以同一锁对象的 token 有先后。
	// 本地不持有（acquired=false）时，Redis 上 owner 若是本锁对象较早一代的 token，它一定是"结果未知"留下的：
	// 取锁脚本执行了但回复丢失或迟到（RR-20261004-01）、释放没有得到明确答复（RR-20260930-21）、或放弃未准入许可的清理失败。
	// TryLock 把 tokenPrefix 与本次序号交给 Lua，由 Redis 判定：owner 是本锁对象更早的序号就视为可取得（新 token、新 fence、
	// 新代际），被别人持有走 NotAcquired，空闲正常取锁。只认"更早"，迟到落地的旧代脚本挤不掉已经取得的新代际（RR-20260924-20）。
	// 不在本地记"哪几个 token 结果未知"：连续多次没有答复时每一个都可能是 owner，只记一个会取不回真正的 owner，全记又无界。
	tokenPrefix string
	tokenSeq    uint64

	// incarnation 非 nil 时 tokenPrefix 以它的 self 开头，取锁时可以接管同一单实例锁持有者上一代进程留下的
	// owner（O-M6-6，见 lockIncarnation）；nil 时不接管，旧格式 token，行为与之前相同。
	incarnation *lockIncarnation

	// 异步续期按锁代际绑定（RR-20260926-43）。touchGeneration 是当前登记的续期 goroutine
	// 所服务的 token，空表示没有登记。旧代际 goroutine 只续期、只判失效自己的 token，退出时
	// 也只清除仍属于自己的登记；新代际 TryLock 时取消旧 goroutine 并为新 token 启动续期，
	// 不依赖旧 goroutine 先退出。touchWg 覆盖全部尚未退出的续期 goroutine。
	touchMu         sync.Mutex
	touchGeneration string
	touchCancel     context.CancelFunc
	touchWg         sync.WaitGroup
}

var _ fredis.IVersionedLock = (*versionedLock)(nil)

func newVersionedLock(redis fredis.IRedis, id int64, opts fredis.VersionedLockOptions) *versionedLock {
	key := "lock:" + opts.Key + ":" + strconv.FormatInt(id, 10)

	l := &versionedLock{
		redis:       redis,
		id:          id,
		key:         key,
		ttl:         opts.TTL,
		opts:        opts,
		tokenPrefix: generateToken() + ".",
	}
	if redis == nil {
		l.initErr = fmt.Errorf("%w: redis client is nil", ErrVersionedLockConfig)
	}
	if opts.TTL < time.Millisecond {
		l.initErr = errors.Join(l.initErr, fmt.Errorf("%w: TTL must be at least 1ms", ErrVersionedLockConfig))
	}
	if opts.RetryCount < 0 {
		l.opts.RetryCount = 0
	}
	if opts.RetryInterval < 0 {
		l.opts.RetryInterval = 0
	}

	// Normalize async touch params
	if opts.AutoAsyncTouch {
		if opts.AsyncTouchExtend <= 0 {
			opts.AsyncTouchExtend = opts.TTL / 2
		}
		if opts.AsyncTouchInterval <= 0 {
			opts.AsyncTouchInterval = opts.TTL / 3
		}
		if opts.AsyncTouchExtend < time.Millisecond || opts.AsyncTouchInterval < time.Millisecond {
			l.initErr = errors.Join(l.initErr, fmt.Errorf("%w: async touch durations must be at least 1ms", ErrVersionedLockConfig))
		}
		l.opts = opts
	}
	if l.opts.RetryCount < 0 {
		l.opts.RetryCount = 0
	}
	if l.opts.RetryInterval < 0 {
		l.opts.RetryInterval = 0
	}

	return l
}

func (l *versionedLock) TryLock(ctx context.Context) error {
	if err := l.validationError(); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.acquired {
		return fmt.Errorf("versioned lock already acquired")
	}

	ttlMs := l.ttl.Milliseconds()
	l.tokenSeq++
	seq := l.tokenSeq
	token := l.tokenPrefix + strconv.FormatUint(seq, 10)
	// 本锁对象更早一代留在 Redis 上的租约交给 Lua 以 Redis 为准取回（RR-20261004-01 / RR-20260930-21）。
	// Eval 出错时脚本可能已经执行（owner 已是 token）：本地不记录，下一次 TryLock 的序号更大，同一条判定会把它取回。
	// 同一单实例锁持有者上一代进程留下的 owner 也交给 Lua 当场接管（O-M6-6）；没有代际时两项为空，不做这项判定。
	scope, self := l.incarnation.prefixes()
	result, err := l.redis.Eval(ctx, versionedTryLockLua, []string{l.key, l.key + ":fence"}, token, ttlMs, l.tokenPrefix, seq, scope, self)
	if err != nil {
		return fmt.Errorf("versioned lock redis error: %w", err)
	}

	vals, err := toInt64Slice(result)
	if err != nil {
		return fmt.Errorf("versioned lock parse error: %w", err)
	}
	if len(vals) < 3 || vals[0] == 0 {
		return ErrVersionedLockNotAcquired
	}
	if len(vals) > 3 && vals[3] == 1 {
		l.incarnation.noteTakeover(l.id)
	}

	if l.authority != nil {
		grant, grantErr := l.authority.GrantWrite(ctx, l.id, token, 0)
		if grantErr == nil {
			refreshed, refreshErr := l.redis.Eval(ctx, versionedRefreshLua, []string{l.key}, token, ttlMs)
			owned, parseErr := toInt(refreshed)
			grantErr = errors.Join(refreshErr, parseErr)
			if grantErr == nil && owned == 0 {
				// Mongo 已确认许可，但业务尚未准入且 Redis 租约已失效。
				// 这是确定的竞争失败，可以用新 token 重新争锁；未知 grant 结果不走此分支。
				grantErr = errors.Join(ErrVersionedLockNotAcquired, ErrVersionedLockExpired)
			}
		}
		if grantErr != nil {
			// 尚未向业务发许可；只清理本 token，版本未知时不回写 Redis 版本缓存。
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			_, _ = l.redis.Eval(cleanup, versionedAbandonLua, []string{l.key}, token)
			cancel()
			return grantErr
		}
		vals[1], vals[2] = grant.Version, int64(grant.Fence)
		l.grant = grant
	}
	l.acquired = true
	l.token = token
	l.version = vals[1]
	l.fence = uint64(vals[2])

	if l.opts.AutoAsyncTouch {
		l.startAsyncTouchLocked(token)
	}

	return nil
}

func (l *versionedLock) Lock(ctx context.Context) error {
	if err := l.validationError(); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	retryCount := l.opts.RetryCount
	retryInterval := l.opts.RetryInterval

	var lastErr error
	for i := 0; i <= retryCount; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		err := l.TryLock(ctx)
		if err == nil {
			return nil
		}
		lastErr = err

		if !errors.Is(err, ErrVersionedLockNotAcquired) {
			return err // non-retryable error
		}

		if i < retryCount && retryInterval > 0 {
			backoff := boundedBackoff(retryInterval, i)
			jitter := time.Duration(float64(backoff) * (0.5 + rand.Float64()*0.5))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(jitter):
			}
		}
	}
	return lastErr
}

func (l *versionedLock) Unlock(ctx context.Context, newVersion int64, versionTTL time.Duration) error {
	if err := l.validationError(); err != nil {
		return err
	}
	return l.UnlockWithRetry(ctx, newVersion, versionTTL, l.opts.RetryCount, l.opts.RetryInterval)
}

func (l *versionedLock) UnlockWithRetry(ctx context.Context, newVersion int64, versionTTL time.Duration, retryCount int, retryInterval time.Duration) error {
	if err := l.validationError(); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	l.stopAsyncTouch()

	l.mu.Lock()
	if !l.acquired {
		l.mu.Unlock()
		return ErrVersionedLockNotOwned
	}
	token := l.token
	l.mu.Unlock()

	if versionTTL <= 0 {
		versionTTL = l.ttl
	}
	if retryCount < 0 {
		retryCount = 0
	}

	verTTLMs := versionTTL.Milliseconds()
	unlockID := generateToken()
	var lastErr error
	for i := 0; i <= retryCount; i++ {
		select {
		case <-ctx.Done():
			l.releaseOutcomeUnknown(token)
			return ctx.Err()
		default:
		}

		result, err := l.redis.Eval(ctx, versionedUnlockLua, []string{l.key}, token, unlockID, newVersion, verTTLMs)
		if err != nil {
			lastErr = fmt.Errorf("versioned lock unlock error: %w", err)
		} else {
			ok, pErr := toInt(result)
			if pErr != nil {
				lastErr = fmt.Errorf("versioned lock unlock parse: %w", pErr)
			} else if ok == 0 {
				l.mu.Lock()
				if l.token == token {
					l.acquired = false
				}
				l.mu.Unlock()
				return ErrVersionedLockNotOwned
			} else {
				// 1 = unlocked now; 2 = a previous attempt whose response was
				// lost already landed. Both mean the unlock succeeded.
				l.mu.Lock()
				// Redis 已按 token 隔离代际；迟到的回复也只能更新同一代本地状态。
				if l.token == token {
					l.acquired = false
					l.version = newVersion
				}
				l.mu.Unlock()
				return nil
			}
		}

		if i < retryCount && retryInterval > 0 {
			backoff := boundedBackoff(retryInterval, i)
			jitter := time.Duration(float64(backoff) * (0.5 + rand.Float64()*0.5))
			select {
			case <-ctx.Done():
				l.releaseOutcomeUnknown(token)
				return ctx.Err()
			case <-time.After(jitter):
			}
		}
	}
	// Redis 错误用尽重试：没有明确答复，Redis 上 owner 可能仍是 token。本地进入持有状态未知，
	// 下一次 TryLock 以 Redis 为准（RR-20260930-21）；这里既不能伪造已释放，也不能保持 acquired。
	l.releaseOutcomeUnknown(token)
	return lastErr
}

// releaseOutcomeUnknown 记录一次没有拿到 Redis 明确答复的释放（RR-20260930-21）：只对仍是当前代际的 token 生效
// （Touch / Refresh 已看到失效的代际不再改），本地不再算持有。Redis 上 owner 可能仍是 token，下一次 TryLock 按
// 分代规则（tokenPrefix / tokenSeq，RR-20261004-01）交 Lua 裁决。
// 调用时续期 goroutine 已由 stopAsyncTouch 停掉并等待退出，不会再有人为这个 token 续期。
func (l *versionedLock) releaseOutcomeUnknown(token string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.acquired || l.token != token {
		return
	}
	l.acquired = false
}

// writeGrant 返回当前锁代际已确认的许可；失效或 Redis-only 锁不提供持久证明。
func (l *versionedLock) writeGrant() (WriteGrant, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.grant, l.acquired && l.authority != nil
}

func (l *versionedLock) Version() int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.version
}

func (l *versionedLock) Fence() uint64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fence
}

func (l *versionedLock) IsAcquired() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.acquired
}

func (l *versionedLock) Touch(ctx context.Context, duration time.Duration) error {
	if err := l.validationError(); err != nil {
		return err
	}
	if duration < time.Millisecond {
		return fmt.Errorf("%w: touch duration must be at least 1ms", ErrVersionedLockConfig)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	l.mu.Lock()
	if !l.acquired {
		l.mu.Unlock()
		return ErrVersionedLockExpired
	}
	token := l.token
	l.mu.Unlock()
	return l.touchLease(ctx, token, duration)
}

// holdsGeneration 报告 token 所代表的代际是否仍是本地持有的当前代际。
func (l *versionedLock) holdsGeneration(token string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.acquired && l.token == token
}

// touchLease 只为 token 这一代际续期：代际已被替换或已失效时直接返回过期，
// 迟到的 -1 回复也只能把同一代际标记为失效。
func (l *versionedLock) touchLease(ctx context.Context, token string, duration time.Duration) error {
	if !l.holdsGeneration(token) {
		return ErrVersionedLockExpired
	}
	addMs := duration.Milliseconds()
	maxTTLMs := (2 * l.ttl).Milliseconds()
	result, err := l.redis.Eval(ctx, versionedTouchLua, []string{l.key}, token, addMs, maxTTLMs)
	if err != nil {
		return fmt.Errorf("versioned lock touch error: %w", err)
	}

	val, err := toInt64(result)
	if err != nil {
		return fmt.Errorf("versioned lock touch parse: %w", err)
	}
	if val == -1 {
		l.mu.Lock()
		if l.token == token {
			l.acquired = false
		}
		l.mu.Unlock()
		return ErrVersionedLockExpired
	}
	return nil
}

func (l *versionedLock) Refresh(ctx context.Context) error {
	if err := l.validationError(); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	l.mu.Lock()
	if !l.acquired {
		l.mu.Unlock()
		return ErrVersionedLockExpired
	}
	token := l.token
	l.mu.Unlock()

	ttlMs := l.ttl.Milliseconds()
	result, err := l.redis.Eval(ctx, versionedRefreshLua, []string{l.key}, token, ttlMs)
	if err != nil {
		return fmt.Errorf("versioned lock refresh error: %w", err)
	}
	ok, err := toInt(result)
	if err != nil {
		return fmt.Errorf("versioned lock refresh parse: %w", err)
	}
	if ok == 0 {
		l.mu.Lock()
		if l.token == token {
			l.acquired = false
		}
		l.mu.Unlock()
		return ErrVersionedLockExpired
	}
	return nil
}

func (l *versionedLock) Close() error {
	if err := l.validationError(); err != nil {
		return err
	}
	l.stopAsyncTouch()

	l.mu.Lock()
	acquired := l.acquired
	ver := l.version
	l.mu.Unlock()

	if acquired {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := l.Unlock(ctx, ver, 0)
		cancel()
		return err
	}
	return nil
}

// --- Async Touch ---

// startAsyncTouchLocked 为刚取得的 token 代际启动续期；调用方持有 l.mu。
// 上一代际的 goroutine 可能已判定失效但尚未退出：这里只取消它，不在 l.mu 下等待
// （它的 touchLease 需要 l.mu）。它不会再续期或清除新代际的登记，并在一个续期间隔内退出，
// 因此每个锁对象同时至多有一个服务当前代际的 goroutine，外加正在退出的旧 goroutine。
func (l *versionedLock) startAsyncTouchLocked(token string) {
	l.touchMu.Lock()
	defer l.touchMu.Unlock()
	if l.touchGeneration == token {
		return
	}
	if l.touchCancel != nil {
		l.touchCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.touchGeneration = token
	l.touchCancel = cancel
	l.touchWg.Add(1)
	go l.runAsyncTouch(ctx, cancel, token, l.opts.AsyncTouchExtend, l.opts.AsyncTouchInterval)
}

// stopAsyncTouch 取消当前代际的续期并等待全部续期 goroutine（含正在退出的旧代际）结束。
func (l *versionedLock) stopAsyncTouch() {
	l.touchMu.Lock()
	cancel := l.touchCancel
	l.touchMu.Unlock()
	if cancel != nil {
		cancel()
	}
	l.touchWg.Wait()
}

func (l *versionedLock) runAsyncTouch(ctx context.Context, cancel context.CancelFunc, token string, extend, interval time.Duration) {
	defer l.touchWg.Done()
	defer func() {
		cancel()
		l.touchMu.Lock()
		// 只清除仍属于本代际的登记；新代际已登记时保持不动。
		if l.touchGeneration == token {
			l.touchGeneration = ""
			l.touchCancel = nil
		}
		l.touchMu.Unlock()
	}()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			evalCtx, evalCancel := context.WithTimeout(ctx, interval)
			err := l.touchLease(evalCtx, token, extend)
			evalCancel()
			if err != nil {
				if errors.Is(err, ErrVersionedLockExpired) || !l.holdsGeneration(token) {
					return
				}
			}
		}
	}
}

// --- 进程代际（O-M6-6） ---

// ProcessIncarnation 是“同一个 sid 同一时刻只有本进程在跑”的证明，来自 App 单实例锁
// （docs/feature/APP-SINGLETON-LOCK-2026-10-05.md）：Holder 是单实例锁的键（<key_prefix>:<server_type>:<sid>），
// Token 是本次启动的持锁令牌（锁值第一段，每次启动都不同）。
//
// 传给 Assemble 之后，Remote 实体共享锁的 token 带上 sid、Holder 摘要与 Token；新进程第一次取某个实体的锁时，
// owner 若是同一 Holder、不同 Token 留下的（上一代进程），在取锁脚本里当场接管，不再等 lock_ttl
// （docs/feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md §7）。只有持有单实例锁的进程才能传：没有“旧进程已死
// 或已失锁 fail-stop”的保证时接管会打断仍在服务的进程，所以 singleton.enabled=false 时不传，照旧按 TTL 等待。
type ProcessIncarnation struct {
	Holder string
	Token  string
}

// lockIncarnation 是取锁 token 里的进程代际前缀。新格式 token：
//
//	~<sid>~<Holder 的 SHA-256 前 16 位十六进制>~<Token>~<锁对象随机串>.<序号>
//
// scope = "~<sid>~<摘要>~" 是同一个单实例锁持有者（同 sid、同服务类型、同项目前缀）的全部进程；self = scope +
// "<Token>~" 是本进程这一代。旧格式 token 以 crand.Text 的 base32 大写字母开头，不会以 "~" 开头，永远不在
// 任何 scope 内。Token 只允许字母、数字、'-'、'_'，不会含分隔符，前缀判定没有歧义。
type lockIncarnation struct {
	scope, self string
	logOnce     sync.Once
}

// newLockIncarnation 校验并构造 sid 的进程代际。
func newLockIncarnation(sid int32, incarnation ProcessIncarnation) (*lockIncarnation, error) {
	if sid == 0 {
		return nil, fmt.Errorf("%w: incarnation needs a non-zero sid", ErrVersionedLockConfig)
	}
	if incarnation.Holder == "" {
		return nil, fmt.Errorf("%w: incarnation holder is empty", ErrVersionedLockConfig)
	}
	if len(incarnation.Token) == 0 || len(incarnation.Token) > 64 {
		return nil, fmt.Errorf("%w: incarnation token must be 1..64 characters", ErrVersionedLockConfig)
	}
	for _, c := range incarnation.Token {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' || c == '_') {
			return nil, fmt.Errorf("%w: incarnation token %q may only contain letters, digits, '-' and '_'", ErrVersionedLockConfig, incarnation.Token)
		}
	}
	digest := sha256.Sum256([]byte(incarnation.Holder))
	scope := "~" + strconv.FormatInt(int64(sid), 10) + "~" + hex.EncodeToString(digest[:8]) + "~"
	return &lockIncarnation{scope: scope, self: scope + incarnation.Token + "~"}, nil
}

// prefixes 返回交给取锁脚本的范围前缀与本代前缀；nil 时都为空（不接管）。
func (i *lockIncarnation) prefixes() (scope, self string) {
	if i == nil {
		return "", ""
	}
	return i.scope, i.self
}

// noteTakeover 记录一次接管：计数每次都加，日志每个 Assembly 只在第一次记（重启后每个被写的实体各接管一次）。
func (i *lockIncarnation) noteTakeover(id int64) {
	metrics.IncCounter("remote_entity.lock_takeover_total", nil, 1)
	if i == nil {
		return
	}
	i.logOnce.Do(func() {
		slog.Info("remote_entity: took over a shared lock left by the previous process of this sid (singleton lock held); later takeovers are counted in remote_entity.lock_takeover_total",
			"entity", id, "scope", i.scope)
	})
}

// --- Versioned Lock Factory ---

type versionedLockFactory struct {
	redis       fredis.IRedis
	authority   WriteAuthority
	incarnation *lockIncarnation
}

var _ fredis.IVersionedLockFactory = (*versionedLockFactory)(nil)

// NewVersionedLockFactory 无 authority 的兼容模式仅提供 Redis 协调，不保证故障切换 fencing。
func NewVersionedLockFactory(redis fredis.IRedis, authorities ...WriteAuthority) *versionedLockFactory {
	f := &versionedLockFactory{redis: redis}
	if len(authorities) > 0 {
		f.authority = authorities[0]
	}
	return f
}

func (f *versionedLockFactory) NewVersionedLock(id int64, opts fredis.VersionedLockOptions) fredis.IVersionedLock {
	if f == nil {
		return newVersionedLock(nil, id, opts)
	}
	lock := newVersionedLock(f.redis, id, opts)
	lock.authority = f.authority
	if f.incarnation != nil {
		// 代际前缀放在锁对象前缀之前：RR-20261004-01 的“本锁对象更早序号”判定照旧以完整 tokenPrefix 为准。
		lock.incarnation = f.incarnation
		lock.tokenPrefix = f.incarnation.self + lock.tokenPrefix
	}
	return lock
}

// --- Helpers ---

func generateToken() string {
	return crand.Text()
}

func (l *versionedLock) validationError() error {
	if l == nil {
		return fmt.Errorf("%w: lock is nil", ErrVersionedLockConfig)
	}
	return l.initErr
}

func boundedBackoff(base time.Duration, attempt int) time.Duration {
	const maxBackoff = 30 * time.Second
	if base <= 0 {
		return 0
	}
	if attempt > 30 {
		attempt = 30
	}
	if base > maxBackoff/time.Duration(1<<attempt) {
		return maxBackoff
	}
	backoff := base * time.Duration(1<<attempt)
	if backoff > maxBackoff {
		return maxBackoff
	}
	return backoff
}

func toInt64Slice(v any) ([]int64, error) {
	switch val := v.(type) {
	case []interface{}:
		result := make([]int64, len(val))
		for i, item := range val {
			n, err := toInt64(item)
			if err != nil {
				return nil, err
			}
			result[i] = n
		}
		return result, nil
	case []int64:
		return val, nil
	default:
		return nil, fmt.Errorf("unexpected type %T for int64 slice", v)
	}
}

func toInt64(v any) (int64, error) {
	switch val := v.(type) {
	case int64:
		return val, nil
	case int:
		return int64(val), nil
	case float64:
		return int64(val), nil
	case string:
		return strconv.ParseInt(val, 10, 64)
	default:
		return 0, fmt.Errorf("unexpected type %T for int64", v)
	}
}

func toInt(v any) (int, error) {
	n, err := toInt64(v)
	return int(n), err
}

// 放弃未准入的许可时不修改版本缓存，避免用未知/过期版本覆盖已提交版本。
const versionedAbandonLua = `
if redis.call("HGET", KEYS[1], "owner") == ARGV[1] then
 return redis.call("HDEL", KEYS[1], "owner")
end
return 0
`
