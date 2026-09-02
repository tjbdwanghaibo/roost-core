package dataengine

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
)

// LeaseFenceReceiptNamespace is reserved for transaction preconditions. The
// record is carried in the existing WAL receipt section but is never exposed
// as a business receipt or staged in the receipt collection.
const LeaseFenceReceiptNamespace = "__dataengine_lease_fence_v1"

var ErrInvalidLeaseFence = errors.New("dataengine: invalid lease fence")

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
