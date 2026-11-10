// 线格式的字节：位置增量这类最常见的更新，帧里除业务载荷之外的开销（帧头、对象头、组件头、subject 信封）有上限。

package entitysync

import (
	"bytes"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
)

// positionPatch 是生成 DAO 的一份“只改 PosX”的 PB 补丁的大小（字段号 7 的包装消息：tag、长度、内层 tag、两字节 varint）。
var positionPatch = []byte{0x3a, 0x03, 0x08, 0x8a, 0x04}

func positionSubject(id int64, namespace string) *entity.SubjectSyncState {
	return entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{
		Enabled: true, SubjectID: id, Namespace: namespace, SubjectKind: 1,
		Packer: entity.SubjectSyncPackFunc{
			Snapshot: func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
				return entity.CopyFrozenSyncPayload(2, []byte("a full view of the subject")), nil
			},
			Delta: func(entity.SyncProfile, uint64) (entity.FrozenSyncPayload, error) {
				return entity.CopyFrozenSyncPayload(2, positionPatch), nil
			},
		},
	})
}

// 一个观察者看着 subject 移动：之后的每一步是一份只带位置的增量。帧里除载荷之外的字节：
// 一帧只有这一个对象时不超过 24 字节（帧头 + 对象头 + 组件头 + 信封）；同一帧四个对象时每个对象的边际开销不超过 12 字节。
// 上限对应线格式 v3 在小计数（tick、版本、对象引用都在一字节 varint 以内）下的实际值，见 docs/framework/ENTITY_SYNC.md“线格式”。
// 修前（帧头 32、对象头 9、组件头 9、信封 64 加命名空间与视图键字符串）一帧一个对象的开销是 125 字节。
func TestAPositionDeltaCarriesLittleBesidesThePosition(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	ids := []int64{9975812, 2323435195258110993, 9984004, 9985028}
	states := make([]*entity.SubjectSyncState, len(ids))
	for i, id := range ids {
		states[i] = positionSubject(id, "player")
		if err := manager.Register(states[i]); err != nil {
			t.Fatal(err)
		}
	}
	open(t, manager, 1)
	for _, id := range ids {
		mustSubscribe(t, manager, 1, id, entity.SyncProfile{})
	}
	mustFlush(t, manager)
	if frames := transport.take(1); len(frames) != 1 {
		t.Fatalf("snapshot frames = %d, want 1", len(frames))
	}

	// 一步：只有一个对象动。
	states[0].MarkDirty(0x20)
	mustFlush(t, manager)
	single := transport.take(1)
	if len(single) != 1 {
		t.Fatalf("one step reached the watcher as %d frame(s), want 1", len(single))
	}
	overhead := len(single[0]) - len(positionPatch)
	t.Logf("one subject's position step: frame %d bytes, %d of them besides the %d-byte patch", len(single[0]), overhead, len(positionPatch))
	if overhead > 24 {
		t.Fatalf("one position step costs %d bytes besides the position patch, want at most 24", overhead)
	}

	// 同一拍四个对象各走一步：帧头分摊之后每个对象的边际开销。
	for _, state := range states {
		state.MarkDirty(0x20)
	}
	mustFlush(t, manager)
	together := transport.take(1)
	if len(together) != 1 {
		t.Fatalf("four steps reached the watcher as %d frame(s), want 1", len(together))
	}
	perObject := (len(together[0]) - len(single[0])) / (len(ids) - 1)
	t.Logf("four position steps in one frame: %d bytes, %d per extra subject (%d besides the patch)", len(together[0]), perObject, perObject-len(positionPatch))
	if perObject-len(positionPatch) > 12 {
		t.Fatalf("each extra subject in a frame costs %d bytes besides the position patch, want at most 12", perObject-len(positionPatch))
	}
}

// 信封往返：全量带齐身份，增量只带版本、mask、reason 与载荷，身份从基线继承；缺省视图键在线上是空串。
func TestSubjectEnvelopeRoundTripsAndADeltaInheritsItsIdentity(t *testing.T) {
	full := entity.SubjectSyncUpdate{
		SubjectID: 2323435195258110993, SubjectKind: 4, Namespace: "monster", Profile: entity.SyncProfile{Key: "near", LOD: 1, SchemaVersion: 7},
		Version: 9, BaseVersion: 9, Full: true, Reason: 2, Payload: entity.CopyFrozenSyncPayload(2, []byte("full")),
	}
	// 服务端的增量也带着身份，只是不编码。
	delta := entity.SubjectSyncUpdate{SubjectID: full.SubjectID, SubjectKind: full.SubjectKind, Namespace: full.Namespace, Profile: full.Profile,
		Version: 12, BaseVersion: 9, Mask: 0x20, Payload: entity.CopyFrozenSyncPayload(2, positionPatch)}
	for _, tc := range []struct {
		name     string
		update   entity.SubjectSyncUpdate
		baseline *entity.SubjectSyncUpdate
		want     entity.SubjectSyncUpdate
	}{
		{"full", full, nil, full},
		{"full on the default view", func() entity.SubjectSyncUpdate { u := full; u.Profile = entity.SyncProfile{}; return u }(), nil,
			func() entity.SubjectSyncUpdate { u := full; u.Profile = entity.SyncProfile{Key: "default"}; return u }()},
		{"delta", delta, &full, delta},
	} {
		encoded, err := EncodeSubjectUpdate(tc.update, 0)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(encoded) != subjectUpdateBytes(tc.update) {
			t.Fatalf("%s: encoded %d bytes, subjectUpdateBytes says %d", tc.name, len(encoded), subjectUpdateBytes(tc.update))
		}
		got, err := decodeSubjectUpdate(encoded, 0, tc.baseline)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got.SubjectID != tc.want.SubjectID || got.SubjectKind != tc.want.SubjectKind || got.Namespace != tc.want.Namespace || got.Profile != tc.want.Profile ||
			got.Version != tc.want.Version || got.BaseVersion != tc.want.BaseVersion || got.Mask != tc.want.Mask || got.Reason != tc.want.Reason ||
			got.Full != tc.want.Full || !got.Payload.Equal(tc.want.Payload) {
			t.Fatalf("%s: round trip\n got %+v\nwant %+v", tc.name, got, tc.want)
		}
	}
	encoded, err := EncodeSubjectUpdate(delta, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != 4+len(positionPatch) {
		t.Fatalf("a position delta's envelope is %d bytes, want 4 (flags, base, step, mask)", len(encoded)-len(positionPatch))
	}
	if _, err := decodeSubjectUpdate(encoded, 0, nil); err == nil {
		t.Fatal("a delta decoded without a baseline")
	}
	stale := full
	stale.Version = 8
	if _, err := decodeSubjectUpdate(encoded, 0, &stale); err == nil {
		t.Fatal("a delta on version 9 applied to a baseline at version 8")
	}
	if _, err := EncodeSubjectUpdate(entity.SubjectSyncUpdate{SubjectID: 1, Version: 3, BaseVersion: 3}, 0); err == nil {
		t.Fatal("a delta that does not advance the version was encoded")
	}
	for name, corrupt := range map[string][]byte{
		"unknown flag":       append([]byte{encoded[0] | 0x80}, encoded[1:]...),
		"truncated":          encoded[:1],
		"zero step":          {0, 9, 0},
		"mask flag, no mask": {subjectFlagMask, 9, 1, 0},
	} {
		if _, err := decodeSubjectUpdate(corrupt, 0, &full); err == nil {
			t.Fatalf("%s: decoded", name)
		}
	}
}

// Decoder 是接收端的契约：增量必须接在同一对象上一次的版本之后，乱序、重放、缺口、对象引用换 subject 都不通过，
// 失败不改变状态、之后只接受全量；全量清空对象表。
func TestDecoderRefusesWhatDoesNotContinueTheStream(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	a, b := positionSubject(11, "player"), positionSubject(12, "monster")
	for _, state := range []*entity.SubjectSyncState{a, b} {
		if err := manager.Register(state); err != nil {
			t.Fatal(err)
		}
	}
	open(t, manager, 1)
	mustSubscribe(t, manager, 1, 11, entity.SyncProfile{})
	mustSubscribe(t, manager, 1, 12, entity.SyncProfile{Key: "near"})
	mustFlush(t, manager)
	a.MarkDirty(0x20)
	mustFlush(t, manager)
	a.MarkDirty(0x20)
	b.MarkDirty(0x20)
	mustFlush(t, manager)
	frames := transport.take(1)
	if len(frames) != 3 {
		t.Fatalf("frames = %d, want 3", len(frames))
	}
	snapshot, first, second := frames[0], frames[1], frames[2]

	decoder := NewDecoder(frame.DefaultLimits())
	got, err := decoder.Decode(snapshot)
	if err != nil || got.Kind != frame.Full || len(got.Objects) != 2 {
		t.Fatalf("snapshot = %+v, %v", got, err)
	}
	if _, err := decoder.Decode(second); err == nil {
		t.Fatal("a frame past a gap was accepted")
	}
	if _, err := decoder.Decode(first); err == nil {
		t.Fatal("after a refused frame the decoder accepted a delta instead of waiting for a full")
	}

	decoder = NewDecoder(frame.DefaultLimits())
	for _, raw := range [][]byte{snapshot, first, second} {
		if _, err := decoder.Decode(raw); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := decoder.Decode(first); !errors.Is(err, ErrStaleFrame) {
		t.Fatalf("a replayed frame = %v, want ErrStaleFrame", err)
	}
	decoder = NewDecoder(frame.DefaultLimits())
	if _, err := decoder.Decode(snapshot); err != nil {
		t.Fatal(err)
	}
	got, err = decoder.Decode(first)
	if err != nil || len(got.Objects) != 1 {
		t.Fatalf("first step = %+v, %v", got, err)
	}
	update := got.Objects[0].Components[0].Update
	if update.SubjectID != 11 || update.Namespace != "player" || update.Profile.Key != "default" || update.SubjectKind != 1 ||
		update.Full || update.Version != update.BaseVersion+1 || update.Payload.Codec() != 2 || !bytes.Equal(update.Payload.BytesCopy(), positionPatch) {
		t.Fatalf("the delta did not inherit the identity of its baseline: %+v", update)
	}
	got, err = decoder.Decode(second)
	if err != nil || len(got.Objects) != 2 {
		t.Fatalf("second step = %+v, %v", got, err)
	}
	for _, object := range got.Objects {
		want := map[int64]string{11: "default", 12: "near"}[object.SubjectID]
		if profile := object.Components[0].Update.Profile.Key; want == "" || profile != want {
			t.Fatalf("object %v: subject %d profile %q", object.Ref, object.SubjectID, profile)
		}
	}

	// 同一帧换掉对象引用指向的 subject、或基线版本对不上：编出这样的帧，解码器拒绝。
	held := got.Objects[0].Ref
	forged := func(update entity.SubjectSyncUpdate) []byte {
		data, err := EncodeSubjectUpdate(update, 0)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := frame.Encode(frame.Frame{SnapshotMeta: frame.SnapshotMeta{StreamID: wireStream, Epoch: got.Epoch, Tick: got.Tick + 1, SchemaVersion: got.SchemaVersion},
			Kind: frame.Delta, BaseTick: got.Tick, Objects: []frame.ObjectDelta{{Operation: frame.ObjectUpdate, Ref: held,
				Components: []frame.ComponentDelta{{Operation: frame.ComponentSet, TypeID: 1, SchemaVersion: 1, Data: data}}}}}, frame.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	current := got.Objects[0].Components[0].Update
	for name, raw := range map[string][]byte{
		"another subject on the ref": forged(entity.SubjectSyncUpdate{SubjectID: current.SubjectID + 1, Version: 1, Full: true, Payload: entity.CopyFrozenSyncPayload(2, []byte("x"))}),
		"a delta on an old version":  forged(entity.SubjectSyncUpdate{SubjectID: 1, Version: current.Version, BaseVersion: current.Version - 1}),
	} {
		if _, err := decoder.Decode(raw); err == nil {
			t.Fatalf("%s: accepted", name)
		}
		decoder = NewDecoder(frame.DefaultLimits())
		for _, ok := range [][]byte{snapshot, first, second} {
			if _, err := decoder.Decode(ok); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := decoder.Decode(forged(entity.SubjectSyncUpdate{SubjectID: 1, Version: current.Version + 1, BaseVersion: current.Version})); err != nil {
		t.Fatalf("a delta continuing the baseline was refused: %v", err)
	}
}
