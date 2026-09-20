package roost

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// U-0089 (C2): every refusal in Add's per-kind parameter checks must fire on
// its own, and a refused Add must leave roost.yaml byte-identical.
func TestAddRefusesEachInvalidKindParameter(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{
		Name: "planet", Module: "example.com/planet", Out: target,
		Services: []string{"game", "gate"}, Mods: []string{"configdata"}, Features: []string{"protocol"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "access", Name: "player", Service: "game"}); err != nil {
		t.Fatalf("seed access player: %v", err)
	}
	if _, err := Add(root, AddOptions{Kind: "transport", Name: "tcp", Service: "game"}); err != nil {
		t.Fatalf("seed transport tcp: %v", err)
	}
	manifestPath := filepath.Join(root, ManifestName)
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		options AddOptions
		want    string
	}{
		{"invalid name", AddOptions{Kind: "service", Name: "9 bad!"}, `invalid service name "9 bad!"`},
		{"service exists", AddOptions{Kind: "service", Name: "game"}, "service game already exists"},
		{"mod unknown service", AddOptions{Kind: "mod", Name: "nest", Service: "lobby"}, `unknown service "lobby"`},
		{"mod unknown kit mod", AddOptions{Kind: "mod", Name: "teleport", Service: "game"}, `unknown kit mod "teleport"`},
		{"access unsupported", AddOptions{Kind: "access", Name: "admin", Service: "game"}, `unsupported access layer "admin"`},
		{"access ambiguous service", AddOptions{Kind: "access", Name: "player"}, "requires --service when the project has multiple services"},
		{"access unknown service", AddOptions{Kind: "access", Name: "player", Service: "lobby"}, `unknown service "lobby"`},
		{"access exists", AddOptions{Kind: "access", Name: "player", Service: "gate"}, "access layer player already exists"},
		{"transport unsupported", AddOptions{Kind: "transport", Name: "udp"}, `unsupported player transport "udp"`},
		{"transport wrong service", AddOptions{Kind: "transport", Name: "tcp", Service: "gate"}, `player access belongs to service "game", not "gate"`},
		{"transport exists", AddOptions{Kind: "transport", Name: "tcp", Service: "game"}, "player transport tcp already exists"},
		{"saga ambiguous service", AddOptions{Kind: "saga", Name: "trade", Steps: []string{"reserve", "commit"}}, "saga requires -service when the project has multiple services"},
		{"saga unknown service", AddOptions{Kind: "saga", Name: "trade", Service: "lobby", Steps: []string{"reserve", "commit"}}, `unknown service "lobby"`},
		{"protocol invalid handler", AddOptions{Kind: "protocol", Name: "PlayerLogin", Group: "game", Handler: "9 bad!"}, `invalid protocol handler "9 bad!"`},
		{"protocol invalid group", AddOptions{Kind: "protocol", Name: "PlayerLogin", Group: "9 bad!"}, `invalid protocol group`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paths, err := Add(root, tc.options)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Add(%+v) = %v, %v; want error containing %q", tc.options, paths, err, tc.want)
			}
			after, readErr := os.ReadFile(manifestPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("refused Add changed %s:\nbefore:\n%s\nafter:\n%s", ManifestName, before, after)
			}
		})
	}
}

// The handler kind is refused when the project never enabled the nest feature.
func TestAddHandlerRequiresNestFeature(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{
		Name: "planet", Module: "example.com/planet", Out: target,
		Mods: []string{"configdata"}, Features: []string{"protocol", "entity"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Add(root, AddOptions{Kind: "handler", Name: "Login", Entity: "Player", Protocol: "PlayerLogin"})
	if err == nil || !strings.Contains(err.Error(), "handler requires the nest feature") {
		t.Fatalf("handler without nest feature error = %v", err)
	}
}
