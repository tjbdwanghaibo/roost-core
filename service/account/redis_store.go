package account

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"

	"github.com/tjbdwanghaibo/roost-core/service/directory"
)

// RedisStores are the four stores this package needs, over Redis, plus the
// name directory.
//
// There is no storage logic here. Accounts, roles, servers and slots are all
// versioned state, and versionstore.RedisStore already implements that
// contract. What this contributes is the part that must not be each caller's
// to invent: four key namespaces under one prefix that must not collide, and
// the TTL decisions below, each of which has a wrong answer that looks fine.
type RedisStores struct {
	Accounts versionstore.Store[string, Account]
	// Identities 是身份级记录（<前缀>:ident:<身份键>，identity_accounts.go）：与账号记录一样是持久的，不带 TTL。
	Identities versionstore.Store[string, IdentityRecord]
	// AccountIDs 是账号号计数器（<前缀>:account_id 上的 INCR）：与账号数据同生共死，备份、迁移、按前缀清数据时算在内。
	AccountIDs *RedisAccountIDs
	Roles      versionstore.Store[int64, Role]
	Servers    versionstore.Store[int32, GameServer]
	Slots      versionstore.Store[string, Slot]
	Names      directory.Directory
	// Presence 是区服入口的登记表（redis_presence.go）：易失、带过期，与上面四份不同，过期正是它的用途。
	Presence PresenceStore
}

// NewRedisStores builds them.
//
// None of the four carries a key TTL, and each for a reason worth stating,
// because "add a TTL, it is only a cache" is how this kind of state gets
// quietly lost:
//
//   - Accounts and Roles are the durable record of a player. There is no
//     expiry that is not data loss.
//   - Servers are operator-managed.
//   - Slots are the one-role-per-server exclusion. A TTL on versioned state
//     takes the version with the value, so an expired slot rewritten later
//     restarts at version 1 — and for an exclusion that is the exclusion
//     lapsing on a timer nobody chose. A slot is released by rollback, and a
//     rollback that fails is counted rather than waited out.
func NewRedisStores(client versionstore.RedisClient, prefix string, claimTTL time.Duration) (RedisStores, error) {
	if strings.TrimSpace(prefix) == "" {
		return RedisStores{}, fmt.Errorf("account: redis key prefix is required")
	}
	if claimTTL <= 0 {
		claimTTL = DefaultClaimTTL
	}

	accounts, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[string, Account]{
		Prefix: prefix + ":acct:",
		KeyOf:  func(id string) string { return id },
		Codec:  versionstore.JSONCodec[Account]{},
	})
	if err != nil {
		return RedisStores{}, fmt.Errorf("account: account store: %w", err)
	}
	identities, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[string, IdentityRecord]{
		Prefix: prefix + ":ident:",
		KeyOf:  func(key string) string { return key },
		Codec:  versionstore.JSONCodec[IdentityRecord]{},
	})
	if err != nil {
		return RedisStores{}, fmt.Errorf("account: identity store: %w", err)
	}
	accountIDs, err := NewRedisAccountIDs(client, prefix+":account_id")
	if err != nil {
		return RedisStores{}, err
	}
	// 角色存储带在途换服的索引（transfer_index.go）：Transfer 非空的角色按期限排在 <前缀>:transfers 里，
	// 与角色记录同一个脚本写，所以角色键与索引键要在同一个 slot——Redis Cluster 上前缀要带 hash tag
	// （wiring/account 的 Mod 用 ValidateClusterKeyPrefix 检查）。
	roleStore, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[int64, Role]{
		Prefix: prefix + ":role:",
		KeyOf:  func(playerID int64) string { return strconv.FormatInt(playerID, 10) },
		Codec:  versionstore.JSONCodec[Role]{},
		Index:  &versionstore.RedisIndex[Role]{Key: prefix + ":transfers", Entry: transferIndexEntry},
	})
	if err != nil {
		return RedisStores{}, fmt.Errorf("account: role store: %w", err)
	}
	roles := redisRoles{roleStore}
	servers, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[int32, GameServer]{
		Prefix: prefix + ":srv:",
		KeyOf:  func(id int32) string { return strconv.FormatInt(int64(id), 10) },
		Codec:  versionstore.JSONCodec[GameServer]{},
	})
	if err != nil {
		return RedisStores{}, fmt.Errorf("account: server store: %w", err)
	}
	slots, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[string, Slot]{
		Prefix: prefix + ":slot:",
		KeyOf:  func(key string) string { return key },
		Codec:  versionstore.JSONCodec[Slot]{},
	})
	if err != nil {
		return RedisStores{}, fmt.Errorf("account: slot store: %w", err)
	}

	nameState, err := directory.NewRedisState(client, prefix+":names", 0)
	if err != nil {
		return RedisStores{}, fmt.Errorf("account: name directory state: %w", err)
	}
	names, err := directory.New(nameState, directory.Config{
		Normalize:  directory.NormalizeLower,
		DefaultTTL: claimTTL,
	})
	if err != nil {
		return RedisStores{}, fmt.Errorf("account: name directory: %w", err)
	}

	presence, err := NewRedisPresenceStore(client, prefix+":presence")
	if err != nil {
		return RedisStores{}, err
	}

	return RedisStores{
		Accounts: accounts, Identities: identities, AccountIDs: accountIDs,
		Roles: roles, Servers: servers, Slots: slots, Names: names, Presence: presence,
	}, nil
}

// RedisAccountIDs 是账号号的 Redis 计数器：一个键上的 INCR，所有 account 副本共用，持久。一个键，Redis Cluster 下也是一个槽。
// 应答丢失时不知道加没加：调用方（Login）重试拿下一个号，中间空一个号不要紧，号只要求不重复，不要求连续。
type RedisAccountIDs struct {
	client fredis.ScriptRunner
	key    string
}

// NewRedisAccountIDs 构造。key 是计数器的完整键名（NewRedisStores 用 "<prefix>:account_id"）。
func NewRedisAccountIDs(client fredis.ScriptRunner, key string) (*RedisAccountIDs, error) {
	if client == nil {
		return nil, fmt.Errorf("account: account id counter needs a redis client")
	}
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("account: account id counter needs a key")
	}
	return &RedisAccountIDs{client: client, key: key}, nil
}

// NextAccountID implements AccountIDAllocator.
func (r *RedisAccountIDs) NextAccountID(ctx context.Context) (int64, error) {
	reply, err := r.client.Eval(ctx, "return redis.call('INCR', KEYS[1])", []string{r.key})
	if err != nil {
		return 0, fmt.Errorf("account: account id counter: %w", err)
	}
	number, ok := reply.(int64)
	if !ok {
		return 0, fmt.Errorf("account: account id counter returned %T, want an integer", reply)
	}
	return number, nil
}
