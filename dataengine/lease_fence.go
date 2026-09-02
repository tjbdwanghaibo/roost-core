package dataengine

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// LeaseFenceReceiptNamespace is reserved for transaction preconditions. The
// record is carried in the existing WAL receipt section but is never exposed
// as a business receipt or staged in the receipt collection.
const LeaseFenceReceiptNamespace = "__dataengine_lease_fence_v1"

var ErrInvalidLeaseFence = errors.New("dataengine: invalid lease fence")

// Field names of the coordination document a LeaseFence points at, and the
// only status under which a fenced transaction may apply.
//
// These are shared on purpose. The projector queries the document and the
// lease owner writes it, and the two live in different packages; when each
// side spelled the schema out for itself, a rename on either side turned
// every fenced transaction into a silent no-op — a fence that can never match
// is indistinguishable from a legitimately stale lease, and both are
// acknowledged as a skipped transaction with no error and no metric.
const (
	LeaseFenceFieldOwner      = "owner"
	LeaseFenceFieldToken      = "lease_token"
	LeaseFenceFieldDigest     = "digest"
	LeaseFenceFieldStatus     = "status"
	LeaseFenceFieldLeaseUntil = "lease_until"

	LeaseFenceStatusPending = "pending"
)

// LeaseFence requires one Mongo coordination document to still belong to the
// same owner/token when the WAL record is projected. A stale fence makes the
// entire record an idempotent no-op; it is not a fatal projection conflict.
type LeaseFence struct {
	Database   string `json:"database"`
	Resource   string `json:"resource"`
	DocumentID string `json:"document_id"`
	Owner      string `json:"owner"`
	Token      uint64 `json:"token"`
	Digest     []byte `json:"digest"`
}

func (fence LeaseFence) Validate() error {
	if strings.TrimSpace(fence.Database) == "" || strings.TrimSpace(fence.Resource) == "" || strings.TrimSpace(fence.DocumentID) == "" || strings.TrimSpace(fence.Owner) == "" || fence.Token == 0 || len(fence.Digest) != sha256.Size {
		return ErrInvalidLeaseFence
	}
	return nil
}

// Predicate is the document that must still exist for this fence to hold:
// same owner, same lease token, same command digest, still pending, and a
// lease that has not expired at now. A projector must use exactly this rather
// than assembling its own filter — see the constants above for why.
func (fence LeaseFence) Predicate(now time.Time) bson.M {
	return bson.M{
		"_id":                     fence.DocumentID,
		LeaseFenceFieldOwner:      fence.Owner,
		LeaseFenceFieldToken:      fence.Token,
		LeaseFenceFieldDigest:     fence.Digest,
		LeaseFenceFieldStatus:     LeaseFenceStatusPending,
		LeaseFenceFieldLeaseUntil: bson.M{"$gt": now},
	}
}

func NewLeaseFenceReceipt(fence LeaseFence) (Receipt, error) {
	if err := fence.Validate(); err != nil {
		return Receipt{}, err
	}
	payload, err := json.Marshal(fence)
	if err != nil {
		return Receipt{}, err
	}
	digest := sha256.Sum256(payload)
	return Receipt{Namespace: LeaseFenceReceiptNamespace, ID: fence.Database + "/" + fence.Resource + "/" + fence.DocumentID, Digest: digest[:], Payload: payload}, nil
}

func DecodeLeaseFenceReceipt(receipt Receipt) (LeaseFence, bool, error) {
	if receipt.Namespace != LeaseFenceReceiptNamespace {
		return LeaseFence{}, false, nil
	}
	var fence LeaseFence
	if receipt.ID == "" || len(receipt.Payload) == 0 || json.Unmarshal(receipt.Payload, &fence) != nil {
		return LeaseFence{}, true, ErrInvalidLeaseFence
	}
	if err := fence.Validate(); err != nil {
		return LeaseFence{}, true, err
	}
	expectedID := fence.Database + "/" + fence.Resource + "/" + fence.DocumentID
	expectedDigest := sha256.Sum256(receipt.Payload)
	if receipt.ID != expectedID || !bytes.Equal(receipt.Digest, expectedDigest[:]) {
		return LeaseFence{}, true, ErrInvalidLeaseFence
	}
	return fence, true, nil
}
