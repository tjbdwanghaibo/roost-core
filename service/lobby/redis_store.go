package lobby

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// RedisRooms 是 Redis 上的房间目录，照 service/directory 的写法建在 versionstore.RedisStore 上。
//
// 键：
//
//	<prefix>:room:<房间号>              房间记录（versionstore 信封 + JSON），PX = room TTL，每次写入重置
//	<prefix>:rooms:<game>:<mode>        可加入房间的索引（ZSET，成员是房间号，分数是 -建房秒，新房在前）
//
// 记录与索引在同一个脚本里写，所以 Redis Cluster 上两者必须同槽：prefix 带 hash tag（例如 roost:{party}:lobby）。
// 这里用记录的键 TTL 而不是 directory 那样的过期字段：房间号每次随机生成、从不复用，记录过期后版本从 1 重来
// 不会让任何 compare 失效；Update 遇到不存在的房间直接报 ErrRoomMissing，过期的房间不会被心跳“复活”。
type RedisRooms struct {
	store  *versionstore.RedisStore[string, Room]
	prefix string
}

// NewRedisRooms 建 Redis 房间目录。prefix 必填；ttl 是房间记录的寿命，必须为正。
func NewRedisRooms(client versionstore.RedisClient, prefix string, ttl time.Duration) (*RedisRooms, error) {
	if strings.TrimSpace(prefix) == "" {
		return nil, fmt.Errorf("lobby: redis key prefix is required")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("lobby: room ttl must be positive; a room record without expiry outlives a crashed relay")
	}
	rooms := &RedisRooms{prefix: prefix}
	store, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[string, Room]{
		Prefix: prefix + ":room:",
		KeyOf:  func(id string) string { return id },
		Codec:  versionstore.JSONCodec[Room]{},
		TTL:    ttl,
		Index: &versionstore.RedisIndex[Room]{
			KeyOf: func(room Room) string { return rooms.indexKey(room.Game, room.Mode) },
			Entry: func(room Room) (float64, bool) { return -float64(room.CreatedAtUnix), room.listable() },
		},
	})
	if err != nil {
		return nil, fmt.Errorf("lobby: %w", err)
	}
	rooms.store = store
	return rooms, nil
}

func (r *RedisRooms) indexKey(game, mode string) string {
	return r.prefix + ":rooms:" + game + ":" + mode
}

func (r *RedisRooms) Create(ctx context.Context, room Room) (bool, error) {
	_, created, err := r.store.Create(ctx, room.ID, room)
	return created, err
}

func (r *RedisRooms) Get(ctx context.Context, id string) (Room, bool, error) {
	got, found, err := r.store.Get(ctx, id)
	return got.Value, found, err
}

func (r *RedisRooms) Update(ctx context.Context, id string, mutate func(*Room) (bool, error)) (Room, error) {
	result, _, err := r.store.Update(ctx, id, func(current Room, found bool) (Room, bool, error) {
		if !found {
			return Room{}, false, ErrRoomMissing
		}
		next := copyRoom(current)
		save, err := mutate(&next)
		if err != nil || !save {
			return current, false, err
		}
		return next, true, nil
	})
	return result.Value, err
}

// List 读索引里分数最小（最新）的一批房间号，逐个读记录；记录已经过期的，把索引项删掉再补读一批，
// 最多三轮。还在索引里、但按当前时刻已经不可加入的房间（例如预留席位刚过期前满员）由调用方筛掉。
func (r *RedisRooms) List(ctx context.Context, game, mode string, limit int) ([]Room, error) {
	indexKey := r.indexKey(game, mode)
	var out []Room
	for round := 0; round < 3; round++ {
		ids, err := r.store.IndexDueIn(ctx, indexKey, 0, limit)
		if err != nil {
			return nil, err
		}
		out = out[:0]
		stale := 0
		for _, id := range ids {
			got, found, err := r.store.Get(ctx, id)
			if err != nil && !errors.Is(err, versionstore.ErrMalformedRecord) {
				return nil, err
			}
			if !found || err != nil {
				// 房间号从不复用，记录不在了就不会再出现：无条件撤掉索引项是安全的。
				if _, err := r.store.IndexRemoveIn(ctx, indexKey, id); err != nil {
					return nil, err
				}
				stale++
				continue
			}
			out = append(out, got.Value)
		}
		if stale == 0 || len(ids) < limit {
			break
		}
	}
	return out, nil
}

var _ RoomStore = (*RedisRooms)(nil)
