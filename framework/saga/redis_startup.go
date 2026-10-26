package saga

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// redisFormat 是 Redis 上 saga 键的格式版本；改动键或编码时加一，旧格式的 Redis 被启动守卫拒绝。
const redisFormat = "1"

// EnsureInfrastructure 是先恢复再服务的启动关口：任何一条不过都拒绝启动，协调器不开始领取。
//
//	1～3. 部署契约（fredis.CheckAuthority）：单主（redis_version >= 7.2、非 Cluster、role:master）；appendonly yes、
//	   appendfsync always、no-appendfsync-on-rewrite no、maxmemory-policy noeviction（托管 Redis 禁用 CONFIG 时跳过这一条）；
//	   meta 键上一次真实写经同连接 WAITAOF 得到 local=1。
//	4. SCRIPT LOAD 全部脚本。
//	5. 元数据守卫：Mongo 的 saga 元数据（_saga_meta 集合 _id "redis"）与 Redis meta 的实例 ID 必须一致。两边都没有是首次
//	   初始化（Mongo 里不能有非终态 saga：旧版本的在途 saga 要先用旧版本跑完）；Mongo 有、Redis 没有是“权威 Redis 被清空或
//	   换了库”，拒绝——空 Redis 不自动当权威；不一致返回 ErrRedisAuthorityMismatch。
//	6. 分区数变了：所有旧分区的日志都已投影、裁剪为空时才改写，否则拒绝。
//	7. 冷副本的 Mongo 索引。
func (s *RedisStore) EnsureInfrastructure(ctx context.Context) error {
	if err := fredis.CheckAuthority(ctx, s.redis, s.keys.meta(), s.opts.WaitAOFTimeout); err != nil {
		return fmt.Errorf("saga redis: %w", err)
	}
	if err := preloadScripts(ctx, s.redis.(fredis.ScriptLoader), s.lua); err != nil {
		return err
	}
	if err := s.copy.EnsureInfrastructure(ctx); err != nil {
		return err
	}
	return s.guardAuthority(ctx)
}

// sagaMetaDoc 是 Mongo 里的 saga 元数据：本库对应的权威 Redis 实例。
type sagaMetaDoc struct {
	ID        string `bson:"_id"`
	Instance  string `bson:"instance"`
	KeyPrefix string `bson:"key_prefix"`
}

// guardAuthority 是元数据守卫（EnsureInfrastructure 第 5、6 条）。
func (s *RedisStore) guardAuthority(ctx context.Context) error {
	var stored sagaMetaDoc
	err := s.copy.meta().FindOne(ctx, bson.M{"_id": "redis"}, &stored)
	mongoHas := err == nil
	if err != nil && !errors.Is(err, fmongo.ErrNotFound) {
		return fmt.Errorf("saga redis: read saga metadata: %w", err)
	}
	values, err := s.hashes.HMGet(ctx, s.keys.meta(), "instance", "partitions")
	if err != nil {
		return fmt.Errorf("saga redis: read redis metadata: %w", err)
	}
	redisInstance := string(values[0])
	switch {
	case !mongoHas && redisInstance == "":
		active, err := s.copy.sagas().CountDocuments(ctx, bson.M{"status": bson.M{"$in": []Status{StatusPending, StatusWaiting, StatusCompensating}}})
		if err != nil {
			return fmt.Errorf("saga redis: count active sagas in mongo: %w", err)
		}
		if active > 0 {
			return fmt.Errorf("%w: mongo holds %d non-terminal sagas written by an earlier version; finish them with that version before switching to the redis authority", ErrRedisAuthorityMismatch, active)
		}
		if _, err := s.copy.meta().InsertOne(ctx, sagaMetaDoc{ID: "redis", Instance: NewID(), KeyPrefix: s.keys.prefix}); err != nil && !errors.Is(err, fmongo.ErrDuplicateKey) {
			return fmt.Errorf("saga redis: write saga metadata: %w", err)
		}
		// 并发初始化时以 Mongo 里先写成的那一份为准。
		if err := s.copy.meta().FindOne(ctx, bson.M{"_id": "redis"}, &stored); err != nil {
			return fmt.Errorf("saga redis: read back saga metadata: %w", err)
		}
	case mongoHas && redisInstance == "":
		return fmt.Errorf("%w: mongo database %q records authority instance %s (key prefix %q) but the redis prefix %q is empty; the authority redis was flushed or replaced — follow the disaster recovery runbook instead of starting on an empty redis",
			ErrRedisAuthorityMismatch, s.copy.opts.Database, stored.Instance, stored.KeyPrefix, s.keys.prefix)
	case !mongoHas:
		return fmt.Errorf("%w: redis prefix %q belongs to authority instance %s but mongo database %q has no saga metadata", ErrRedisAuthorityMismatch, s.keys.prefix, redisInstance, s.copy.opts.Database)
	case stored.Instance != redisInstance:
		return fmt.Errorf("%w: mongo database %q expects authority instance %s, redis prefix %q holds %s", ErrRedisAuthorityMismatch, s.copy.opts.Database, stored.Instance, s.keys.prefix, redisInstance)
	}
	allowChange := "0"
	if old := string(values[1]); old != "" && old != strconv.Itoa(s.opts.Partitions) {
		previous, err := strconv.Atoi(old)
		if err != nil || previous < 1 {
			return fmt.Errorf("%w: redis metadata has partitions %q", ErrRedisAuthorityMismatch, old)
		}
		reader, ok := s.redis.(fredis.StreamReader)
		if !ok {
			return errors.New("saga redis: the authority client cannot read streams")
		}
		for partition := range max(previous, s.opts.Partitions) {
			backlog, err := reader.XLen(ctx, s.keys.log(partition))
			if err != nil {
				return err
			}
			if backlog > 0 {
				return fmt.Errorf("%w: partitions change from %d to %d while partition %d still has %d unprojected log entries; let the store service drain them first",
					ErrRedisAuthorityMismatch, previous, s.opts.Partitions, partition, backlog)
			}
		}
		allowChange = "1"
	}
	code, items, err := s.run(ctx, "meta", []string{s.keys.meta()}, stored.Instance, s.opts.Partitions, redisFormat, allowChange)
	if err != nil {
		return err
	}
	switch code {
	case "OK":
		return nil
	case "MISMATCH":
		return fmt.Errorf("%w: redis prefix %q holds instance %s, mongo expects %s", ErrRedisAuthorityMismatch, s.keys.prefix, replyString(items, 0), stored.Instance)
	case "FORMAT":
		return fmt.Errorf("%w: redis prefix %q has key format %s, this version writes %s; stop the old version and start on a new prefix", ErrRedisAuthorityMismatch, s.keys.prefix, replyString(items, 0), redisFormat)
	case "PARTITIONS":
		return fmt.Errorf("%w: redis prefix %q was written with %s partitions", ErrRedisAuthorityMismatch, s.keys.prefix, replyString(items, 0))
	default:
		return fmt.Errorf("saga redis: meta returned %q", code)
	}
}
