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
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/robot"
	"github.com/tjbdwanghaibo/roost-core/robot/session"
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
		start := time.Now()
		defer func() { noteWait(ctx, time.Since(start)) }()
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
			noteRetryAttempt(ctx)
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

// Step 给一段子树命名并计时：每跑一次，把耗时、成败、子树里 Retry 实际执行的次数和 Wait 睡掉的时间
// 记进机器人的 Recorder（本次运行的报告，见 session.StepReport）。机器人没有 Recorder 时照常执行、不计时。
// 嵌套的 Step 各自记一份，内层的 Retry / Wait 同时算进每一层外层。
// YAML 场景由 ParseSpec 自动给每个顶层步骤套上 Step，代码搭的场景按需自己套。
func Step(name string, node Node) Node {
	return StepInGroup("", name, node)
}

// StepInGroup 同 Step，并把这一步归入 group（去首尾空白、转小写；YAML 顶层步骤写 group 时由 ParseSpec 套上）。
// 这一步的耗时还累计到本次场景运行在该组上的时间（robot.Context.AddGroupTime），报告据此给出组内时间与
// “除去该组”的场景时间（session.GroupReport），门槛可以只卡后者（loadtest 的 p95_excluding:<组>）。
// 同组嵌套只按最外层算一次。
func StepInGroup(group, name string, node Node) Node {
	group = normalizeName(group)
	return NodeFunc(func(ctx context.Context, rb *robot.Context) error {
		if node == nil {
			return nil
		}
		if rb == nil || rb.Recorder == nil {
			return node.Run(ctx, rb)
		}
		frame := &stepFrame{parent: stepFrameOf(ctx), group: group}
		start := time.Now()
		err := node.Run(context.WithValue(ctx, stepFrameKey{}, frame), rb)
		elapsed := time.Since(start)
		rb.Recorder.FinishStep(name, session.StepSample{
			Group:         group,
			Elapsed:       elapsed,
			Waited:        time.Duration(frame.waited.Load()),
			RetryAttempts: frame.retryAttempts.Load(),
			OK:            err == nil,
		})
		if group != "" && !frame.parent.inGroup(group) {
			rb.AddGroupTime(group, elapsed)
		}
		return err
	})
}

// stepFrame 是一次 Step 执行期间的计数；Parallel 分支会并发写，所以用原子量。
type stepFrame struct {
	parent        *stepFrame
	group         string
	waited        atomic.Int64
	retryAttempts atomic.Int64
}

type stepFrameKey struct{}

// inGroup 报告这一层或任一外层 Step 是否已属于 group（那一层会把整段时间算进组里）。
func (f *stepFrame) inGroup(group string) bool {
	for ; f != nil; f = f.parent {
		if f.group == group {
			return true
		}
	}
	return false
}

func stepFrameOf(ctx context.Context) *stepFrame {
	frame, _ := ctx.Value(stepFrameKey{}).(*stepFrame)
	return frame
}

// noteWait / noteRetryAttempt 把 Wait 时间与 Retry 次数记到所在的每一层 Step；不在 Step 里时什么都不做。
func noteWait(ctx context.Context, d time.Duration) {
	for frame := stepFrameOf(ctx); frame != nil; frame = frame.parent {
		frame.waited.Add(int64(d))
	}
}

func noteRetryAttempt(ctx context.Context) {
	for frame := stepFrameOf(ctx); frame != nil; frame = frame.parent {
		frame.retryAttempts.Add(1)
	}
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
