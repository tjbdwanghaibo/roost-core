package session

import domain "github.com/tjbdwanghaibo/roost-core/service/session"

type ModOption func(*Mod)

// WithSweepOwners connects the deployment's rotating bounded owner roster to
// the generated server's recovery hook. Without it recovery remains lazy.
func WithSweepOwners(source domain.OwnerSource) ModOption {
	return func(mod *Mod) { mod.owners = source }
}
