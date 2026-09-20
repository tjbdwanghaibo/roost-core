package roost

import (
	"os"
	"path/filepath"
)

// optionalHint is one "you could also" step project next prints once the
// current workflow is complete: a capability the framework has and the
// project has not used yet, with the command that starts it and the reason it
// exists. They are suggestions, not the next required action — nothing in
// doctor turns red for skipping them.
type optionalHint struct {
	Command string
	Why     string
}

// optionalNextHints lists the framework capabilities this project has not
// picked up, in the order they usually pay off.
func optionalNextHints(root string, m Manifest) []optionalHint {
	game := ""
	for _, name := range sortedServiceNames(m) {
		if !m.isFrameworkService(name) {
			game = name
			break
		}
	}
	if game == "" {
		return nil
	}
	var hints []optionalHint
	ownsRPC := false
	for _, name := range sortedServiceNames(m) {
		ownsRPC = ownsRPC || len(m.Services[name].Rpcs) > 0
	}
	if !ownsRPC {
		hints = append(hints, optionalHint{
			Command: "roost add rpc <Name> -service " + game,
			Why:     "a cross-process service this project owns: callers look up the interface, so moving it into its own process later changes no business code",
		})
	}
	if !contains(m.Features, "saga") {
		hints = append(hints, optionalHint{
			Command: "roost add saga <name> -service " + game + " -steps <step1,step2>",
			Why:     "a multi-step operation across services that either completes or compensates; exactly-once steps on the Data Engine",
		})
	}
	if !contains(m.Features, "attribute") {
		hints = append(hints, optionalHint{
			Command: "add attribute to roost.yaml features, then make sync",
			Why:     "attribute profiles: ids, masks, setters, derived formulas and snapshots generated from one declaration (game/gameplay/attribute/)",
		})
	}
	if _, err := os.Stat(filepath.Join(root, "game", "skills")); os.IsNotExist(err) {
		hints = append(hints, optionalHint{
			Command: "roost add skill <Name>",
			Why:     "a skill as a stable roost-core/skill JSON contract, compiled at startup and catalogued (game/skills/)",
		})
	}
	if !contains(m.Features, "webroute") {
		hints = append(hints, optionalHint{
			Command: "add webroute to roost.yaml features, then make sync and roost add webroute <Name>",
			Why:     "typed HTTP routes for GM and operations tooling (service/web/), registered by generated code",
		})
	}
	if _, err := os.Stat(filepath.Join(root, "configs", "schema", "cfg.yaml")); os.IsNotExist(err) {
		hints = append(hints, optionalHint{
			Command: "write configs/schema/cfg.yaml, then go run github.com/tjbdwanghaibo/roost-codegen/cmd/cfggen -meta configs/schema/cfg.yaml -out configs/cfg (see roost help cfggen)",
			Why:     "tables, objects and beans declared in one YAML with keys, indexes and references, bound to typed accessors — the meta-first alternative to //roost:table",
		})
	}
	return hints
}
