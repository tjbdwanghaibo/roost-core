package skill

type motionIR interface {
	isMotionIR()
	walkValues(valueVisitor)
}

type processIR struct {
	source          sourceRef
	kind            string
	durationTicks   Tick
	intervalTicks   Tick
	emitLeaveOnStop bool
	visual          *visualIR
	area            *selectIR
	motion          motionIR
	numericTracks   []numericTrackIR
}

func (p *processIR) walkValues(visitor valueVisitor) {
	if p.area != nil {
		p.area.walkValues(visitor)
	}
	if p.motion != nil {
		p.motion.walkValues(visitor)
	}
	for _, track := range p.numericTracks {
		track.walkValues(visitor)
	}
}

type canonicalMotionIR struct {
	source     sourceRef
	frame      motionFrameIR
	steering   motionSteeringIR
	trajectory motionTrajectoryIR
	offsets    []motionOffsetIR
	collision  *motionCollisionIR
	carry      *motionCarryIR
	completion motionCompletionIR
}

func (*canonicalMotionIR) isMotionIR() {}
func (m *canonicalMotionIR) walkValues(visitor valueVisitor) {
	m.frame.walkValues(visitor)
	if m.steering != nil {
		m.steering.walkValues(visitor)
	}
	m.trajectory.walkValues(visitor)
	for _, offset := range m.offsets {
		offset.walkValues(visitor)
	}
	if m.carry != nil {
		m.carry.walkValues(visitor)
	}
}

type motionFrameIR interface{ walkValues(valueVisitor) }
type worldFrameIR struct{}

func (worldFrameIR) walkValues(valueVisitor) {}

type followFrameIR struct{ target valueIR }

func (f followFrameIR) walkValues(visitor valueVisitor) { walkValue(f.target, visitor) }

type motionSteeringIR interface{ walkValues(valueVisitor) }
type trackingSteeringIR struct {
	target        valueIR
	durationTicks Tick
}

func (s trackingSteeringIR) walkValues(visitor valueVisitor) { walkValue(s.target, visitor) }

type motionTrajectoryIR interface {
	name() string
	walkValues(valueVisitor)
}
type stationaryTrajectoryIR struct{}

func (stationaryTrajectoryIR) name() string            { return "stationary" }
func (stationaryTrajectoryIR) walkValues(valueVisitor) {}

type linearTrajectoryIR struct{ speed valueIR }

func (linearTrajectoryIR) name() string                      { return "linear" }
func (t linearTrajectoryIR) walkValues(visitor valueVisitor) { walkValue(t.speed, visitor) }

type pathTrajectoryIR struct{ points, speed valueIR }

func (pathTrajectoryIR) name() string { return "path" }
func (t pathTrajectoryIR) walkValues(visitor valueVisitor) {
	walkValue(t.points, visitor)
	walkValue(t.speed, visitor)
}

type orbitTrajectoryIR struct{ anchor, radius, angularSpeed valueIR }

func (orbitTrajectoryIR) name() string { return "orbit" }
func (t orbitTrajectoryIR) walkValues(visitor valueVisitor) {
	walkValue(t.anchor, visitor)
	walkValue(t.radius, visitor)
	walkValue(t.angularSpeed, visitor)
}

type parabolaTrajectoryIR struct {
	destination, height valueIR
	durationTicks       Tick
}

func (parabolaTrajectoryIR) name() string { return "parabola" }
func (t parabolaTrajectoryIR) walkValues(visitor valueVisitor) {
	walkValue(t.destination, visitor)
	walkValue(t.height, visitor)
}

type motionOffsetIR interface{ walkValues(valueVisitor) }
type zigzagOffsetIR struct {
	amplitude   valueIR
	periodTicks Tick
}

func (o zigzagOffsetIR) walkValues(visitor valueVisitor) { walkValue(o.amplitude, visitor) }

type circularOffsetIR struct{ radius, angularSpeed valueIR }

func (o circularOffsetIR) walkValues(visitor valueVisitor) {
	walkValue(o.radius, visitor)
	walkValue(o.angularSpeed, visitor)
}

type motionCollisionIR struct {
	layers                  []string
	response                string
	maxReflects, maxPierces int
}
type motionCarryIR struct{ target valueIR }

func (c motionCarryIR) walkValues(visitor valueVisitor) { walkValue(c.target, visitor) }

type motionCompletionIR interface{ name() string }
type endCompletionIR struct{}

func (endCompletionIR) name() string { return "end" }

type pauseThenEndCompletionIR struct{ pauseTicks Tick }

func (pauseThenEndCompletionIR) name() string { return "pause_then_end" }

type boomerangCompletionIR struct{ maxReturnTicks Tick }

func (boomerangCompletionIR) name() string { return "boomerang" }
