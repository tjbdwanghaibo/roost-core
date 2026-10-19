package versionstore

// 多键原子更新：一个事实横跨几个版本化值时（例如 service/match 一局的票分属几个分区队列），
// 读、决定、写必须对这几个值一起成立，否则会出现“一个分区已成局、另一个分区的票还在等待”。
// Update 只覆盖一个键，这里把同一个比较并设置契约推广到多个键：一次读全部键，mutate 一次看到全部当前值，
// 写入时所有键都仍是读到的版本才全部生效，任何一个键被别人改过就整体重试。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

// MutateAll 是多键版的 Mutate：current[i] / found[i] 是 keys[i] 的当前值与是否存在，返回的 next 必须与 keys
// 一一对应。和 Mutate 一样可能被调用多次，必须是参数的纯函数；save == false 时一个键也不写。
type MutateAll[T any] func(current []T, found []bool) (next []T, save bool, err error)

// MultiUpdater 由支持多键原子更新的 Store 实现（MemoryStore、RedisStore）。
type MultiUpdater[K comparable, T any] interface {
	// UpdateAll 在一次比较并设置里更新 keys：save 时所有键都写成 next 里各自的值（版本各自加 1，
	// 不存在的键被创建），且只有每个键都仍是读到的版本时才写；任何一个键在读与写之间被改过，整次放弃并
	// 重新读、重新调用 mutate，预算与 Update 相同，用尽返回 ErrConflict。
	//
	// keys 不能为空、不能重复。返回值与 keys 一一对应；applied 为 false 表示 mutate 拒绝保存，此时返回
	// 读到的当前值。
	//
	// 与 Update 的差别：
	//   - Redis 实现要求全部键在同一个 hash slot（Redis Cluster 下用同一个 hash tag），否则服务端拒绝脚本。
	//   - 不支持 RedisConfig.Index；配置了索引的 store 调用会报错。
	//   - 回复丢失时同样用每个键信封里的写令牌判断这次写是否已生效；判断不出时返回 *UnknownOutcomeError，
	//     但它不能交给 Resume——重试会对当时的值重新调用 mutate，所以 mutate 需要自己幂等（典型做法是
	//     值里记着这次请求的结果，service/match 的跨分区成局就是同一票集合返回原比赛）。
	UpdateAll(ctx context.Context, keys []K, mutate MutateAll[T]) (results []Versioned[T], applied bool, err error)
}

// UpdateAll 在一把锁里读、调用 mutate、写回全部键，所以比较与写入天然原子，从不进入重试。
func (s *MemoryStore[K, T]) UpdateAll(_ context.Context, keys []K, mutate MutateAll[T]) ([]Versioned[T], bool, error) {
	if err := checkUpdateAll(keys, mutate); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := make([]Versioned[T], len(keys))
	values := make([]T, len(keys))
	found := make([]bool, len(keys))
	for i, key := range keys {
		current[i], found[i] = s.items[key]
		values[i] = current[i].Value
	}
	next, save, err := mutate(values, found)
	if err != nil {
		return nil, false, err
	}
	if !save {
		return current, false, nil
	}
	if len(next) != len(keys) {
		return nil, false, fmt.Errorf("versionstore: mutate returned %d values for %d keys", len(next), len(keys))
	}
	stored := make([]Versioned[T], len(keys))
	for i, key := range keys {
		stored[i] = Versioned[T]{Value: next[i], Version: current[i].Version + 1}
		s.items[key] = stored[i]
	}
	return stored, true, nil
}

func checkUpdateAll[K comparable, T any](keys []K, mutate MutateAll[T]) error {
	if mutate == nil {
		return fmt.Errorf("versionstore: mutate is nil")
	}
	if len(keys) == 0 {
		return fmt.Errorf("versionstore: UpdateAll needs at least one key")
	}
	seen := make(map[K]bool, len(keys))
	for _, key := range keys {
		if seen[key] {
			return fmt.Errorf("versionstore: UpdateAll key %v appears twice", key)
		}
		seen[key] = true
	}
	return nil
}

// UpdateAll 读全部键，调用 mutate，再用 fredis.CompareAndSetAll 一次写入：脚本逐键比较读到的整条信封字节，
// 全部相同才全部写入。每个键的新信封各带一个写令牌，回复丢失时按令牌判断（settleAll）。
func (s *RedisStore[K, T]) UpdateAll(ctx context.Context, keys []K, mutate MutateAll[T]) ([]Versioned[T], bool, error) {
	if err := checkUpdateAll(keys, mutate); err != nil {
		return nil, false, err
	}
	if s.cfg.Index != nil {
		return nil, false, fmt.Errorf("versionstore: UpdateAll does not maintain an index; this store has one")
	}
	redisKeys := make([]string, len(keys))
	for i, key := range keys {
		redisKey, err := s.key(key)
		if err != nil {
			return nil, false, err
		}
		redisKeys[i] = redisKey
	}
	read := func() ([][]byte, []Versioned[T], []bool, error) {
		raws := make([][]byte, len(keys))
		current := make([]Versioned[T], len(keys))
		found := make([]bool, len(keys))
		for i, redisKey := range redisKeys {
			raw, value, ok, err := s.readRaw(ctx, redisKey)
			if err != nil {
				return nil, nil, nil, err
			}
			raws[i], current[i], found[i] = raw, value, ok
		}
		return raws, current, found, nil
	}

	remaining := s.cfg.MaxAttempts
	raws, current, found, err := read()
	if err != nil {
		return nil, false, err
	}
	for attempt := 0; remaining > 0; attempt++ {
		values := make([]T, len(keys))
		for i := range current {
			values[i] = current[i].Value
		}
		next, save, err := mutate(values, found)
		if err != nil {
			return nil, false, err
		}
		if !save {
			return current, false, nil
		}
		if len(next) != len(keys) {
			return nil, false, fmt.Errorf("versionstore: mutate returned %d values for %d keys", len(next), len(keys))
		}
		writes := make([]*pendingWrite, len(keys))
		for i, key := range keys {
			var base []byte
			if found[i] {
				base = raws[i]
			}
			if writes[i], err = s.prepareWrite(writeUpdate, key, redisKeys[i], base, next[i], current[i].Version+1); err != nil {
				return nil, false, err
			}
		}
		result, err := s.settleAll(ctx, writes, &remaining)
		if result == outcomeFailed {
			return nil, false, err
		}
		CountCompareAndSet(s.cfg.Prefix, result == outcomeApplied)
		if result == outcomeApplied {
			stored := make([]Versioned[T], len(keys))
			for i := range keys {
				stored[i] = Versioned[T]{Value: next[i], Version: writes[i].version}
			}
			return stored, true, nil
		}
		if remaining == 0 {
			break
		}
		s.backoff(attempt)
		if raws, current, found, err = read(); err != nil {
			return nil, false, err
		}
	}
	CountConflict(s.cfg.Prefix)
	return nil, false, fmt.Errorf("%w: %s after %d attempts", ErrConflict, strings.Join(redisKeys, ", "), s.cfg.MaxAttempts)
}

// settleAll 是 settleWithBudget 的多键版：发送一次多键比较并设置；回复丢失时逐键读当前值、逐键 judge。
//
// 脚本全有或全无，所以任何一个键的结论就是整次写的结论：
//   - 某个键上认出了自己的令牌（verdictApplied）：脚本执行过，全部键都已写入；
//   - 某个键从基准演进、但那个版本不是自己的令牌（verdictLost）：别人的写占了那个版本，这次脚本没执行、也不会再成功；
//   - 某个键仍是基准字节（verdictNotRun）：脚本没执行（执行过的话每个键都换了带新令牌的信封），原样重发；
//   - 其余（键不存在、令牌已被挤出历史）证明不了，返回 *UnknownOutcomeError（不能交给 Resume，见 UpdateAll）。
func (s *RedisStore[K, T]) settleAll(ctx context.Context, writes []*pendingWrite, remaining *int) (outcome, error) {
	cmds := make([]fredis.CompareAndSetCommand, len(writes))
	keys := make([]string, len(writes))
	for i, write := range writes {
		cmds[i] = fredis.CompareAndSetCommand{Key: write.key, Expected: write.base, Next: write.next, TTL: s.cfg.TTL}
		keys[i] = write.key
	}
	ambiguous := false
	var transportErr error
	resolved := func(result string, o outcome) (outcome, error) {
		if ambiguous {
			countUnknownOutcome(s.cfg.Prefix, result)
		}
		return o, nil
	}
	unresolved := func(err error) (outcome, error) {
		countUnknownOutcome(s.cfg.Prefix, "unresolved")
		if transportErr != nil {
			err = transportErr
		}
		return outcomeFailed, &UnknownOutcomeError{Key: strings.Join(keys, ", "), Token: writes[0].token, Err: err}
	}
	for sends := 0; ; {
		if ambiguous {
			switch verdict, err := s.judgeAll(ctx, writes); {
			case err != nil:
				return unresolved(err)
			case verdict == verdictApplied:
				return resolved("applied", outcomeApplied)
			case verdict == verdictLost:
				return resolved("lost", outcomeLost)
			case verdict == verdictUnproven:
				return unresolved(errors.New("the keys' history does not show this write either way"))
			}
			// verdictNotRun: send it again.
			if *remaining <= 0 {
				return unresolved(errors.New("the command has not run and the resend budget is spent"))
			}
			if err := ctx.Err(); err != nil {
				return unresolved(err)
			}
			s.backoff(sends)
		}
		sends++
		(*remaining)--
		applied, err := fredis.CompareAndSetAll(ctx, s.client, cmds)
		switch {
		case err == nil && applied:
			return resolved("applied", outcomeApplied)
		case err == nil && !ambiguous:
			return outcomeLost, nil
		case err == nil:
			// A resend lost its compare; the earlier send may have changed the keys.
		case replyNotLost(err) && !ambiguous:
			return outcomeFailed, err
		case replyNotLost(err):
		default:
			transportErr = err
		}
		ambiguous = true
	}
}

// judgeAll 读每个键并按 settleAll 注释里的规则合并逐键结论。
func (s *RedisStore[K, T]) judgeAll(ctx context.Context, writes []*pendingWrite) (verdict, error) {
	verdicts := make([]verdict, len(writes))
	for i, write := range writes {
		current, err := s.rawOrNil(ctx, write.key)
		if err != nil {
			return verdictUnproven, err
		}
		verdicts[i] = judge(write, current)
	}
	for _, want := range []verdict{verdictApplied, verdictLost, verdictNotRun} {
		for _, got := range verdicts {
			if got == want {
				return want, nil
			}
		}
	}
	return verdictUnproven, nil
}

var (
	_ MultiUpdater[string, int] = (*MemoryStore[string, int])(nil)
	_ MultiUpdater[string, int] = (*RedisStore[string, int])(nil)
)
