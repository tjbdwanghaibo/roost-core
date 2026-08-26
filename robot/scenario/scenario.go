// Package scenario is the robot's blocking behavior tree: small composable
// nodes driving named actions. Ported from the cube robot service's mission
// package (renamed — core/taskflow's Mission is the server-side player-task
// state machine, an unrelated concept) and completed with the combinators
// business code had to hand-roll: Parallel, Selector, BestEffort, Cond.
//
// Nodes are stateless closures; a scenario can safely run for many robots
// concurrently. Randomness comes from the robot's seeded RNG so a run is
// reproducible.
package scenario

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/cube-core/robot"
)

// Scenario is a named behavior tree.
type Scenario interface {
	Name() string
	Run(context.Context, *robot.Context) error
}

type Node interface {
	Run(context.Context, *robot.Context) error
}

type NodeFunc func(context.Context, *robot.Context) error

func (f NodeFunc) Run(ctx context.Context, rb *robot.Context) error {
	return f(ctx, rb)
}

type simpleScenario struct {
	name string
	root Node
}

// New builds a named scenario from a root node.
func New(name string, root Node) Scenario {
	return &simpleScenario{name: normalizeName(name), root: root}
}

func (m *simpleScenario) Name() string { return m.name }

func (m *simpleScenario) Run(ctx context.Context, rb *robot.Context) error {
	if m.root == nil {
		return nil
	}
	return m.root.Run(ctx, rb)
}

type Registry struct {
	mu        sync.RWMutex
	scenarios map[string]Scenario
}

func NewRegistry() *Registry {
	return &Registry{scenarios: make(map[string]Scenario)}
}

func (r *Registry) Register(s Scenario) error {
	if r == nil {
		return errors.New("robot scenario: registry is nil")
	}
	if s == nil {
		return errors.New("robot scenario: scenario is nil")
	}
	name := normalizeName(s.Name())
	if name == "" {
		return errors.New("robot scenario: name is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.scenarios[name]; ok {
		return fmt.Errorf("robot scenario: duplicate %q", name)
	}
	r.scenarios[name] = s
	return nil
}

func (r *Registry) MustRegister(s Scenario) {
	if err := r.Register(s); err != nil {
		panic(err)
	}
}

func (r *Registry) Get(name string) (Scenario, bool) {
	if r == nil {
		return nil, false
	}
	name = normalizeName(name)
	r.mu.RLock()
	s, ok := r.scenarios[name]
	r.mu.RUnlock()
	return s, ok
}

func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	names := make([]string, 0, len(r.scenarios))
	for name := range r.scenarios {
		names = append(names, name)
	}
	r.mu.RUnlock()
	sort.Strings(names)
	return names
}

// --- combinators ---

// Action runs one named action with a param.
func Action(name string, param any) Node {
	return NodeFunc(func(ctx context.Context, rb *robot.Context) error {
		return rb.Do(ctx, name, param)
	})
}

// Wait sleeps for d, honoring ctx.
func Wait(d time.Duration) Node {
	return NodeFunc(func(ctx context.Context, _ *robot.Context) error {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	})
}

// Sequence runs nodes in order, stopping at the first error.
func Sequence(nodes ...Node) Node {
	return NodeFunc(func(ctx context.Context, rb *robot.Context) error {
		for _, node := range nodes {
			if node == nil {
				continue
			}
			if err := node.Run(ctx, rb); err != nil {
				return err
			}
		}
		return nil
	})
}

// Selector runs nodes in order until one succeeds (behavior-tree fallback);
// it fails only when every child fails, returning the last error.
func Selector(nodes ...Node) Node {
	return NodeFunc(func(ctx context.Context, rb *robot.Context) error {
		var last error
		for _, node := range nodes {
			if node == nil {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := node.Run(ctx, rb); err != nil {
				last = err
				continue
			}
			return nil
		}
		return last
	})
}

// Parallel runs all nodes concurrently and waits for them; errors join.
// Robots are single actors — use Parallel only for nodes that are safe to
// interleave (independent waits, independent calls).
func Parallel(nodes ...Node) Node {
	return NodeFunc(func(ctx context.Context, rb *robot.Context) error {
		var wg sync.WaitGroup
		errs := make([]error, len(nodes))
		for i, node := range nodes {
			if node == nil {
				continue
			}
			wg.Add(1)
			go func(slot int, n Node) {
				defer wg.Done()
				errs[slot] = n.Run(ctx, rb)
			}(i, node)
		}
		wg.Wait()
		return errors.Join(errs...)
	})
}

// BestEffort runs node and swallows its error (context cancellation still
// propagates) — the standard "optional step" wrapper business code used to
// hand-roll.
func BestEffort(node Node) Node {
	return NodeFunc(func(ctx context.Context, rb *robot.Context) error {
		if node == nil {
			return nil
		}
		if err := node.Run(ctx, rb); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		return nil
	})
}

// Cond runs then when predicate returns true, otherwise els (which may be
// nil).
func Cond(predicate func(*robot.Context) bool, then Node, els Node) Node {
	return NodeFunc(func(ctx context.Context, rb *robot.Context) error {
		if predicate != nil && predicate(rb) {
			if then == nil {
				return nil
			}
			return then.Run(ctx, rb)
		}
		if els == nil {
			return nil
		}
		return els.Run(ctx, rb)
	})
}

// Loop repeats node times times (times <= 0 loops until ctx cancels).
func Loop(times int, node Node) Node {
	return NodeFunc(func(ctx context.Context, rb *robot.Context) error {
		if node == nil {
			return nil
		}
		for i := 0; times <= 0 || i < times; i++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := node.Run(ctx, rb); err != nil {
				return err
			}
		}
		return nil
	})
}

// Retry re-runs node up to times attempts, returning the last error.
func Retry(times int, node Node) Node {
	return NodeFunc(func(ctx context.Context, rb *robot.Context) error {
		if node == nil {
			return nil
		}
		if times <= 0 {
			times = 1
		}
		var last error
		for i := 0; i < times; i++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := node.Run(ctx, rb); err != nil {
				last = err
				continue
			}
			return nil
		}
		return last
	})
}

// Timeout bounds node with a derived deadline.
func Timeout(d time.Duration, node Node) Node {
	return NodeFunc(func(ctx context.Context, rb *robot.Context) error {
		if node == nil {
			return nil
		}
		if d <= 0 {
			return node.Run(ctx, rb)
		}
		timeoutCtx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		return node.Run(timeoutCtx, rb)
	})
}

type WeightedNode struct {
	Weight int
	Node   Node
}

func Weighted(weight int, node Node) WeightedNode {
	return WeightedNode{Weight: weight, Node: node}
}

// Random picks one node by weight using the robot's seeded RNG (falls back
// to the global source when the robot has no seed) — runs stay reproducible
// per seed.
func Random(nodes ...WeightedNode) Node {
	return NodeFunc(func(ctx context.Context, rb *robot.Context) error {
		total := 0
		for _, item := range nodes {
			if item.Node != nil && item.Weight > 0 {
				total += item.Weight
			}
		}
		if total <= 0 {
			return nil
		}
		n := robotIntn(rb, total)
		for _, item := range nodes {
			if item.Node == nil || item.Weight <= 0 {
				continue
			}
			if n < item.Weight {
				return item.Node.Run(ctx, rb)
			}
			n -= item.Weight
		}
		return nil
	})
}

const rngKey = "scenario:rng"

// lockedRand keeps the per-robot RNG safe under Parallel branches.
type lockedRand struct {
	mu  sync.Mutex
	rng *rand.Rand
}

func (l *lockedRand) intn(n int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rng.Intn(n)
}

func robotIntn(rb *robot.Context, n int) int {
	if rb == nil || rb.Blackboard == nil {
		return rand.Intn(n)
	}
	if raw, ok := rb.Blackboard.Get(rngKey); ok {
		if rng, ok := raw.(*lockedRand); ok {
			return rng.intn(n)
		}
	}
	if rb.Seed != 0 {
		rng := &lockedRand{rng: rand.New(rand.NewSource(rb.Seed))}
		rb.Blackboard.Set(rngKey, rng)
		return rng.intn(n)
	}
	return rand.Intn(n)
}

func normalizeName(name string) string {
	return strings.TrimSpace(strings.ToLower(name))
}
