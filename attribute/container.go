package attribute

import "sync"

// Selector names one layer of a subject's attributes. A game decides what its
// layers mean; the framework only keeps them apart. Two are named here
// because every game has them under some spelling: Base is what the subject
// is without anything applied, Final is what it is after everything is.
type Selector struct {
	Layer string
}

var (
	// Base is the subject's own attributes.
	Base = Selector{Layer: "base"}
	// Final is the resolved view a client is shown.
	Final = Selector{Layer: "final"}
)

// String makes a selector readable in errors and logs.
func (s Selector) String() string {
	if s.Layer == "" {
		return Base.Layer
	}
	return s.Layer
}

func (s Selector) key() string {
	if s.Layer == "" {
		return Base.Layer
	}
	return s.Layer
}

// Snapshot is one layer read at one moment. Profile is a copy: mutating it
// cannot reach the container it came from, which is what makes a snapshot
// safe to hand to a renderer, a formula or another goroutine. A snapshot of a
// layer that holds nothing has a nil Profile — the generated typed accessors
// answer (nil, false) for it rather than panicking.
type Snapshot struct {
	Selector Selector
	Profile  Profile
}

// Container holds a subject's layers. It is safe for concurrent use; a game
// whose attributes only move inside an entity lock pays a mutex it does not
// need, which is cheaper than the bug it prevents in the games that read
// attributes from a second goroutine.
type Container struct {
	mu     sync.RWMutex
	layers map[string]Profile
}

func NewContainer() *Container {
	return &Container{layers: make(map[string]Profile, 2)}
}

// Install puts a profile in a layer, replacing whatever was there. The
// container keeps the profile itself, not a copy: the caller goes on owning
// it and the container's job is to find it again.
func (c *Container) Install(selector Selector, profile Profile) {
	if c == nil || profile == nil {
		return
	}
	c.mu.Lock()
	if c.layers == nil {
		c.layers = make(map[string]Profile, 2)
	}
	c.layers[selector.key()] = profile
	c.mu.Unlock()
}

// Remove drops a layer and reports whether one was there.
func (c *Container) Remove(selector Selector) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.layers[selector.key()]; !ok {
		return false
	}
	delete(c.layers, selector.key())
	return true
}

// Live returns the profile a layer holds, without copying. It is for the
// owner of the subject — the code already holding whatever lock the game uses
// — and never for a reader that might outlive the call; those take Snapshot.
func (c *Container) Live(selector Selector) (Profile, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	profile, ok := c.layers[selector.key()]
	return profile, ok
}

// Snapshot copies a layer out. The copy is taken under the container's lock,
// so a snapshot never catches a profile mid-write from this container's own
// mutators.
func (c *Container) Snapshot(selector Selector) Snapshot {
	if c == nil {
		return Snapshot{Selector: selector}
	}
	c.mu.RLock()
	profile, ok := c.layers[selector.key()]
	c.mu.RUnlock()
	if !ok || profile == nil {
		return Snapshot{Selector: selector}
	}
	return Snapshot{Selector: selector, Profile: profile.CloneProfile()}
}

// Layers lists the selectors this container holds, in no particular order.
func (c *Container) Layers() []Selector {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Selector, 0, len(c.layers))
	for layer := range c.layers {
		out = append(out, Selector{Layer: layer})
	}
	return out
}

// Apply moves a bulk update into a layer and returns the bits it turned on.
// It is LoadValues with the container's lock held, so the profile cannot be
// snapshotted halfway through the update.
func (c *Container) Apply(selector Selector, values map[AttrID]AttrValue) uint64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	profile, ok := c.layers[selector.key()]
	if !ok || profile == nil {
		return 0
	}
	return profile.LoadValues(values)
}

// Dirty is the layer's dirty mask, or 0 when the layer is empty.
func (c *Container) Dirty(selector Selector) uint64 {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	profile, ok := c.layers[selector.key()]
	if !ok || profile == nil {
		return 0
	}
	return profile.DirtyMask()
}

// ClearDirty clears one layer's mask. Publishing a change and clearing the
// mask belong together: clear only after the change is out, or the next
// reader is told nothing moved.
func (c *Container) ClearDirty(selector Selector) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if profile, ok := c.layers[selector.key()]; ok && profile != nil {
		profile.ClearDirty()
	}
}
