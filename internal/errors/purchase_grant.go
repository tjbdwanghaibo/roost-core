package errors

import "github.com/tjbdwanghaibo/roost-core/errcode"

// ErrPurchaseGrant: the grant names no order, or carries no payment moment to
// place it in time. Both mean the record cannot be made exactly-once, so it is
// refused rather than granted.
var ErrPurchaseGrant = errcode.Define(100014, "purchase_grant", "invalid purchase grant")
