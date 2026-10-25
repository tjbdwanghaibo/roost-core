//go:build !physx || !cgo

package physx

type World struct{}

func New(gravity float32, workers int) (*World, error)  { return nil, ErrUnavailable }
func (*World) Add(Body) error                           { return ErrUnavailable }
func (*World) Step(float32, []Command) ([]State, error) { return nil, ErrUnavailable }
func (*World) Close()                                   {}
func Version() string                                   { return "unavailable" }
