package redis

import (
	fredis "github.com/tjbdwanghaibo/cube-core/redis"

	goredis "github.com/redis/go-redis/v9"
)

type pubSub struct {
	ps *goredis.PubSub
	ch <-chan *fredis.PubSubMessage
}

func newPubSub(ps *goredis.PubSub) *pubSub {
	goCh := ps.Channel()
	// Convert go-redis channel to framework channel
	msgCh := make(chan *fredis.PubSubMessage, cap(goCh))
	go func() {
		for msg := range goCh {
			msgCh <- &fredis.PubSubMessage{
				Channel: msg.Channel,
				Payload: msg.Payload,
			}
		}
		close(msgCh)
	}()
	return &pubSub{ps: ps, ch: msgCh}
}

func (p *pubSub) Channel() <-chan *fredis.PubSubMessage {
	return p.ch
}

func (p *pubSub) Close() error {
	return p.ps.Close()
}

var _ fredis.IPubSub = (*pubSub)(nil)
