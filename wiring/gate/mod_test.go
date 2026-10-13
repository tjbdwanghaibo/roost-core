package gate

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/service/account"
)

type roleVerifier struct {
	account.Accounts
	playerID int64
	token    string
	role     account.Role
}

func (verifier *roleVerifier) ValidateSession(_ context.Context, id int64, token string) (account.Role, error) {
	verifier.playerID = id
	verifier.token = token
	return verifier.role, nil
}
func TestAccountAuthenticatorUsesVerifiedFixedRoute(t *testing.T) {
	verifier := &roleVerifier{role: account.Role{PlayerID: 77, ServerID: 8}}
	auth := AccountAuthenticator{Accounts: verifier}
	principal, err := auth.Authenticate(context.Background(), "session:77:signed:token", nil)
	if err != nil || principal.PlayerID != 77 || principal.ServerID != 8 || len(principal.SessionID) != 32 || verifier.token != "signed:token" {
		t.Fatalf("principal=%+v err=%v", principal, err)
	}
	for _, ticket := range []string{"account:77:token", "session:0:token", "session:77:", "77"} {
		if _, err := auth.Authenticate(context.Background(), ticket, nil); err == nil {
			t.Fatalf("accepted %q", ticket)
		}
	}
	verifier.role.PlayerID = 88
	if _, err := auth.Authenticate(context.Background(), "session:77:token", nil); err == nil {
		t.Fatal("accepted mismatching role")
	}
}
func TestGateModsRefuseIncompleteProductionAssembly(t *testing.T) {
	options := DefaultOptions()
	options.TCP.Enabled = true
	options.TCP.MaxPayloadBytes = uint32(options.Config.MaxPayloadBytes)
	mod := NewGateMod(options)
	if err := mod.Init(viper.New()); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(app.NewRegistry(viper.New())); err == nil {
		t.Fatal("started without singleton and real NATS")
	}
	options.TCP.MaxPayloadBytes++
	if err := NewGateMod(options).Init(viper.New()); err == nil {
		t.Fatal("payload mismatch silently shrank TCP")
	}
	options.TCP.Enabled = false
	if err := NewGateMod(options).Init(viper.New()); err == nil {
		t.Fatal("disabled standalone Gate")
	}
	if err := NewGameIngressMod(nil, nil, options).Init(viper.New()); err == nil {
		t.Fatal("ingress without dispatcher")
	}
	options.Authenticator = gateway.AuthenticatorFunc(func(context.Context, string, net.Addr) (gateway.Principal, error) { return gateway.Principal{}, nil })
	deps := NewGameIngressMod(func(context.Context, gateway.Session, uint32, uint32, wire.PayloadKind, []byte) (any, error) {
		return nil, nil
	}, nil, options).DependsOn()
	for _, name := range deps {
		if name == account.CapabilityName {
			t.Fatal("custom auth unexpectedly needs Accounts")
		}
	}
}

type discoveryRecorder struct {
	etcd.IDiscovery
	infos                      []*etcd.ServiceInfo
	registered                 *etcd.ServiceInfo
	registerErr, deregisterErr error
	removes                    int
}

func (d *discoveryRecorder) Discover(context.Context, string) ([]*etcd.ServiceInfo, error) {
	return d.infos, nil
}
func (d *discoveryRecorder) Register(_ context.Context, info *etcd.ServiceInfo) error {
	if d.registerErr != nil {
		return d.registerErr
	}
	d.registered = info
	return nil
}
func (d *discoveryRecorder) Deregister(context.Context) error { d.removes++; return d.deregisterErr }

type flushRecorder struct {
	fnats.RawClient
	flushed bool
}

func (c *flushRecorder) FlushContext(context.Context) error { c.flushed = true; return nil }
func TestDiscoveryFixedRouteAndRegistrationOwnership(t *testing.T) {
	id := gateway.ProcessIdentity{ServerID: 22, Incarnation: "game000000000001"}
	d := &discoveryRecorder{infos: []*etcd.ServiceInfo{{Sid: 23, Metadata: map[string]string{IngressReadyMetadata: "true", IncarnationMetadata: id.Incarnation}}, {Sid: 22, Metadata: map[string]string{IngressReadyMetadata: "false", IncarnationMetadata: id.Incarnation}}}}
	resolve := FixedGameResolver(d, "game")
	if _, err := resolve(context.Background(), 22); !errors.Is(err, gateway.ErrTransportUnavailable) {
		t.Fatalf("wrong SID fallback=%v", err)
	}
	d.infos = append(d.infos, &etcd.ServiceInfo{Sid: 22, Metadata: map[string]string{IngressReadyMetadata: "true", IncarnationMetadata: id.Incarnation}})
	if actual, err := resolve(context.Background(), 22); err != nil || actual != id {
		t.Fatalf("identity=%+v err=%v", actual, err)
	}
	d.infos = append(d.infos, &etcd.ServiceInfo{Sid: 22, Metadata: map[string]string{IngressReadyMetadata: "true", IncarnationMetadata: "game000000000002"}})
	if _, err := resolve(context.Background(), 22); err == nil {
		t.Fatal("guessed among conflicting incarnations")
	}
	raw := &flushRecorder{}
	deps := connections{identity: id, client: raw, discovery: d}
	if err := deps.register("game", "", time.Second); err != nil {
		t.Fatal(err)
	}
	if !raw.flushed || d.registered.Sid != 22 || d.registered.Metadata[IncarnationMetadata] != id.Incarnation || !deps.registered {
		t.Fatal("registered before role inbox ready or identity missing")
	}
	d.deregisterErr = context.DeadlineExceeded
	if err := deps.deregister(context.Background()); !errors.Is(err, context.DeadlineExceeded) || !deps.registered {
		t.Fatalf("failed deregister released ownership: %v", err)
	}
	d.deregisterErr = nil
	if err := deps.deregister(context.Background()); err != nil || deps.registered {
		t.Fatalf("retry deregister=%v", err)
	}
	if err := deps.deregister(context.Background()); err != nil || d.removes != 2 {
		t.Fatalf("duplicate removal=%d err=%v", d.removes, err)
	}
	d.registerErr = errors.New("already registered")
	if err := deps.register("game", "", time.Second); err == nil || deps.registered {
		t.Fatal("took ownership of another registration")
	}
}
