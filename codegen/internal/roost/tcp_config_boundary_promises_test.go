package roost

import (
	"testing"
	"time"
)

func TestExplicitZeroTCPShutdownBudgetIsRefused(t *testing.T) {
	var s shutdownSettings
	s.PlayerAccess.TCP.ShutdownTimeout = "0s"
	if got, err := configuredDeclaredBudgets(serviceShutdown{playerTCP: 1}, s); err == nil {
		t.Fatalf("explicit zero accepted with budget %s", got)
	}
	s.PlayerAccess.TCP.ShutdownTimeout = ""
	if got, err := configuredDeclaredBudgets(serviceShutdown{playerTCP: 1}, s); err != nil || got != 10*time.Second {
		t.Fatalf("missing default: %s %v", got, err)
	}
}
func TestGeneratedTCPBudgetKeepsInheritance(t *testing.T) {
	for _, raw := range []string{"nest:\n  request_timeout: 12s\n", "service: game\n"} {
		d, err := parseYAMLDocument([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range playerTCPDefaultsFor(d) {
			if (entry.key == "dispatch_timeout" || entry.key == "login_timeout") && entry.value != "0s" {
				t.Errorf("generated %s=%s freezes a runtime fallback", entry.key, entry.value)
			}
		}
	}
}
