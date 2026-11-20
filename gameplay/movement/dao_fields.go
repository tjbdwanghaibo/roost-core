package movement

import "github.com/tjbdwanghaibo/roost-core/infra/base/spatial"

type sceneFacingStore interface {
	GetSceneID() int64
	SetSceneID(int64)
	GetFacing() int32
	SetFacing(int32)
}

// PosXYStore stores movement in SceneID, PosX, PosY and Facing.
// The generated DAO setters own dirty tracking and transaction rollback.
type PosXYStore interface {
	sceneFacingStore
	GetPosX() int64
	SetPosX(int64)
	GetPosY() int64
	SetPosY(int64)
}

// XYStore is the same contract for storage whose coordinates are named X and Y.
type XYStore interface {
	sceneFacingStore
	GetX() int64
	SetX(int64)
	GetY() int64
	SetY(int64)
}

// BindPosXY adapts an explicitly selected store to Fields, without caching a DAO
// or its values. Each operation resolves current once under the caller's lock.
// afterWrite runs synchronously after each logical setter (once after X then Y
// for SetPos). Pass nil when the caller leaves publication to the commit observer.
func BindPosXY[D PosXYStore](current func() D, afterWrite func()) Fields {
	return &daoFields[D]{current: current, afterWrite: afterWrite, x: D.GetPosX, y: D.GetPosY, setX: D.SetPosX, setY: D.SetPosY}
}

// BindXY is BindPosXY for an explicitly selected XYStore, e.g. a nested Actor
// state. Supply the same publication choice as for BindPosXY.
func BindXY[D XYStore](current func() D, afterWrite func()) Fields {
	return &daoFields[D]{current: current, afterWrite: afterWrite, x: D.GetX, y: D.GetY, setX: D.SetX, setY: D.SetY}
}

type daoFields[D sceneFacingStore] struct {
	current    func() D
	afterWrite func()
	x, y       func(D) int64
	setX, setY func(D, int64)
}

func (f *daoFields[D]) SceneID() int64 { return f.current().GetSceneID() }
func (f *daoFields[D]) SetSceneID(v int64) {
	f.current().SetSceneID(v)
	f.wrote()
}
func (f *daoFields[D]) Pos() spatial.Point {
	d := f.current()
	return spatial.Point{X: f.x(d), Y: f.y(d)}
}
func (f *daoFields[D]) SetPos(v spatial.Point) {
	d := f.current()
	f.setX(d, v.X)
	f.setY(d, v.Y)
	f.wrote()
}
func (f *daoFields[D]) Facing() Facing { return Facing(f.current().GetFacing()) }
func (f *daoFields[D]) SetFacing(v Facing) {
	f.current().SetFacing(int32(v))
	f.wrote()
}
func (f *daoFields[D]) wrote() {
	if f.afterWrite != nil {
		f.afterWrite()
	}
}
