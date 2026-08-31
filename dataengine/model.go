// Package dataengine defines the durable, infrastructure-neutral transaction
// model shared by Nest, WAL implementations, projectors, loaders, and codegen.
package dataengine

import (
	"fmt"
	"slices"

	"github.com/tjbdwanghaibo/cube-core/entity"
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

type DatabaseScope uint8

const (
	DatabaseGlobal DatabaseScope = iota
	DatabaseServer
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
		effect.Headers = make(map[string]string, len(effect.Headers))
		for key, value := range effect.Headers {
			effect.Headers[key] = value
		}
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
