package entity

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Mirror 方案第 1、2 步（docs/feature/MIRROR-STEPS-1-3-2026-10-06.md）：只读契约与统一读出口。
//
// kind 243：同包已用 kind 见各 *_test.go（241/242 在 snapshot_delete_l2_promises_test.go），243 未被占用。
// 注册为 managed（owner 身份），用来证明同进程的只读方不需要再注册任何东西。
const mirrorContractKind EntityKind = 243

func mirrorContractKey(t *testing.T, unique int64) RemoteSnapshotKey {
	t.Helper()
	MustRegisterEntityKindDefs(EntityKindDef{Kind: mirrorContractKind, Category: 1, RemotePolicy: RemotePolicyManaged})
	id, err := BuildEntityID(unique, mirrorContractKind)
	if err != nil {
		t.Fatal(err)
	}
	return RemoteSnapshotKey{EntityID: id, Kind: mirrorContractKind, Scope: 7}
}

func mirrorEnvelope(key RemoteSnapshotKey, marker, route, version uint64, payload string) RemoteSnapshotEnvelope {
	data := []byte(payload)
	return RemoteSnapshotEnvelope{
		Key: key, StateVersion: version, MarkerEpoch: marker, RouteEpoch: route,
		Schema: 77, Codec: 1, Full: true, Checksum: RemoteSnapshotChecksum(data),
		Payload: CopyFrozenRemoteSnapshotPayload(data),
	}
}

// cacheReadOnly 把快照缓存唯一的读出口包成只读能力（remoteentity.SnapshotClient 做的就是这件事，外加兴趣与生命周期）。
type cacheReadOnly struct{ cache *RemoteSnapshotCache }

func (s cacheReadOnly) ReadSnapshot(ctx context.Context, req RemoteSnapshotRead) (RemoteSnapshotEnvelope, bool, error) {
	return s.cache.Read(ctx, req)
}

// 观察 token 的排序与快照准入同一条规则：更新的 epoch 胜出（不论版本），同 epoch 比版本，混合 epoch 不可比。
func TestRemoteObservationCoversFollowsAdmissionOrder(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snapshot RemoteObservation
		min      RemoteObservation
		want     bool
		wantErr  error
	}{
		{"zero token constrains nothing", RemoteObservation{1, 1, 1}, RemoteObservation{}, true, nil},
		{"version-only token: lower version", RemoteObservation{2, 2, 3}, RemoteObservation{StateVersion: 5}, false, nil},
		{"version-only token: equal version", RemoteObservation{2, 2, 5}, RemoteObservation{StateVersion: 5}, true, nil},
		{"same epochs: lower version", RemoteObservation{1, 1, 4}, RemoteObservation{1, 1, 5}, false, nil},
		{"same epochs: same version", RemoteObservation{1, 1, 5}, RemoteObservation{1, 1, 5}, true, nil},
		{"newer route epoch wins over a higher version", RemoteObservation{1, 2, 1}, RemoteObservation{1, 1, 9}, true, nil},
		{"newer marker epoch wins over a higher version", RemoteObservation{2, 1, 1}, RemoteObservation{1, 1, 9}, true, nil},
		{"older route epoch loses despite a higher version", RemoteObservation{1, 1, 9}, RemoteObservation{1, 2, 1}, false, nil},
		{"mixed epochs are incomparable", RemoteObservation{2, 1, 9}, RemoteObservation{1, 2, 1}, false, ErrRemoteObservationIncomparable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.snapshot.Covers(tc.min)
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) || got != tc.want {
				t.Fatalf("%+v.Covers(%+v) = %v, %v; want %v, %v", tc.snapshot, tc.min, got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// 第 1 步验收“DTO 修改不污染缓存”：解码器拿到的是独立副本。这里的解码器故意直接保留传入的切片（最容易
// 出错的写法），业务改写 DTO 之后再读，缓存里的值不变。
func TestRemoteMirrorReaderDTOMutationDoesNotPolluteCache(t *testing.T) {
	ctx := context.Background()
	key := mirrorContractKey(t, 9701)
	c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{TTL: time.Minute}, nil, nil)
	if err := c.Publish(ctx, mirrorEnvelope(key, 1, 1, 3, "summary-v3")); err != nil {
		t.Fatal(err)
	}
	reader, err := NewRemoteMirrorReader(cacheReadOnly{c}, RemoteMirrorSpec{Kind: key.Kind, Scope: key.Scope, Schema: 77, Codec: 1},
		func(data []byte) ([]byte, error) { return data, nil })
	if err != nil {
		t.Fatal(err)
	}
	first, found, err := reader.Read(ctx, key.EntityID, RemoteReadCached, RemoteObservation{})
	if err != nil || !found || string(first.Value) != "summary-v3" {
		t.Fatalf("first read: value=%q found=%v err=%v", first.Value, found, err)
	}
	if first.Observation != (RemoteObservation{MarkerEpoch: 1, RouteEpoch: 1, StateVersion: 3}) {
		t.Fatalf("observation token = %+v, want marker=1 route=1 version=3", first.Observation)
	}
	for i := range first.Value {
		first.Value[i] = 'X'
	}
	second, found, err := reader.Read(ctx, key.EntityID, RemoteReadCached, RemoteObservation{})
	if err != nil || !found || string(second.Value) != "summary-v3" {
		t.Fatalf("after the caller rewrote its DTO, the next read returned %q found=%v err=%v; the cache was polluted", second.Value, found, err)
	}
	stored, _, _ := c.Get(ctx, key, RemoteReadCached, 0)
	if stored.Checksum != RemoteSnapshotChecksum(stored.Payload.BytesCopy()) || string(stored.Payload.BytesCopy()) != "summary-v3" {
		t.Fatalf("cached payload changed to %q", stored.Payload.BytesCopy())
	}
}

// 第 1 步验收“owner / consumer 同身份、同进程无冲突注册”：owner（生成代码）为这个 kind 注册了 managed
// 身份与全局解码器；旧的消费方式只能再注册一次解码器，必然撞上 ErrRemoteSnapshotDecoderDuplicate。
// RemoteMirrorReader 不注册任何进程级表，同进程可以照常构造并读取，owner 的身份不变。
func TestRemoteMirrorReaderNeedsNoRegistrationBesideTheOwner(t *testing.T) {
	ctx := context.Background()
	key := mirrorContractKey(t, 9702)
	const schema uint32 = 0x6d697272 // 只在本用例使用的 schema 号
	ownerDecoder := func(data []byte) (any, error) { return string(data), nil }
	if err := RegisterRemoteSnapshotDecoder(schema, ownerDecoder); err != nil && !errors.Is(err, ErrRemoteSnapshotDecoderDuplicate) {
		t.Fatal(err)
	}
	// 旧路径：同进程的消费方再注册同一身份的解码器——冲突（修前只有这条路，本条断言记录这个事实）。
	if err := RegisterRemoteSnapshotDecoder(schema, ownerDecoder); !errors.Is(err, ErrRemoteSnapshotDecoderDuplicate) {
		t.Fatalf("registering the owner's schema a second time = %v; want ErrRemoteSnapshotDecoderDuplicate", err)
	}

	c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{TTL: time.Minute}, nil, nil)
	envelope := mirrorEnvelope(key, 1, 1, 4, "owner-summary")
	envelope.Schema = schema
	if err := c.Publish(ctx, envelope); err != nil {
		t.Fatal(err)
	}
	reader, err := NewRemoteMirrorReader(cacheReadOnly{c}, RemoteMirrorSpec{Kind: key.Kind, Scope: key.Scope, Schema: schema, Codec: 1},
		func(data []byte) (string, error) { return string(data), nil })
	if err != nil {
		t.Fatalf("a consumer reader for the owner's identity in the same process: %v", err)
	}
	got, found, err := reader.Read(ctx, key.EntityID, RemoteReadCached, RemoteObservation{})
	if err != nil || !found || got.Value != "owner-summary" {
		t.Fatalf("consumer read value=%q found=%v err=%v", got.Value, found, err)
	}
	if policy := GetEntityKindRemotePolicy(key.Kind); policy != RemotePolicyManaged {
		t.Fatalf("the owner's kind policy changed to %d after a consumer reader was built", policy)
	}
}

// 身份与 schema 是读出口的后置条件（RR-20261005-NC-35 / RR-20260913-07 的读侧）：source 交回别的 key 或别的
// schema 时报错，解码器不被调用。
func TestRemoteMirrorReaderRejectsForeignIdentityAndSchema(t *testing.T) {
	ctx := context.Background()
	key := mirrorContractKey(t, 9703)
	other := mirrorContractKey(t, 9704)
	for _, tc := range []struct {
		name    string
		answer  RemoteSnapshotEnvelope
		wantErr error
	}{
		{"another entity's snapshot", mirrorEnvelope(other, 1, 1, 1, "other"), nil},
		{"another schema", func() RemoteSnapshotEnvelope { e := mirrorEnvelope(key, 1, 1, 1, "v1"); e.Schema = 78; return e }(), ErrRemoteSnapshotSchemaMismatch},
		{"another codec", func() RemoteSnapshotEnvelope { e := mirrorEnvelope(key, 1, 1, 1, "v1"); e.Codec = 2; return e }(), ErrRemoteSnapshotSchemaMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoded := false
			reader, err := NewRemoteMirrorReader(fixedReadOnly{tc.answer}, RemoteMirrorSpec{Kind: key.Kind, Scope: key.Scope, Schema: 77, Codec: 1},
				func(data []byte) (string, error) { decoded = true; return string(data), nil })
			if err != nil {
				t.Fatal(err)
			}
			_, found, err := reader.Read(ctx, key.EntityID, RemoteReadCached, RemoteObservation{})
			if err == nil || found || decoded || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
				t.Fatalf("found=%v decoded=%v err=%v; want an error (%v) before decoding", found, decoded, err, tc.wantErr)
			}
		})
	}
}

type fixedReadOnly struct{ answer RemoteSnapshotEnvelope }

func (s fixedReadOnly) ReadSnapshot(context.Context, RemoteSnapshotRead) (RemoteSnapshotEnvelope, bool, error) {
	return s.answer, true, nil
}

// 第 2 步：所有读出口共用同一组后置条件。每个出口（Read 的三种一致性、旧 Get、直接 LoadAuthoritative）
// 对同一种情形给出同样的结论：
//
//   - 过期（ExpiresAt）的 L1 条目不交出；权威给出同版本、更晚有效期时刷新有效期（同版本刷新），各出口
//     都交出刷新后的值；
//   - 带 epoch 的 token：更新 epoch、更低版本的快照满足它；混合 epoch 报 ErrRemoteObservationIncomparable；
//   - Cached 有值但不满足最低要求：ErrRemoteSnapshotStale，不交出低于要求的值。
func TestRemoteSnapshotReadExitsShareOnePostCondition(t *testing.T) {
	ctx := context.Background()
	type exit struct {
		name string
		read func(c *RemoteSnapshotCache, key RemoteSnapshotKey, after RemoteObservation) (RemoteSnapshotEnvelope, bool, error)
	}
	exits := []exit{
		{"Read/Monotonic", func(c *RemoteSnapshotCache, key RemoteSnapshotKey, after RemoteObservation) (RemoteSnapshotEnvelope, bool, error) {
			return c.Read(ctx, RemoteSnapshotRead{Key: key, Consistency: RemoteReadMonotonic, After: after})
		}},
		{"Read/Linearizable", func(c *RemoteSnapshotCache, key RemoteSnapshotKey, after RemoteObservation) (RemoteSnapshotEnvelope, bool, error) {
			return c.Read(ctx, RemoteSnapshotRead{Key: key, Consistency: RemoteReadLinearizable, After: after})
		}},
		{"LoadAuthoritative", func(c *RemoteSnapshotCache, key RemoteSnapshotKey, after RemoteObservation) (RemoteSnapshotEnvelope, bool, error) {
			if !after.versionOnly() {
				// 旧签名只有版本；带 epoch 的 token 经内部同一个函数。
				return c.loadAuthoritative(ctx, key, RemoteReadMonotonic, after)
			}
			return c.LoadAuthoritative(ctx, key, RemoteReadMonotonic, after.StateVersion)
		}},
	}

	t.Run("same-version refresh of an expired entry", func(t *testing.T) {
		for i, e := range exits {
			key := mirrorContractKey(t, 9710+int64(i))
			later := time.Now().Add(time.Hour).UnixNano()
			c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{TTL: time.Minute}, nil,
				func(_ context.Context, k RemoteSnapshotKey, _ RemoteReadConsistency, _ uint64) (RemoteSnapshotEnvelope, bool, error) {
					fresh := mirrorEnvelope(k, 1, 1, 5, "v5")
					fresh.ExpiresAt = later
					return fresh, true, nil
				})
			expired := mirrorEnvelope(key, 1, 1, 5, "v5")
			expired.ExpiresAt = time.Now().Add(-time.Second).UnixNano()
			if err := c.Publish(ctx, expired); err != nil {
				t.Fatal(err)
			}
			if _, found, err := c.Read(ctx, RemoteSnapshotRead{Key: key, Consistency: RemoteReadCached}); err != nil || found {
				t.Fatalf("Cached served an expired entry: found=%v err=%v", found, err)
			}
			got, found, err := e.read(c, key, RemoteObservation{})
			if err != nil || !found || got.StateVersion != 5 || got.ExpiresAt != later {
				t.Fatalf("%s: same-version authoritative refresh returned version=%d expires=%d found=%v err=%v; want v5 expiring at %d",
					e.name, got.StateVersion, got.ExpiresAt, found, err, later)
			}
			if got, found, err := c.Read(ctx, RemoteSnapshotRead{Key: key, Consistency: RemoteReadCached}); err != nil || !found || got.ExpiresAt != later {
				t.Fatalf("%s: after the refresh Cached read found=%v expires=%d err=%v", e.name, found, got.ExpiresAt, err)
			}
		}
	})

	t.Run("token across an epoch change", func(t *testing.T) {
		for i, e := range exits {
			key := mirrorContractKey(t, 9720+int64(i))
			// 迁移之后：route epoch 2、版本 1 比 route epoch 1、版本 9 新。
			c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{TTL: time.Minute}, nil,
				func(_ context.Context, k RemoteSnapshotKey, _ RemoteReadConsistency, minVersion uint64) (RemoteSnapshotEnvelope, bool, error) {
					if minVersion > 1 {
						// 像 Mongo 的 state_version >= min 过滤：带 epoch 的 token 不能把版本下推给 loader。
						return RemoteSnapshotEnvelope{}, false, nil
					}
					return mirrorEnvelope(k, 1, 2, 1, "after-takeover"), true, nil
				})
			got, found, err := e.read(c, key, RemoteObservation{MarkerEpoch: 1, RouteEpoch: 1, StateVersion: 9})
			if err != nil || !found || got.RouteEpoch != 2 || got.StateVersion != 1 {
				t.Fatalf("%s: a token from before the takeover returned route=%d version=%d found=%v err=%v; the newer epoch satisfies it",
					e.name, got.RouteEpoch, got.StateVersion, found, err)
			}
			if _, found, err := e.read(c, key, RemoteObservation{MarkerEpoch: 2, RouteEpoch: 1, StateVersion: 1}); !errors.Is(err, ErrRemoteObservationIncomparable) || found {
				t.Fatalf("%s: mixed-epoch token found=%v err=%v; want ErrRemoteObservationIncomparable", e.name, found, err)
			}
		}
	})

	t.Run("cached read below the requested minimum", func(t *testing.T) {
		key := mirrorContractKey(t, 9730)
		c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{TTL: time.Minute}, nil, nil)
		if err := c.Publish(ctx, mirrorEnvelope(key, 1, 1, 3, "v3")); err != nil {
			t.Fatal(err)
		}
		got, found, err := c.Get(ctx, key, RemoteReadCached, 5)
		if found || !errors.Is(err, ErrRemoteSnapshotStale) {
			t.Fatalf("Cached read with minVersion=5 returned version=%d found=%v err=%v; want ErrRemoteSnapshotStale, not a value below the minimum",
				got.StateVersion, found, err)
		}
		if _, found, err := c.Read(ctx, RemoteSnapshotRead{Key: key, Consistency: RemoteReadCached, After: RemoteObservation{MarkerEpoch: 1, RouteEpoch: 1, StateVersion: 3}}); err != nil || !found {
			t.Fatalf("a satisfied token: found=%v err=%v", found, err)
		}
	})
}
