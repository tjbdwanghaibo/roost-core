//go:build integration

package saga

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/redistest"
)

// B1 在真实权威 Redis（冷副本在真实 Mongo 副本集）上：同一个放弃后迟到的成功并发送达多次，tombstone 上的告警标记只让
// 一个调用得到 first=true（mark_alarm.lua 的 HSETNX 原子性）。
func TestRealMongoLateSuccessAlarmIsMarkedOnce(t *testing.T) {
	client, database := realMongoU0280(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store := realRedisStore(t, client, database)
	for round := 0; round < 20; round++ {
		sagaID := fmt.Sprintf("gift-b1-%d", round)
		operation := sagaID + ":1:0"
		now := time.Now().UTC().Truncate(time.Millisecond)
		if err := store.Create(ctx, Record{ID: sagaID, Type: "rally", DefinitionVersion: 1, BusinessKey: sagaID, Status: StatusFailed, Phase: PhaseForward, Version: 3, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		seedTombstone(t, store, sagaID, operation, operationClosureAbandoned, now)
		late := Completion{CommandID: operation + ":1", IdempotencyKey: operation, SagaID: sagaID, Success: true}
		const deliveries = 8
		var wg sync.WaitGroup
		firsts := make([]bool, deliveries)
		errs := make([]error, deliveries)
		start := make(chan struct{})
		for i := 0; i < deliveries; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				firsts[i], errs[i] = store.MarkLateSuccessAlarm(ctx, late, 0)
			}(i)
		}
		close(start)
		wg.Wait()
		winners := 0
		for i := range firsts {
			if errs[i] != nil {
				t.Fatalf("round %d delivery %d: %v", round, i, errs[i])
			}
			if firsts[i] {
				winners++
			}
		}
		if winners != 1 {
			t.Fatalf("round %d: %d concurrent deliveries of one late success were each told they are the first alarm, want exactly 1", round, winners)
		}
		fields, err := store.redis.(*redisdriver.Client).Raw().HGetAll(ctx, store.keys.record(sagaID)).Result()
		if err != nil {
			t.Fatal(err)
		}
		alarms := 0
		for field := range fields {
			if strings.HasPrefix(field, "a:") {
				alarms++
			}
		}
		if _, ok := fields["a:"+operation+":0"]; alarms != 1 || !ok {
			t.Fatalf("round %d: alarm marks = %d (r0 present %v), want only r0", round, alarms, ok)
		}
		if !strings.HasPrefix(fields["o:"+operation], operationClosureAbandoned+"|") {
			t.Fatalf("round %d: marking changed the tombstone: %q", round, fields["o:"+operation])
		}
	}
}

// realRedisStore 是真实权威 Redis（redistest.AOFAddr：scripts/it-redis-aof.sh 起的实例，或私有 redis-server）加真实
// Mongo 冷副本上的协调器存储。
func realRedisStore(t *testing.T, client fmongo.IMongo, database string) *RedisStore {
	t.Helper()
	addr := redistest.AOFAddr(t)
	env := newRedisTestEnvAt(t, addr, redistest.Prefix(t, addr, "sagareal"), client)
	env.db = database
	return env.open(t)
}
