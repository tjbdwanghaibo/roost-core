package entitysync

import (
	"encoding/binary"
	"errors"
	"fmt"
	"maps"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
)

// WireVersion is the subject-update encoding version (the data of each frame
// component). Frames are per session: a frame is a plain frame.Frame whose
// Epoch/Tick are the session's own clock, with no outer shared header, and
// StreamID carries nothing but the stream constant below. Version 3 travels
// in frame protocol 2 and is implied by it; the envelope itself carries no
// magic or version.
const WireVersion uint16 = 3

// wireStream is what goes into SnapshotMeta.StreamID. frame requires it to
// be non-zero and equal between a delta and its base; a session has exactly
// one stream, so a constant does.
const wireStream uint64 = 1

// subject 信封 v3 的首字节（flags）。
const (
	subjectFlagFull   uint8 = 1 << 0
	subjectFlagMask   uint8 = 1 << 1 // Mask 非零，随后一个 uvarint
	subjectFlagReason uint8 = 1 << 2 // Reason 非零，随后一个 uvarint
	subjectFlagsKnown       = subjectFlagFull | subjectFlagMask | subjectFlagReason
)

// minFullEnvelopeBytes 是一个全量信封可能的最小长度（flags 与九个一字节字段），只用于从未捕获过的冷创建的预估。
const minFullEnvelopeBytes = 10

// EncodeSubjectUpdate 把一个 subject 的更新编码成一个帧组件的数据（信封 v3，见 docs/framework/ENTITY_SYNC.md“线格式”）：
//
//	flags(1)
//	全量  uvarint SubjectID、SubjectKind、Version、BaseVersion、[Mask]、[Reason]、Codec，LOD(1)，uvarint Profile.SchemaVersion，
//	      uvarint 长度 + Namespace，uvarint 长度 + Profile.Key（"default" 写成空串），载荷
//	增量  uvarint BaseVersion、Version−BaseVersion（≥1）、[Mask]、[Reason]，载荷
//
// 载荷是组件数据的剩余部分，不另写长度。增量不带身份（SubjectID、Kind、Namespace、Profile、Codec），
// 接收端从同一对象引用、同一组件类型上的基线继承（Decoder）：服务端只在会话已持有同一 subject、同一 profile
// 的基线时发增量，换 profile、恢复、首次持有都发全量；对象引用在一个 epoch 内只属于一个 subject（代际区分复用）。
// BaseVersion 仍随增量下发，接收端用它核对基线（不连续就要求全量），乱序与重放过不了这一关。
// packer 对同一 profile 必须用同一 codec：增量的非空载荷沿用基线的 codec。
func EncodeSubjectUpdate(update entity.SubjectSyncUpdate, maxBytes int) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = frame.DefaultLimits().MaxComponentBytes
	}
	if update.SubjectID == 0 || !update.Full && update.Version <= update.BaseVersion {
		return nil, fmt.Errorf("%w: subject %d, delta version %d over base %d", ErrWireFrame, update.SubjectID, update.Version, update.BaseVersion)
	}
	total := subjectUpdateBytes(update)
	if total > maxBytes {
		return nil, frame.ErrComponentTooLarge
	}
	out := make([]byte, 1, total)
	flags := uint8(0)
	if update.Full {
		flags |= subjectFlagFull
	}
	if update.Mask != 0 {
		flags |= subjectFlagMask
	}
	if update.Reason != 0 {
		flags |= subjectFlagReason
	}
	out[0] = flags
	if update.Full {
		profile := update.Profile.Normalize()
		out = binary.AppendUvarint(out, uint64(update.SubjectID))
		out = binary.AppendUvarint(out, uint64(update.SubjectKind))
		out = binary.AppendUvarint(out, update.Version)
		out = binary.AppendUvarint(out, update.BaseVersion)
		out = appendOptional(out, flags, update.Mask, update.Reason)
		out = binary.AppendUvarint(out, uint64(update.Payload.Codec()))
		out = append(out, profile.LOD)
		out = binary.AppendUvarint(out, uint64(profile.SchemaVersion))
		out = binary.AppendUvarint(out, uint64(len(update.Namespace)))
		out = append(out, update.Namespace...)
		key := wireProfileKey(profile.Key)
		out = binary.AppendUvarint(out, uint64(len(key)))
		out = append(out, key...)
	} else {
		out = binary.AppendUvarint(out, update.BaseVersion)
		out = binary.AppendUvarint(out, update.Version-update.BaseVersion)
		out = appendOptional(out, flags, update.Mask, update.Reason)
	}
	return update.Payload.AppendTo(out), nil
}

func appendOptional(out []byte, flags uint8, mask uint64, reason uint32) []byte {
	if flags&subjectFlagMask != 0 {
		out = binary.AppendUvarint(out, mask)
	}
	if flags&subjectFlagReason != 0 {
		out = binary.AppendUvarint(out, uint64(reason))
	}
	return out
}

// wireProfileKey：缺省视图键在线上写成空串，接收端按 SyncProfile.Normalize 还原。
func wireProfileKey(key string) string {
	if key == "default" {
		return ""
	}
	return key
}

func uvarintLen(value uint64) int {
	n := 1
	for value >= 0x80 {
		value >>= 7
		n++
	}
	return n
}

// subjectUpdateBytes 是 EncodeSubjectUpdate 的输出长度，只用于预算预估，不做上限校验。
func subjectUpdateBytes(update entity.SubjectSyncUpdate) int {
	n := 1 + update.Payload.Len()
	if update.Mask != 0 {
		n += uvarintLen(update.Mask)
	}
	if update.Reason != 0 {
		n += uvarintLen(uint64(update.Reason))
	}
	if !update.Full {
		if update.Version <= update.BaseVersion {
			return n + 2
		}
		return n + uvarintLen(update.BaseVersion) + uvarintLen(update.Version-update.BaseVersion)
	}
	profile := update.Profile.Normalize()
	key := wireProfileKey(profile.Key)
	return n + uvarintLen(uint64(update.SubjectID)) + uvarintLen(uint64(update.SubjectKind)) + uvarintLen(update.Version) +
		uvarintLen(update.BaseVersion) + uvarintLen(uint64(update.Payload.Codec())) + 1 + uvarintLen(uint64(profile.SchemaVersion)) +
		uvarintLen(uint64(len(update.Namespace))) + len(update.Namespace) + uvarintLen(uint64(len(key))) + len(key)
}

// envelopeReader 读信封；截断、超长或非最短的 varint 都是 ErrWireFrame。
type envelopeReader struct {
	data []byte
	at   int
}

func (r *envelopeReader) uvarint(limit uint64) (uint64, error) {
	value, n := binary.Uvarint(r.data[r.at:])
	if n <= 0 || value > limit || n != uvarintLen(value) {
		return 0, fmt.Errorf("%w: malformed subject envelope", ErrWireFrame)
	}
	r.at += n
	return value, nil
}

func (r *envelopeReader) text(limit int) (string, error) {
	length, err := r.uvarint(uint64(limit))
	if err != nil {
		return "", err
	}
	if length > uint64(len(r.data)-r.at) {
		return "", fmt.Errorf("%w: truncated subject envelope", ErrWireFrame)
	}
	value := string(r.data[r.at : r.at+int(length)])
	r.at += int(length)
	return value, nil
}

// decodeSubjectUpdate 是 EncodeSubjectUpdate 的逆。增量没有身份，调用方（Decoder）传入同一对象、同一组件上的基线，
// 身份从它继承，BaseVersion 必须等于基线版本；全量不需要基线。
func decodeSubjectUpdate(data []byte, maxBytes int, baseline *entity.SubjectSyncUpdate) (entity.SubjectSyncUpdate, error) {
	if maxBytes <= 0 {
		maxBytes = frame.DefaultLimits().MaxComponentBytes
	}
	if len(data) == 0 || len(data) > maxBytes || data[0]&^subjectFlagsKnown != 0 {
		return entity.SubjectSyncUpdate{}, fmt.Errorf("%w: bad subject envelope flags", ErrWireFrame)
	}
	flags := data[0]
	r := &envelopeReader{data: data, at: 1}
	var update entity.SubjectSyncUpdate
	var err error
	readOptional := func() error {
		if flags&subjectFlagMask != 0 {
			if update.Mask, err = r.uvarint(^uint64(0)); err != nil {
				return err
			}
			if update.Mask == 0 {
				return fmt.Errorf("%w: mask flag with a zero mask", ErrWireFrame)
			}
		}
		if flags&subjectFlagReason != 0 {
			reason, err := r.uvarint(uint64(^uint32(0)))
			if err != nil {
				return err
			}
			if reason == 0 {
				return fmt.Errorf("%w: reason flag with a zero reason", ErrWireFrame)
			}
			update.Reason = uint32(reason)
		}
		return nil
	}
	codec := uint64(0)
	if flags&subjectFlagFull != 0 {
		update.Full = true
		id, err := r.uvarint(^uint64(0))
		if err != nil {
			return entity.SubjectSyncUpdate{}, err
		}
		update.SubjectID = int64(id)
		kind, err := r.uvarint(uint64(^uint32(0)))
		if err != nil {
			return entity.SubjectSyncUpdate{}, err
		}
		update.SubjectKind = uint32(kind)
		if update.Version, err = r.uvarint(^uint64(0)); err != nil {
			return entity.SubjectSyncUpdate{}, err
		}
		if update.BaseVersion, err = r.uvarint(^uint64(0)); err != nil {
			return entity.SubjectSyncUpdate{}, err
		}
		if err := readOptional(); err != nil {
			return entity.SubjectSyncUpdate{}, err
		}
		if codec, err = r.uvarint(uint64(^uint16(0))); err != nil {
			return entity.SubjectSyncUpdate{}, err
		}
		if r.at >= len(data) {
			return entity.SubjectSyncUpdate{}, fmt.Errorf("%w: truncated subject envelope", ErrWireFrame)
		}
		lod := data[r.at]
		r.at++
		schema, err := r.uvarint(uint64(^uint32(0)))
		if err != nil {
			return entity.SubjectSyncUpdate{}, err
		}
		if update.Namespace, err = r.text(int(^uint16(0))); err != nil {
			return entity.SubjectSyncUpdate{}, err
		}
		key, err := r.text(int(^uint16(0)))
		if err != nil {
			return entity.SubjectSyncUpdate{}, err
		}
		update.Profile = entity.SyncProfile{Key: key, LOD: lod, SchemaVersion: uint32(schema)}.Normalize()
		if update.SubjectID == 0 {
			return entity.SubjectSyncUpdate{}, fmt.Errorf("%w: subject id is zero", ErrWireFrame)
		}
	} else {
		if baseline == nil {
			return entity.SubjectSyncUpdate{}, fmt.Errorf("%w: delta without a baseline", ErrWireFrame)
		}
		if update.BaseVersion, err = r.uvarint(^uint64(0)); err != nil {
			return entity.SubjectSyncUpdate{}, err
		}
		step, err := r.uvarint(^uint64(0) - update.BaseVersion)
		if err != nil {
			return entity.SubjectSyncUpdate{}, err
		}
		if step == 0 {
			return entity.SubjectSyncUpdate{}, fmt.Errorf("%w: delta does not advance the version", ErrWireFrame)
		}
		update.Version = update.BaseVersion + step
		if update.BaseVersion != baseline.Version {
			return entity.SubjectSyncUpdate{}, fmt.Errorf("%w: subject %d delta on version %d, baseline is %d", ErrWireFrame, baseline.SubjectID, update.BaseVersion, baseline.Version)
		}
		if err := readOptional(); err != nil {
			return entity.SubjectSyncUpdate{}, err
		}
		update.SubjectID, update.SubjectKind, update.Namespace, update.Profile = baseline.SubjectID, baseline.SubjectKind, baseline.Namespace, baseline.Profile
		codec = uint64(baseline.Payload.Codec())
	}
	if payload := data[r.at:]; len(payload) > 0 {
		update.Payload = entity.CopyFrozenSyncPayload(uint16(codec), payload)
	} else if update.Full {
		update.Payload = entity.CopyFrozenSyncPayload(uint16(codec), nil)
	}
	return update, nil
}

// DecodeFrame decodes the outer frame only: object refs and operations, with
// each component's data still the subject envelope. A delta envelope carries
// no identity, so reading subjects needs the stream's history: use a Decoder.
func DecodeFrame(data []byte, limits frame.Limits) (frame.Frame, error) {
	return frame.Decode(data, limits)
}

// ErrStaleFrame 是 Decoder 对旧代或重复帧的回答：没有应用、状态不变，接收端忽略它即可。
var ErrStaleFrame = errors.New("entitysync: stale or duplicate frame")

// ReceivedFrame 是 Decoder 解出的一帧：外层帧的元数据与每个对象，组件数据已解成完整的 subject 更新。
type ReceivedFrame struct {
	frame.SnapshotMeta
	Kind     frame.Kind
	BaseTick uint32
	Objects  []ReceivedObject
}

// ReceivedObject 是一帧里的一个对象。SubjectID 对 create、update 与 remove 都给出（remove 的取自接收端记住的对象表）。
type ReceivedObject struct {
	Operation  frame.ObjectOperation
	Ref        frame.ObjectRef
	Archetype  uint16
	SubjectID  int64
	Components []ReceivedComponent
}

// ReceivedComponent 是对象的一个组件。Set 时 Update 是完整的更新：增量的身份字段（SubjectID、Kind、Namespace、
// Profile、载荷 codec）取自基线；Remove 时 Update 是被移除前最后的身份与版本，没有载荷。
type ReceivedComponent struct {
	Operation frame.ComponentOperation
	TypeID    uint16
	Update    entity.SubjectSyncUpdate
	// Size 是组件数据（subject 信封加载荷）在线上的字节数，不含帧与组件头，供测量。
	Size int
}

// receivedObject 是 Decoder 为一个对象引用记住的东西：代际、subject 与每个组件的基线（不含载荷）。
type receivedObject struct {
	generation uint16
	subjectID  int64
	components map[uint16]entity.SubjectSyncUpdate
}

// Decoder 是一个会话的接收端（客户端、机器人、测试）：按到达顺序解一条流的帧。它维护 frame.Receiver 的帧基线
// 与对象表，增量从同一对象引用、同一组件类型上的基线继承身份并核对 BaseVersion；全量帧先清空对象表。
// 任何不一致（缺口、对象代际不符、版本不连续、对象引用换了 subject）返回错误且状态不变，之后只接受新的全量帧
// （与 frame.Receiver 相同）；旧代或重复帧返回 ErrStaleFrame。一个 Decoder 只给一条流用，调用方串行调用。
type Decoder struct {
	receiver *frame.Receiver
	limits   frame.Limits
	objects  map[uint16]*receivedObject
}

func NewDecoder(limits frame.Limits) *Decoder {
	return &Decoder{receiver: frame.NewReceiver(limits), limits: limits, objects: make(map[uint16]*receivedObject)}
}

// Reset 放弃全部状态：之后只接受全量帧。
func (d *Decoder) Reset() {
	d.receiver.Reset()
	d.objects = make(map[uint16]*receivedObject)
}

// Decode 解一帧并推进状态；只有整帧都解成功才推进。
func (d *Decoder) Decode(data []byte) (ReceivedFrame, error) {
	var out ReceivedFrame
	var next map[uint16]*receivedObject
	applied, err := d.receiver.Receive(data, func(f frame.Frame) error {
		var err error
		out, next, err = d.apply(f)
		return err
	})
	if err != nil {
		return ReceivedFrame{}, err
	}
	if !applied {
		return ReceivedFrame{}, ErrStaleFrame
	}
	d.objects = next
	return out, nil
}

func (d *Decoder) apply(f frame.Frame) (ReceivedFrame, map[uint16]*receivedObject, error) {
	objects := make(map[uint16]*receivedObject, len(d.objects))
	if f.Kind != frame.Full {
		for id, object := range d.objects {
			objects[id] = object
		}
	}
	// 本帧改动的对象先复制一份，失败时 d.objects 原样保留。
	owned := make(map[uint16]bool, len(f.Objects))
	out := ReceivedFrame{SnapshotMeta: f.SnapshotMeta, Kind: f.Kind, BaseTick: f.BaseTick, Objects: make([]ReceivedObject, 0, len(f.Objects))}
	for _, object := range f.Objects {
		current, exists := objects[object.Ref.ID]
		switch {
		case object.Operation == frame.ObjectCreate:
			if exists {
				return ReceivedFrame{}, nil, fmt.Errorf("%w: create of object %d which is held", ErrWireFrame, object.Ref.ID)
			}
			current = &receivedObject{generation: object.Ref.Generation, components: make(map[uint16]entity.SubjectSyncUpdate, len(object.Components))}
			objects[object.Ref.ID], owned[object.Ref.ID] = current, true
		case !exists || current.generation != object.Ref.Generation:
			return ReceivedFrame{}, nil, fmt.Errorf("%w: object %d/%d is not held", ErrWireFrame, object.Ref.ID, object.Ref.Generation)
		case !owned[object.Ref.ID]:
			current = &receivedObject{generation: current.generation, subjectID: current.subjectID, components: maps.Clone(current.components)}
			objects[object.Ref.ID], owned[object.Ref.ID] = current, true
		}
		received := ReceivedObject{Operation: object.Operation, Ref: object.Ref, Archetype: object.Archetype, SubjectID: current.subjectID}
		if object.Operation == frame.ObjectRemove {
			delete(objects, object.Ref.ID)
			out.Objects = append(out.Objects, received)
			continue
		}
		for _, component := range object.Components {
			baseline, known := current.components[component.TypeID]
			if component.Operation == frame.ComponentRemove {
				delete(current.components, component.TypeID)
				received.Components = append(received.Components, ReceivedComponent{Operation: component.Operation, TypeID: component.TypeID, Update: baseline})
				continue
			}
			var base *entity.SubjectSyncUpdate
			if known {
				base = &baseline
			}
			update, err := decodeSubjectUpdate(component.Data, d.limits.MaxComponentBytes, base)
			if err != nil {
				return ReceivedFrame{}, nil, fmt.Errorf("object %d/%d: %w", object.Ref.ID, object.Ref.Generation, err)
			}
			// 对象引用在持有期间只属于一个 subject。
			if current.subjectID == 0 {
				current.subjectID = update.SubjectID
			} else if update.SubjectID != current.subjectID {
				return ReceivedFrame{}, nil, fmt.Errorf("%w: object %d/%d held subject %d, now %d", ErrWireFrame, object.Ref.ID, object.Ref.Generation, current.subjectID, update.SubjectID)
			}
			kept := update
			kept.Payload = entity.CopyFrozenSyncPayload(update.Payload.Codec(), nil)
			if !update.Full && update.Payload.Len() == 0 {
				kept.Payload = baseline.Payload // 空载荷的增量不改变基线的 codec
			}
			current.components[component.TypeID] = kept
			received.SubjectID = current.subjectID
			received.Components = append(received.Components, ReceivedComponent{Operation: component.Operation, TypeID: component.TypeID, Update: update, Size: len(component.Data)})
		}
		if object.Operation == frame.ObjectCreate && current.subjectID == 0 {
			return ReceivedFrame{}, nil, fmt.Errorf("%w: create of object %d carries no subject", ErrWireFrame, object.Ref.ID)
		}
		out.Objects = append(out.Objects, received)
	}
	return out, objects, nil
}

// capturedUpdate 把不可变捕获和本 tick 的编码缓存放在一起。
// 同 subject/profile 的多个会话引用同一项，full 和 delta 来自不同捕获列表。
// Flush 串行编码且使用相同 Limits；无需锁，也不跨 tick 保存缓存。
// frame.Encode 复制组件到独立帧，Transport 不会拿到缓存的底层数组。
type capturedUpdate struct {
	update  entity.SubjectSyncUpdate
	encoded []byte
	err     error
	ready   bool
}

// captureUpdates 把 updates 追加进本轮捕获缓冲并返回新追加的部分。
// 扩容不会使已分发的指针失效：旧数组由 frameEntry 持有，后续只追加新捕获。
func captureUpdates(buffer *[]capturedUpdate, updates []entity.SubjectSyncUpdate) []capturedUpdate {
	start := len(*buffer)
	for _, update := range updates {
		*buffer = append(*buffer, capturedUpdate{update: update})
	}
	return (*buffer)[start:]
}

func updateFor(updates []capturedUpdate, profile entity.SyncProfile) (*capturedUpdate, bool) {
	profile = profile.Normalize()
	for i := range updates {
		if updates[i].update.Profile.Normalize() == profile {
			return &updates[i], true
		}
	}
	return nil, false
}

func (captured *capturedUpdate) encode(maxBytes int) ([]byte, error) {
	if !captured.ready {
		captured.encoded, captured.err = EncodeSubjectUpdate(captured.update, maxBytes)
		captured.ready = true
	}
	return captured.encoded, captured.err
}
