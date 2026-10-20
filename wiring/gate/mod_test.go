package gate

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
	"github.com/tjbdwanghaibo/roost-core/service/account"
	"github.com/tjbdwanghaibo/roost-core/service/directory"
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
	verifier := &roleVerifier{role: account.Role{PlayerID: 77, ServerID: 8, ServingServerID: 8}}
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

// 合服后（A 并入 B）A 的角色经正式 account 服务认证：Principal 的路由服是承载服 B，按它选到 B 的 game，
// 原服 A 记在 OriginServerIDClaim；ServerIDClaim 与 Principal.ServerID 一致。未合服的 B 角色不受影响。
func TestAccountAuthenticatorRoutesAMergedServersRoleToItsServingServer(t *testing.T) {
	const serverA, serverB = int32(1001), int32(1000)
	ctx := context.Background()
	names, err := directory.New(versionstore.NewMemoryStore[string, directory.Entry](), directory.Config{Normalize: directory.NormalizeLower, DefaultTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	var nextID atomic.Int64
	service, err := account.New(account.Config{
		Accounts: versionstore.NewMemoryStore[string, account.Account](), Roles: versionstore.NewMemoryStore[int64, account.Role](),
		Servers: versionstore.NewMemoryStore[int32, account.GameServer](), Slots: versionstore.NewMemoryStore[string, account.Slot](), Names: names,
		Verifier: account.VerifierFunc(func(_ context.Context, identity account.Identity) (account.Verified, error) {
			return account.Verified{Channel: identity.Channel, OpenID: identity.OpenID}, nil
		}),
		Allocator:     account.AllocatorFunc(func(context.Context, int32) (int64, error) { return 5000 + nextID.Add(1), nil }),
		NameRules:     account.NameValidatorFunc(func(string) error { return nil }),
		SessionSecret: "gate-merge-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	ticketOn := func(openID string, sid int32) string {
		t.Helper()
		if _, err := service.UpsertServer(ctx, account.GameServer{ID: sid, Status: account.ServerOpen}); err != nil {
			t.Fatal(err)
		}
		acct, err := service.Login(ctx, account.Identity{Channel: "dev", OpenID: openID})
		if err != nil {
			t.Fatal(err)
		}
		role, err := service.CreateRole(ctx, acct.ID, sid, openID)
		if err != nil {
			t.Fatal(err)
		}
		session, err := service.SelectRole(ctx, acct.ID, role.PlayerID)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("session:%d:%s", role.PlayerID, session.Token)
	}
	ticketA, ticketB := ticketOn("on-a", serverA), ticketOn("on-b", serverB)
	if _, err := service.MergeServer(ctx, account.MergeRequest{From: serverA, Into: serverB, Note: "gate test"}); err != nil {
		t.Fatal(err)
	}

	gameB := gateway.ProcessIdentity{ServerID: serverB, Incarnation: "game000000001000"}
	resolve := FixedGameResolver(&discoveryRecorder{infos: []*etcd.ServiceInfo{
		{Sid: serverB, Metadata: map[string]string{IngressReadyMetadata: "true", IncarnationMetadata: gameB.Incarnation}},
	}}, "game")
	auth := AccountAuthenticator{Accounts: service}
	for _, check := range []struct {
		label  string
		ticket string
		origin int32
	}{{"role of the merged server A", ticketA, serverA}, {"role of B", ticketB, serverB}} {
		principal, err := auth.Authenticate(ctx, check.ticket, nil)
		if err != nil {
			t.Fatalf("%s: %v", check.label, err)
		}
		if principal.ServerID != serverB {
			t.Fatalf("%s: Principal.ServerID = %d, want the serving server %d", check.label, principal.ServerID, serverB)
		}
		if got := principal.Claims[account.ServerIDClaim]; got != strconv.Itoa(int(serverB)) {
			t.Fatalf("%s: ServerIDClaim = %q, want %d", check.label, got, serverB)
		}
		if got := principal.Claims[account.OriginServerIDClaim]; got != strconv.Itoa(int(check.origin)) {
			t.Fatalf("%s: OriginServerIDClaim = %q, want %d", check.label, got, check.origin)
		}
		game, err := resolve(ctx, principal.ServerID)
		if err != nil || game != gameB {
			t.Fatalf("%s: ResolveGame(%d) = %+v, %v; want game B", check.label, principal.ServerID, game, err)
		}
	}
}

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
