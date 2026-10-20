package alliance

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
)

// 用正式 Receiver 消费 TCP 收到的编码帧，重建客户端对象表。
type fogClient struct {
	receiver *frame.Receiver
	objects  map[frame.ObjectRef]int64
	fulls    int
	last     []byte
}

func newFogClient() *fogClient {
	return &fogClient{receiver: frame.NewReceiver(frame.DefaultLimits()), objects: map[frame.ObjectRef]int64{}}
}
func (c *fogClient) consume(t *testing.T, frames [][]byte) {
	t.Helper()
	for _, raw := range frames {
		applied, e := c.receiver.Receive(raw, func(f frame.Frame) error {
			if f.Kind == frame.Full {
				clear(c.objects)
				c.fulls++
			}
			for _, obj := range f.Objects {
				if obj.Operation == frame.ObjectRemove {
					delete(c.objects, obj.Ref)
					continue
				}
				for _, component := range obj.Components {
					u, e := entitysync.DecodeSubjectUpdate(component.Data, 0)
					if e != nil {
						return e
					}
					if u.SubjectID != 99 || string(u.Payload.BytesCopy()) != `{"objective":99}` {
						return errors.New("sensitive or unexpected view payload")
					}
					c.objects[obj.Ref] = u.SubjectID
				}
			}
			return nil
		})
		check(t, e)
		if !applied {
			t.Fatal("fresh frame rejected")
		}
		c.last = append([]byte(nil), raw...)
	}
}
func verifyFogRecovery(t *testing.T, f *fog, load func() (state, error)) {
	t.Helper()
	alice, bob := newFogClient(), newFogClient()
	alice.consume(t, f.sink.take(1))
	bob.consume(t, f.sink.take(2))
	if len(alice.objects) != 1 || len(bob.objects) != 0 || alice.fulls == 0 {
		t.Fatal("encoded views disagree with revoked membership")
	}
	old := append([]byte(nil), alice.last...)
	// 远端权限读取失败：先停发布，再尝试读；不能拿旧权限继续 ReadySession。
	injected := errors.New("authority read unavailable")
	if e := f.reconcileFrom(func() (state, error) { return state{}, injected }); !errors.Is(e, injected) {
		t.Fatal("permission failure ignored", e)
	}
	check(t, f.manager.Flush(context.Background()))
	if len(f.sink.take(1)) != 0 || len(f.sink.take(2)) != 0 {
		t.Fatal("held view published after failed authorization refresh")
	}
	check(t, f.reconcileFrom(load))
	alice.consume(t, f.sink.take(1))
	bob.consume(t, f.sink.take(2))
	if len(alice.objects) != 1 || len(bob.objects) != 0 {
		t.Fatal("refresh restored stale membership")
	}
	// 旧缓存和同代伪造都不能恢复退盟角色；失败后依旧必须读权威才能重新发布。
	latest, e := load()
	check(t, e)
	for _, stale := range []bool{true, false} {
		bad := clone(latest)
		if stale {
			bad.PermissionGeneration--
		}
		m := bad.Members["bob"]
		m.Active = true
		bad.Members["bob"] = m
		if e := f.reconcile(bad); !errors.Is(e, errRefused) {
			t.Fatal("stale or conflicting authority accepted", e)
		}
		check(t, f.manager.Flush(context.Background()))
		if len(f.sink.take(1))+len(f.sink.take(2)) != 0 {
			t.Fatal("rejected authority published frames")
		}
		check(t, f.reconcileFrom(load))
		alice.consume(t, f.sink.take(1))
		bob.consume(t, f.sink.take(2))
		if len(bob.objects) != 0 {
			t.Fatal("revoked membership resurrected")
		}
	}
	fulls := alice.fulls
	for _, sid := range []entitysync.SessionID{1, 2} {
		f.manager.CloseSession(sid)
		f.sink.connect(sid)
		check(t, f.manager.OpenHeldSession(sid))
		f.interest.Resubscribe(int64(sid))
	}
	check(t, f.reconcileFrom(load))
	alice.consume(t, f.sink.take(1))
	bob.consume(t, f.sink.take(2))
	if alice.fulls <= fulls || len(alice.objects) != 1 || len(bob.objects) != 0 {
		t.Fatal("reconnect failed to rebuild latest authorized Full")
	}
	applied, e := alice.receiver.Receive(old, func(frame.Frame) error { return errors.New("old epoch reached application") })
	check(t, e)
	if applied {
		t.Fatal("old epoch applied after reconnect")
	}
	t.Log("encoded view: alliance/scout removal, failed authority refresh holds publication, reconnect rebuilds Full, revoked member absent, old epoch refused")
}
