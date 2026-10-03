package errors

import "github.com/tjbdwanghaibo/roost-core/errcode"

// ErrItemUnknown: the item id is not in the item table.
//
// The code is what the client sees and must never change once shipped; the
// message is the client-facing reason. Codes come from the manifest's errcode
// space (100000–199999) and `roost id check` fails on a clash. Wrap it with
// errcode.Wrap(ErrItemUnknown, cause, "item_id", id) to attach operator
// context that stays out of the client response.
var ErrItemUnknown = errcode.Define(100001, "item_unknown", "unknown item")
