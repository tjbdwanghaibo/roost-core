package attribute

import "sync"

// Selector 指定主体属性的一层。各层的含义由游戏决定，框架只负责把它们分开。这里命名了两层，因为每个游戏都以某种
// 写法拥有它们：Base 是主体不加任何东西时的样子，Final 是一切都施加之后的样子。
type Selector struct {
	Layer string
}

var (
	// Base 是主体自身的属性。
	Base = Selector{Layer: "base"}
	// Final 是展示给客户端的最终视图。
	Final = Selector{Layer: "final"}
)

// String 让 selector 在错误与日志里可读；空 Layer 视为 Base。
func (s Selector) String() string {
	if s.Layer == "" {
		return Base.Layer
	}
	return s.Layer
}

// key 是层在容器 map 里的键，与 String 相同（空 Layer 视为 Base）。
func (s Selector) key() string { return s.String() }

// Snapshot 是某一时刻读到的一层。Profile 是副本：修改它碰不到来源容器，所以快照可以安全地交给渲染端、公式或另一个
// goroutine。空层的快照 Profile 为 nil——生成的带类型访问器对它返回 (nil, false)，而不是 panic。
type Snapshot struct {
	Selector Selector
	Profile  Profile
}

// Container 持有主体的各层属性，可以并发使用。属性只在实体锁内变化的游戏会多付一个用不上的互斥锁，但这比在从第二个
// goroutine 读属性的游戏里防住的 bug 便宜。
type Container struct {
	mu     sync.RWMutex
	layers map[string]Profile
}

// NewContainer 返回空容器。
func NewContainer() *Container {
	return &Container{layers: make(map[string]Profile, 2)}
}

// Install 把 profile 放进一层，替换原有内容。容器保存的是 profile 本身而不是副本：调用方继续拥有它，容器只负责再
// 找到它。
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

// Remove 删除一层，报告原先是否存在。
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

// Live 返回一层持有的 profile，不复制。它只给主体的拥有者用——已经持有游戏所用的锁的代码——绝不给可能比这次调用活得
// 更久的读者；那些读者用 Snapshot。
func (c *Container) Live(selector Selector) (Profile, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	profile, ok := c.layers[selector.key()]
	return profile, ok
}

// Snapshot 复制出一层。复制在容器锁内完成，快照不会读到本容器自己的修改方法（Apply / ClearDirty）写到一半的 profile。
// 经 Live 拿到的 profile 上的写入归拥有者负责，不在保护范围内：拥有者用自己的锁串行化它们。
//
// 复制必须在读锁内完成：若先 RUnlock 再 CloneProfile，Apply 能在复制中途拿到写锁改写同一份 profile，快照读到撕裂
// 状态，-race 下是数据竞争。
func (c *Container) Snapshot(selector Selector) Snapshot {
	if c == nil {
		return Snapshot{Selector: selector}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	profile, ok := c.layers[selector.key()]
	if !ok || profile == nil {
		return Snapshot{Selector: selector}
	}
	return Snapshot{Selector: selector, Profile: profile.CloneProfile()}
}

// Layers 列出容器持有的 selector，顺序不定。
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

// Apply 把一次批量更新写进一层，返回它置上的位。它就是持有容器锁时的 LoadValues，profile 不会在更新到一半时被快照。
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

// Dirty 返回该层的脏掩码；层为空时返回 0。
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

// ClearDirty 清掉一层的掩码。发布变化与清掩码是一对：只在变化已经发出之后再清，否则下一个读者会以为什么都没变。
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
