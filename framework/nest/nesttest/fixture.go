// Package nesttest runs generated fixture inspections through the normal Nest
// client. It does not import internal lock capabilities or bypass admission.
package nesttest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

var name = nest.NewHandlerName("roost_test_inspect_exact_instances")
var register sync.Mutex

type visit struct {
	expected []entity.IThreadSafeEntity
	fn       func()
}

// With is a synchronous fixture operation, not a lock primitive. Use outside a
// handler. Normal grouping, Remote ownership, worker routing and fences apply.
func With[T entity.IThreadSafeEntity](ctx context.Context, engine *nest.NestMgr, e T, fn func()) error {
	return WithMany(ctx, engine, []T{e}, fn)
}
func WithMany[T entity.IThreadSafeEntity](ctx context.Context, engine *nest.NestMgr, es []T, fn func()) error {
	if !testing.Testing() || engine == nil || len(es) == 0 || fn == nil {
		return errors.New("nesttest: expected test engine, targets and callback")
	}
	register.Lock()
	_, ok := nest.GetHandlerEntry(name)
	if !ok {
		err := nest.RegisterMemoryHandler(name, func(got []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
			if len(params) != 1 {
				return nil, errors.New("nesttest: expected one inspection")
			}
			v, ok := params[0].(visit)
			if !ok || len(got) != len(v.expected) {
				return nil, errors.New("nesttest: target list mismatch")
			}
			for i, e := range got {
				if e != v.expected[i] {
					return nil, errors.New("nesttest: target instance replaced")
				}
			}
			v.fn()
			return nil, nil
		})
		if err != nil {
			register.Unlock()
			return err
		}
	}
	register.Unlock()
	ids := make([]int64, len(es))
	v := visit{expected: make([]entity.IThreadSafeEntity, len(es)), fn: fn}
	for i, e := range es {
		v.expected[i] = e
		ids[i] = e.GUId()
	}
	var err error
	if len(ids) == 1 {
		_, err = engine.Request(ctx, name, ids[0], nest.NewParams(v))
	} else {
		_, err = engine.RequestMulti(ctx, name, ids, nest.NewParams(v))
	}
	return err
}
