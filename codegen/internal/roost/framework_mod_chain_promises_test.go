package roost

import (
	"strings"
	"testing"
)

// A hosted framework service sometimes has an OPTIONAL collaborator that is
// not a NewMod parameter — platform's pending-order index is the first
// (kit `Mod.WithPendingOrders`). The generated wiring has to call it, or the
// project's collaborators file can define it and nothing will ever read it.
//
// The second promise here is the configuration block: platform's Mod refuses
// at Init when `session_secret` or `payment_secret` is empty, so a generated
// starter config that omits them produces a process that cannot start. The
// account block already emits `session_secret: CHANGE_ME` for exactly this
// reason.

func TestHostedServiceChainsItsOptionalCollaborators(t *testing.T) {
	generated := renderBootstrap(gameTemplateManifest(t))
	const want = "svcplatform.NewMod(servicePlatform.Verify(), servicePlatform.Players(), servicePlatform.Deliver(), servicePlatform.Metrics()).WithPendingOrders(servicePlatform.Pending())"
	if !strings.Contains(generated, want) {
		t.Errorf("the platform Mod is wired without its pending-order index:\n%s", generated)
	}
}

func TestPlatformConfigCarriesTheSecretsItsModRequires(t *testing.T) {
	block := frameworkCatalog["platform"].ConfigFunc("demo")
	for _, key := range []string{"session_secret:", "payment_secret:"} {
		if !strings.Contains(block, key) {
			t.Errorf("platform config block omits %s, so the process refuses to start:\n%s", key, block)
		}
	}
}

// The default collaborators file has to define everything the wiring names,
// including the optional ones, or the project does not compile.
func TestDefaultCollaboratorsDefineEveryWiredName(t *testing.T) {
	body := renderFrameworkCollaborators(gameTemplateManifest(t), "platform")
	for _, fn := range []string{"func Verify()", "func Players()", "func Deliver()", "func Pending()", "func Metrics()"} {
		if !strings.Contains(body, fn) {
			t.Errorf("collaborators file is missing %s:\n%s", fn, body)
		}
	}
}

// A kit service whose Go package is not at the top of service/ — activity
// lives at service/global/activity — must still be hostable. The catalog's
// Package is both an identifier (svcactivity) and an import suffix, and those
// are not the same string here; a catalog that conflates them generates
// `svcglobal/activity`, which is not an identifier.
func TestAServiceNestedUnderAnotherIsHostable(t *testing.T) {
	m := gameTemplateManifest(t)
	for _, name := range []string{"global", "activity"} {
		if m.Services[name].Framework != name {
			t.Fatalf("the game template does not host %s: %+v", name, m.Services[name])
		}
	}
	generated := renderBootstrap(m)
	for _, want := range []string{
		`svcactivity "github.com/tjbdwanghaibo/roost-kit/service/global/activity"`,
		"svcactivity.NewMod(serviceActivity.Metrics())",
		"svcglobal.NewMod(serviceGlobal.Metrics())",
	} {
		if !strings.Contains(generated, want) {
			t.Errorf("bootstrap is missing %q:\n%s", want, generated)
		}
	}
}

// The activity Mod refuses an unset reservation_ttl at Init (it must exceed
// the caller's retry horizon, which the service cannot pick), so a starter
// config that omits it is a process that cannot start.
func TestActivityConfigCarriesTheTTLItsModRequires(t *testing.T) {
	block := frameworkCatalog["activity"].ConfigFunc("demo")
	if !strings.Contains(block, "reservation_ttl:") {
		t.Errorf("activity config block omits reservation_ttl:\n%s", block)
	}
}
