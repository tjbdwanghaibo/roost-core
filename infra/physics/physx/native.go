//go:build physx && cgo

package physx

/*
#cgo darwin LDFLAGS: -L${SRCDIR}/lib -lroost_physx -Wl,-rpath,${SRCDIR}/lib
#cgo linux LDFLAGS: -L${SRCDIR}/lib -lroost_physx -Wl,-rpath,${SRCDIR}/lib
#include "native/bridge.h"
*/
import "C"
import (
	"errors"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"sync"
	"unsafe"
)

type World struct {
	mu      sync.Mutex
	native  *C.rp_world
	ids     map[uint64]Shape
	dynamic int
}

func New(gravity float32, workers int) (*World, error) {
	if err := fctx.BlockingError("physx.New"); err != nil {
		return nil, err
	}
	if !finite(gravity) || workers < 1 || workers > 64 {
		return nil, errors.New("physx: invalid world config")
	}
	p := C.rp_create(C.float(gravity), C.uint(workers))
	if p == nil {
		return nil, errors.New("physx: create failed")
	}
	return &World{native: p, ids: make(map[uint64]Shape)}, nil
}
func (w *World) Add(b Body) error {
	if err := fctx.BlockingError("physx.Add"); err != nil {
		return err
	}
	if e := validateBody(b); e != nil {
		return e
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.native == nil {
		return ErrClosed
	}
	if len(w.ids) >= 16384 {
		return errors.New("physx: body capacity")
	}
	if _, ok := w.ids[b.ID]; ok {
		return errors.New("physx: duplicate body")
	}
	if b.Rotation == (Quat{}) {
		b.Rotation = Quat{0, 0, 0, 1}
	}
	if C.rp_add_pose(w.native, C.uint64_t(b.ID), C.uint(b.Shape), (*C.float)(unsafe.Pointer(&b.Position[0])), (*C.float)(unsafe.Pointer(&b.Rotation[0])), (*C.float)(unsafe.Pointer(&b.Dimensions[0])), C.float(b.Mass)) == 0 {
		return errors.New("physx: add failed")
	}
	w.ids[b.ID] = b.Shape
	if b.Shape != Box {
		w.dynamic++
	}
	return nil
}
func (w *World) Step(dt float32, commands []Command) ([]State, error) {
	if err := fctx.BlockingError("physx.Step"); err != nil {
		return nil, err
	}
	if fctx.InIOWorker() {
		return nil, errors.New("physx: simulation cannot run in io worker")
	}
	if !finite(dt) || dt <= 0 || dt > 0.1 || len(commands) > 16384 {
		return nil, errors.New("physx: invalid step")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.native == nil {
		return nil, ErrClosed
	}
	input := make([]C.rp_command, len(commands))
	seen := make(map[uint64]bool, len(commands))
	for i, c := range commands {
		shape, ok := w.ids[c.ID]
		if !ok || shape == Box || seen[c.ID] || !validVec(c.Acceleration) {
			return nil, errors.New("physx: invalid command")
		}
		seen[c.ID] = true
		input[i].id = C.uint64_t(c.ID)
		for j := range 3 {
			input[i].acceleration[j] = C.float(c.Acceleration[j])
		}
	}
	var p *C.rp_command
	if len(input) > 0 {
		p = &input[0]
	}
	if C.rp_step(w.native, C.float(dt), p, C.uint(len(input))) == 0 {
		return nil, errors.New("physx: simulation failed")
	}
	if w.dynamic == 0 {
		return nil, nil
	}
	raw := make([]C.rp_state, w.dynamic)
	if int(C.rp_read(w.native, &raw[0], C.uint(len(raw)))) != w.dynamic {
		return nil, errors.New("physx: incomplete snapshot")
	}
	states := make([]State, len(raw))
	for i, s := range raw {
		v := &states[i]
		v.ID = uint64(s.id)
		v.Sleeping = s.sleeping != 0
		for j := range 3 {
			v.Position[j] = float32(s.p[j])
			v.Velocity[j] = float32(s.v[j])
			v.AngularVelocity[j] = float32(s.w[j])
		}
		for j := range 4 {
			v.Rotation[j] = float32(s.q[j])
		}
	}
	return states, nil
}
func (w *World) Close() {
	fctx.AssertBlockingAllowed("physx.Close")
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.native != nil {
		C.rp_destroy(w.native)
		w.native = nil
	}
}
func Version() string { return C.GoString(C.rp_version()) }
