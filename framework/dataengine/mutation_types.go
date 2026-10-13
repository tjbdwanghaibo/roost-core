package dataengine

import (
	"fmt"
	"slices"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// TransactionID identifies one durable transaction.
type TransactionID [16]byte

func (id TransactionID) IsZero() bool { return id == TransactionID{} }

func (id TransactionID) String() string { return fmt.Sprintf("%x", id[:]) }

// Durability 是事务记录（WAL、投影、远端 outcome）上的持久化级别，零值即 memory。
// handler 声明用的是 nest.DurabilityPolicy（零值表示“未声明”），nest 在建记录时换算过来。
type Durability uint8

const (
	DurabilityMemory Durability = iota
	DurabilityAsync
	DurabilityStrict
	DurabilityPipelined
)

func (durability Durability) String() string {
	switch durability {
	case DurabilityAsync:
		return "async"
	case DurabilityStrict:
		return "strict"
	case DurabilityPipelined:
		return "pipelined"
	default:
		return "memory"
	}
}

type MutationKind uint8

const (
	MutationPut MutationKind = iota + 1
	MutationPatch
	MutationDelete
)

type DatabaseScope = entity.DatabaseScope

const (
	DatabaseGlobal = entity.DatabaseGlobal
	DatabaseServer = entity.DatabaseServer
)

type DocumentKey struct {
	Database string
	Scope    DatabaseScope
	Resource string
	ID       int64
}

// FieldPatch is an immutable field-level update. SetBSON is a BSON document
// whose keys are update paths and whose values are their new values.
type FieldPatch struct {
	SetBSON []byte
	Unset   []string
}

func (p FieldPatch) Empty() bool { return len(p.SetBSON) == 0 && len(p.Unset) == 0 }

const AllFields uint64 = ^uint64(0)

// Mutation 描述一个文档的一次变更：唯一文档身份（Key）加预期/目标版本，
// 内容是整文档（Put）、字段补丁（Patch）或删除；Remote 非空时内容由远端提交描述。
type Mutation struct {
	Key             DocumentKey
	Kind            MutationKind
	ExpectedVersion uint64
	NextVersion     uint64
	Mask            uint64
	Schema          uint32
	Codec           string
	Data            []byte
	Patch           FieldPatch
	Remote          *entity.RemoteCommit
}

type Effect struct {
	ID          string
	Topic       string
	Key         string
	Payload     []byte
	Headers     map[string]string
	AvailableAt int64
}

type Receipt struct {
	Namespace string
	ID        string
	Digest    []byte
	Payload   []byte
	ExpiresAt int64
}

// CommitRecord is the atomic logical transaction admitted to the WAL.
type CommitRecord struct {
	ID         TransactionID
	Handler    string
	RequestID  string
	CreatedAt  int64
	Durability Durability
	Mutations  []Mutation
	Effects    []Effect
	Receipts   []Receipt
}

func (r CommitRecord) Empty() bool {
	return len(r.Mutations) == 0 && len(r.Effects) == 0 && len(r.Receipts) == 0
}

func CloneMutation(m Mutation) Mutation {
	m.Data = slices.Clone(m.Data)
	m.Patch.SetBSON = slices.Clone(m.Patch.SetBSON)
	m.Patch.Unset = slices.Clone(m.Patch.Unset)
	if m.Remote != nil {
		remote := m.Remote.Clone()
		m.Remote = &remote
	}
	return m
}

func CloneEffect(effect Effect) Effect {
	effect.Payload = slices.Clone(effect.Payload)
	if effect.Headers != nil {
		headers := make(map[string]string, len(effect.Headers))
		for key, value := range effect.Headers {
			headers[key] = value
		}
		effect.Headers = headers
	}
	return effect
}

func CloneReceipt(receipt Receipt) Receipt {
	receipt.Digest = slices.Clone(receipt.Digest)
	receipt.Payload = slices.Clone(receipt.Payload)
	return receipt
}

func CloneCommitRecord(record CommitRecord) CommitRecord {
	record.Mutations = slices.Clone(record.Mutations)
	for i := range record.Mutations {
		record.Mutations[i] = CloneMutation(record.Mutations[i])
	}
	record.Effects = slices.Clone(record.Effects)
	for i := range record.Effects {
		record.Effects[i] = CloneEffect(record.Effects[i])
	}
	record.Receipts = slices.Clone(record.Receipts)
	for i := range record.Receipts {
		record.Receipts[i] = CloneReceipt(record.Receipts[i])
	}
	return record
}

// SyncFieldMeta 描述 DAO 的一个同步字段，由代码生成器写在字段掩码旁边，二者同源不会漂移。
//
// 生成的逐字段掩码常量是 DAO 包私有的；自定义客户端协议的 packer 不在该包内，
// 需要靠这张只读表把脏掩码翻译成字段名。
//
// Bit 只在同一次生成的 schema 内稳定：删除或调整字段顺序会改变位号。
// 生存期超过本次构建的值（持久化的投影、客户端缓存的布局）必须按 Name 或
// WireName 记录，不能按 Bit 记录；这里不是 ABI。
type SyncFieldMeta struct {
	// Name is the Go field name on the DAO definition.
	Name string
	// WireName is the field's name in the document (its bson key), which is
	// also what MarshalSync writes.
	WireName string
	// Bit is the mask bit the generated setters raise for this field.
	Bit uint64
}

// SyncFieldByName looks one field up in a generated table. It is a helper
// rather than a method so the generated code stays a plain slice.
func SyncFieldByName(fields []SyncFieldMeta, name string) (SyncFieldMeta, bool) {
	for _, field := range fields {
		if field.Name == name || field.WireName == name {
			return field, true
		}
	}
	return SyncFieldMeta{}, false
}

// SyncFieldsOf returns the fields a mask names, in declaration order.
func SyncFieldsOf(fields []SyncFieldMeta, mask uint64) []SyncFieldMeta {
	var named []SyncFieldMeta
	for _, field := range fields {
		if mask&field.Bit != 0 {
			named = append(named, field)
		}
	}
	return named
}
