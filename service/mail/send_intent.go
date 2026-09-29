package mail

import "reflect"

// compare the complete persisted intent, including id and expiry. The
// recovery path must not deliver an envelope substituted under a reserved id.
func sameSendIntent(a, b Envelope) bool { return reflect.DeepEqual(a, b) }
