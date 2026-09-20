package account

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"

	"github.com/tjbdwanghaibo/roost-kit/service/directory"
)

// RedisStores are the four stores this package needs, over Redis, plus the
// name directory.
//
// There is no storage logic here. Accounts, roles, servers and slots are all
// versioned state, and kit's versionstore.RedisStore already implements that
// contract. What this contributes is the part that must not be each caller's
// to invent: four key namespaces under one prefix that must not collide, and
// the TTL decisions below, each of which has a wrong answer that looks fine.
type RedisStores struct {
	Accounts versionstore.Store[string, Account]
	Roles    versionstore.Store[int64, Role]
	Servers  versionstore.Store[int32, GameServer]
	Slots    versionstore.Store[string, Slot]
	Names    directory.Directory
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
	roles, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[int64, Role]{
		Prefix: prefix + ":role:",
		KeyOf:  func(playerID int64) string { return strconv.FormatInt(playerID, 10) },
		Codec:  versionstore.JSONCodec[Role]{},
	})
	if err != nil {
		return RedisStores{}, fmt.Errorf("account: role store: %w", err)
	}
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

	return RedisStores{
		Accounts: accounts, Roles: roles, Servers: servers, Slots: slots, Names: names,
	}, nil
}
