package player

import "github.com/tjbdwanghaibo/cube/event"

type Player struct {
	name string
}

func (p *Player) SubEvent(typo any) {}

func (p *Player) DealEventPlayerOnLine(d *event.EventPlayerOnLine) {
	// handle player online
}

func (p *Player) DealEventPlayerOffLine(d *event.EventPlayerOffLine) {
	// handle player offline
}
