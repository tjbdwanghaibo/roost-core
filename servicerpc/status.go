// The response status convention: a service answers business failures inside
// the response envelope with a stable errcode rather than as a bus error, so a
// caller can tell "the peer refused this" from "the call did not happen".
package servicerpc

import (
	"github.com/tjbdwanghaibo/roost-core/errcode"
)

const (
	CodeOK    int32 = errcode.CodeOK
	CodeError int32 = errcode.CodeInternal
)

func Error(err error) (int32, string) {
	return errcode.ClientError(err)
}

func Code(err error) int32 {
	code, _ := Error(err)
	return code
}

func Reason(err error) string {
	_, reason := Error(err)
	return reason
}

func Check(code int32, reason string, fallback string) error {
	return errcode.Remote(code, reason, fallback)
}
