package redis

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// ErrAuthorityContract：Redis 实例不满足“权威存储”的部署契约（单主、appendonly yes、appendfsync always、
// no-appendfsync-on-rewrite no、maxmemory-policy noeviction，以及同连接 WAITAOF 能确认本地落盘）。
var ErrAuthorityContract = errors.New("redis: instance does not meet the authority store contract")

// probeScript 在 probe 键上做一次真实写（计数加一）。键类型不对时返回 CORRUPT，不写。
const probeScript = `#!lua
local t = redis.call('TYPE', KEYS[1])['ok']
if t ~= 'none' and t ~= 'hash' then return 'CORRUPT' end
redis.call('HINCRBY', KEYS[1], 'probes', 1)
return 'OK'`

// CheckAuthority 是权威 Redis 的启动探针（任何一条不过都返回错误，调用方拒绝启动）：
//
//  1. 拓扑：redis_version >= 7.2，cluster_enabled:0，role:master——单主，不支持 Cluster 与副本上的写，也不支持
//     Sentinel 自动切换（切主后已确认的写可能回退，框架发现不了）。
//  2. 持久配置：appendonly yes、appendfsync always、no-appendfsync-on-rewrite no、maxmemory-policy noeviction。
//     托管 Redis 禁用 CONFIG 时记 WARN 跳过这一条，由第 3 条兜底。
//  3. 探针写：probeKey 上一次真实写，经同连接 WAITAOF 必须得到 local = 1。
//
// client 必须实现 ServerInspector 与 DurableScriptBatcher（redis/driver.Client 满足）。
func CheckAuthority(ctx context.Context, client IRedis, probeKey string, waitTimeout time.Duration) error {
	inspector, ok := client.(ServerInspector)
	batcher, okBatch := client.(DurableScriptBatcher)
	if !ok || !okBatch {
		return fmt.Errorf("%w: %T cannot run the startup probe (INFO / CONFIG / durable scripts)", ErrAuthorityContract, client)
	}
	if err := checkAuthorityTopology(ctx, inspector); err != nil {
		return err
	}
	if err := checkAuthorityDurability(ctx, inspector); err != nil {
		return err
	}
	sum := sha1.Sum([]byte(probeScript))
	results, local, _, err := batcher.DurableScriptBatch(ctx, []ScriptCall{{SHA: hex.EncodeToString(sum[:]), Source: probeScript, Keys: []string{probeKey}}}, 1, 0, waitTimeout)
	if err == nil && len(results) == 1 {
		if results[0].Err != nil {
			err = results[0].Err
		} else if results[0].Value != "OK" {
			return fmt.Errorf("%w: probe key %s has the wrong type", ErrAuthorityContract, probeKey)
		}
	}
	if err != nil || local < 1 {
		return fmt.Errorf("%w: the probe write was not confirmed by WAITAOF (local=%d): %v", ErrAuthorityContract, local, err)
	}
	return nil
}

func checkAuthorityTopology(ctx context.Context, inspector ServerInspector) error {
	server, err := inspector.Info(ctx, "server")
	if err != nil {
		return fmt.Errorf("redis: INFO server: %w", err)
	}
	if version := infoField(server, "redis_version"); !versionAtLeast(version, 7, 2) {
		return fmt.Errorf("%w: redis %q is older than 7.2 (WAITAOF and script flags are required)", ErrAuthorityContract, version)
	}
	cluster, err := inspector.Info(ctx, "cluster")
	if err != nil {
		return fmt.Errorf("redis: INFO cluster: %w", err)
	}
	if infoField(cluster, "cluster_enabled") == "1" {
		return fmt.Errorf("%w: the instance is a cluster node; only a single primary is supported", ErrAuthorityContract)
	}
	replication, err := inspector.Info(ctx, "replication")
	if err != nil {
		return fmt.Errorf("redis: INFO replication: %w", err)
	}
	if role := infoField(replication, "role"); role != "master" {
		return fmt.Errorf("%w: the instance has role %q; point it at the primary (automatic failover is not supported)", ErrAuthorityContract, role)
	}
	return nil
}

func checkAuthorityDurability(ctx context.Context, inspector ServerInspector) error {
	want := []struct{ name, value string }{{"appendonly", "yes"}, {"appendfsync", "always"}, {"no-appendfsync-on-rewrite", "no"}, {"maxmemory-policy", "noeviction"}}
	for _, item := range want {
		values, err := inspector.ConfigGet(ctx, item.name)
		if err != nil {
			slog.Warn("redis: CONFIG GET is unavailable; relying on the WAITAOF probe", "parameter", item.name, "err", err)
			return nil
		}
		if got := values[item.name]; got != item.value {
			return fmt.Errorf("%w: %s=%q, the authority store requires %q", ErrAuthorityContract, item.name, got, item.value)
		}
	}
	return nil
}

func infoField(info, name string) string {
	for _, line := range strings.Split(info, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), name+":"); ok {
			return value
		}
	}
	return ""
}

func versionAtLeast(version string, major, minor int) bool {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return false
	}
	gotMajor, err1 := strconv.Atoi(parts[0])
	gotMinor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return gotMajor > major || (gotMajor == major && gotMinor >= minor)
}
