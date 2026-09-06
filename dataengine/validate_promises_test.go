package dataengine

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

func validPut() Mutation {
	return Mutation{
		Key: DocumentKey{Database: "game", Resource: "hero", ID: 7}, Kind: MutationPut,
		ExpectedVersion: 3, NextVersion: 4, Data: []byte(`{"hp":1}`),
	}
}

func validRemote() Mutation {
	commit := &entity.RemoteCommit{
		TransactionID: entity.RemoteTransactionID{1}, EntityID: 7, Kind: entity.EntityKind(2),
		BaseVersion: 3, NextVersion: 4, MarkerEpoch: 1, RouteEpoch: 1,
		Mutations: []entity.RemoteDataMutation{{Database: "game", Collection: "hero", ID: 7, Version: 4, Data: []byte(`{"hp":1}`)}},
	}
	return Mutation{
		Key: DocumentKey{Resource: "remote_entity", ID: 7}, Kind: MutationPut,
		ExpectedVersion: 3, NextVersion: 4, Remote: commit,
	}
}

// ValidateMutation is the admission gate for everything the WAL will replay
// and the projector will write. Each rule is a promise about what can never
// reach Mongo; only the version rule and the patch fallback had tests.
func TestValidateMutationRefusesEachMalformedShape(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Mutation)
		want   error
	}{
		{"legacy identity fields mixed in", func(m *Mutation) { m.EntityID = 7 }, ErrMixedMutationForms},
		{"zero document id", func(m *Mutation) { m.Key.ID = 0 }, ErrInvalidDocumentKey},
		{"empty resource", func(m *Mutation) { m.Key.Resource = "" }, ErrInvalidDocumentKey},
		{"local mutation without database", func(m *Mutation) { m.Key.Database = "" }, ErrInvalidDocumentKey},
		{"put without data", func(m *Mutation) { m.Data = nil }, ErrInvalidPut},
		{"put carrying a patch", func(m *Mutation) { m.Patch = FieldPatch{Unset: []string{"hp"}} }, ErrInvalidPut},
		{"patch on version zero", func(m *Mutation) {
			m.Kind, m.Data, m.Patch, m.ExpectedVersion, m.NextVersion = MutationPatch, nil, FieldPatch{Unset: []string{"hp"}}, 0, 1
		}, ErrInvalidPatch},
		{"patch carrying data", func(m *Mutation) { m.Kind, m.Patch = MutationPatch, FieldPatch{Unset: []string{"hp"}} }, ErrInvalidPatch},
		{"empty patch", func(m *Mutation) { m.Kind, m.Data = MutationPatch, nil }, ErrInvalidPatch},
		{"delete carrying data", func(m *Mutation) { m.Kind = MutationDelete }, ErrInvalidDelete},
		{"delete carrying a patch", func(m *Mutation) { m.Kind, m.Data, m.Patch = MutationDelete, nil, FieldPatch{Unset: []string{"hp"}} }, ErrInvalidDelete},
		{"unknown kind", func(m *Mutation) { m.Kind = MutationKind(99) }, ErrInvalidMutationKind},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validPut()
			tc.mutate(&m)
			if err := ValidateMutation(m); !errors.Is(err, tc.want) {
				t.Fatalf("ValidateMutation = %v, want %v", err, tc.want)
			}
		})
	}
	if err := ValidateMutation(validPut()); err != nil {
		t.Fatalf("valid put rejected: %v", err)
	}
}

// A remote-entity mutation carries the commit twice: in the mutation header
// and inside RemoteCommit. The two must agree, and the shape must match the
// commit's own delete flag — otherwise the projector would write one thing
// and the remote committer another.
func TestValidateMutationCrossChecksRemoteCommitAgainstHeader(t *testing.T) {
	if err := ValidateMutation(validRemote()); err != nil {
		t.Fatalf("valid remote mutation rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*Mutation)
		want   error
	}{
		{"header id disagrees with commit", func(m *Mutation) { m.Key.ID = 8 }, ErrInvalidVersion},
		{"header versions disagree with commit", func(m *Mutation) { m.ExpectedVersion, m.NextVersion = 4, 5 }, ErrInvalidVersion},
		{"delete commit under a put header", func(m *Mutation) {
			m.Remote.Delete, m.Remote.Mutations, m.Remote.Deletes = true, nil, []entity.RemoteDataDelete{{Database: "game", Collection: "hero", ID: 7}}
		}, ErrInvalidMutationKind},
		{"put commit under a delete header", func(m *Mutation) { m.Kind = MutationDelete }, ErrInvalidMutationKind},
		{"remote mutation carrying local data", func(m *Mutation) { m.Data = []byte("x") }, ErrInvalidMutationKind},
		{"remote commit invalid on its own", func(m *Mutation) { m.Remote.MarkerEpoch = 0 }, entity.ErrRemoteFenced},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validRemote()
			commit := *m.Remote
			m.Remote = &commit
			tc.mutate(&m)
			if err := ValidateMutation(m); !errors.Is(err, tc.want) {
				t.Fatalf("ValidateMutation = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestValidateCommitRecordRefusesEachMalformedPart(t *testing.T) {
	record := CommitRecord{ID: TransactionID{1}, Mutations: []Mutation{validPut()}, Effects: []Effect{{ID: "e1", Topic: "hero"}}, Receipts: []Receipt{{Namespace: "ns", ID: "r1"}}}
	if err := ValidateCommitRecord(record); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*CommitRecord)
		want   string
	}{
		{"zero transaction id", func(r *CommitRecord) { r.ID = TransactionID{} }, "zero transaction id"},
		{"invalid mutation is located", func(r *CommitRecord) { r.Mutations[0].Data = nil }, "invalid mutation 0"},
		{"effect without id", func(r *CommitRecord) { r.Effects[0].ID = "" }, "invalid effect 0"},
		{"effect without topic", func(r *CommitRecord) { r.Effects[0].Topic = "" }, "invalid effect 0"},
		{"receipt without namespace", func(r *CommitRecord) { r.Receipts[0].Namespace = "" }, "invalid receipt 0"},
		{"receipt without id", func(r *CommitRecord) { r.Receipts[0].ID = "" }, "invalid receipt 0"},
		{"lease fence receipt with a forged payload", func(r *CommitRecord) {
			r.Receipts[0] = Receipt{Namespace: LeaseFenceReceiptNamespace, ID: "game/hero/7", Payload: []byte(`{"database":"game"}`)}
		}, "invalid lease fence receipt 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := record
			r.Mutations = []Mutation{validPut()}
			r.Effects = []Effect{{ID: "e1", Topic: "hero"}}
			r.Receipts = []Receipt{{Namespace: "ns", ID: "r1"}}
			tc.mutate(&r)
			if err := ValidateCommitRecord(r); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateCommitRecord = %v, want %q", err, tc.want)
			}
		})
	}
}

// A lease fence with any blank identity, a zero token, or a digest of the
// wrong length must not become a receipt: the projector's predicate would
// match documents it was never meant to fence.
func TestLeaseFenceValidateRefusesEachIncompleteFence(t *testing.T) {
	digest := sha256.Sum256([]byte("cmd"))
	valid := LeaseFence{Database: "game", Resource: "mail", DocumentID: "m1", Owner: "sid-1", Token: 9, Digest: digest[:]}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid fence rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*LeaseFence)
	}{
		{"blank database", func(f *LeaseFence) { f.Database = " " }},
		{"blank resource", func(f *LeaseFence) { f.Resource = "" }},
		{"blank document", func(f *LeaseFence) { f.DocumentID = "" }},
		{"blank owner", func(f *LeaseFence) { f.Owner = "" }},
		{"zero token", func(f *LeaseFence) { f.Token = 0 }},
		{"short digest", func(f *LeaseFence) { f.Digest = f.Digest[:8] }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := valid
			tc.mutate(&f)
			if err := f.Validate(); !errors.Is(err, ErrInvalidLeaseFence) {
				t.Fatalf("Validate = %v, want ErrInvalidLeaseFence", err)
			}
			if _, err := NewLeaseFenceReceipt(f); !errors.Is(err, ErrInvalidLeaseFence) {
				t.Fatalf("NewLeaseFenceReceipt = %v, want ErrInvalidLeaseFence", err)
			}
		})
	}
}
