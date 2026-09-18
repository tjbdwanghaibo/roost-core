// Package dataengine defines the durable, infrastructure-neutral transaction
// model shared by Nest, WAL implementations, projectors, loaders, and codegen.
package dataengine

import (
	"fmt"
	"slices"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// TransactionID identifies one durable transaction.
type TransactionID [16]byte

func (id TransactionID) IsZero() bool { return id == TransactionID{} }

func (id TransactionID) String() string { return fmt.Sprintf("%x", id[:]) }

type Durability uint8

func (durability Durability) String() string {
	switch durability {
	case 1:
		return "async"
	case 2:
		return "strict"
	case 3:
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

// Mutation is canonical when Key, Kind, ExpectedVersion, and NextVersion are
// populated and all deprecated compatibility fields are zero.
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

	// Deprecated compatibility fields for generated v1 callers. WAL v2 must
	// only encode the canonical fields above.
	EntityID      int64
	Database      string
	DatabaseScope uint8
	Resource      string
	Version       uint64
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

// SyncFieldMeta names one replicated field of a DAO.
//
// It exists because a game with its own client protocol has to decide what a
// dirty mask means, and the generated per-field bit constants are private to
// the DAO's package — a packer written anywhere else can pass the mask
// through to MarshalSync but cannot look inside it (ARCH-06, from
// W-2026-09-18-02). This is the vocabulary: read-only, generated next to the
// masks it describes, so the two cannot drift.
//
// Bit is stable only within ONE generated schema. Adding a field renumbers
// nothing, but removing or reordering one does — so a value that outlives the
// build (a stored projection, a client's cached layout) must be keyed by Name
// or WireName, never by Bit. Nothing here is an ABI.
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
