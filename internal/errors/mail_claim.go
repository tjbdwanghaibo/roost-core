package errors

import "github.com/tjbdwanghaibo/roost-core/errcode"

// ErrMailClaim: the request names no mail, or names one this call cannot grant.
var ErrMailClaim = errcode.Define(100011, "mail_claim", "invalid mail claim")
