package fctx

import (
	"errors"
	"fmt"
)

var ErrBlockingInFastWorker = errors.New("fctx: blocking operation in fast worker")

// WithFastWorker 标记当前执行资源，不是可传递的请求属性。嵌套 Context 继承，
// ContextSnapshot 不携带它，防止快慢交接时把调用者的执行位置带到接收方。
func WithFastWorker() Option { return func(c *Context) { c.fastWorker = true } }

func InFastWorker() bool {
	c := CurrentContext()
	return c != nil && c.fastWorker
}

// BlockingError 在等待发生前提供入口与业务名；调用方可附加自己的错误身份再 panic。
func BlockingError(operation string) error {
	c := CurrentContext()
	if c == nil || !c.fastWorker {
		return nil
	}
	return fmt.Errorf("%w: %s (handler=%s)", ErrBlockingInFastWorker, operation, c.Meta.Handler)
}

func AssertBlockingAllowed(operation string) {
	if err := BlockingError(operation); err != nil {
		panic(err)
	}
}
