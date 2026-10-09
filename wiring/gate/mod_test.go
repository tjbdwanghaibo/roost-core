package gate

import (
	"context"
	"net"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
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
