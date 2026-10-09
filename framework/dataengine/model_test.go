package dataengine

import (
	"errors"
	"testing"
)

func TestValidateMutationRequiresExactNextVersion(t *testing.T) {
	mutation := Mutation{
		Key:             DocumentKey{Database: "game", Resource: "hero", ID: 7},
		Kind:            MutationPatch,
		ExpectedVersion: 4,
		NextVersion:     6,
		Patch:           FieldPatch{SetBSON: []byte{1}},
	}
	if err := ValidateMutation(mutation); !errors.Is(err, ErrInvalidVersion) {
		t.Fatalf("err = %v, want ErrInvalidVersion", err)
	}
}

func TestValidatePatchRejectsFullFallback(t *testing.T) {
	mutation := Mutation{
		Key:             DocumentKey{Database: "game", Resource: "hero", ID: 7},
		Kind:            MutationPatch,
		ExpectedVersion: 4,
		NextVersion:     5,
		Data:            []byte{1},
		Patch:           FieldPatch{SetBSON: []byte{1}},
	}
	if err := ValidateMutation(mutation); !errors.Is(err, ErrInvalidPatch) {
		t.Fatalf("err = %v, want ErrInvalidPatch", err)
	}
}

func TestCommitRecordEmptyIncludesReceipts(t *testing.T) {
	if (CommitRecord{Receipts: []Receipt{{Namespace: "saga", ID: "step-1"}}}).Empty() {
		t.Fatal("record containing a receipt must not be empty")
	}
}

func TestValidateMutationRejectsUnsafePatchPath(t *testing.T) {
	mutation := Mutation{
		Key:             DocumentKey{Database: "game", Resource: "hero", ID: 7},
		Kind:            MutationPatch,
		ExpectedVersion: 4,
		NextVersion:     5,
		Patch:           FieldPatch{Unset: []string{"profile.$secret"}},
	}
	if err := ValidateMutation(mutation); !errors.Is(err, ErrInvalidPatchPath) {
		t.Fatalf("err = %v, want ErrInvalidPatchPath", err)
	}
}

func TestCloneEffectPreservesAndDetachesHeaders(t *testing.T) {
	original := Effect{Headers: map[string]string{"trace": "abc"}}
	cloned := CloneEffect(original)
	original.Headers["trace"] = "changed"
	if cloned.Headers["trace"] != "abc" {
		t.Fatalf("cloned headers=%v", cloned.Headers)
	}
}
