package dataengine

import "sync"

// SyncStep 是生成 setter 使用的路径片段。Field 是 PB 字段号；Key 只保存
// 可比较的 map key 或 list 索引。它不保存业务值，也不进入持久化编码。
type SyncStep struct {
	Field uint32
	Key   any
}
type SyncPath []SyncStep

func SyncField(field uint32, tail SyncPath) SyncPath {
	return append(SyncPath{{Field: field}}, tail...)
}
func SyncElement(key any, tail SyncPath) SyncPath { return append(SyncPath{{Key: key}}, tail...) }

// SyncChange 记录需要读取最终值的位置。Replace 的版本独立于子节点，
// 旧冻结内容结算后可以消掉旧的整体替换，同时保留之后新写的子字段。
type SyncChange struct {
	Replace  uint64
	Children map[SyncStep]*SyncChange
}

func (c *SyncChange) Child(step SyncStep) *SyncChange {
	if c == nil {
		return nil
	}
	return c.Children[step]
}
func (c *SyncChange) Full() bool { return c == nil || c.Replace != 0 }
func (c *SyncChange) clone() *SyncChange {
	if c == nil {
		return nil
	}
	out := &SyncChange{Replace: c.Replace}
	if len(c.Children) > 0 {
		out.Children = make(map[SyncStep]*SyncChange, len(c.Children))
		for key, child := range c.Children {
			out.Children[key] = child.clone()
		}
	}
	return out
}
func (c *SyncChange) prune(version uint64) bool {
	if c.Replace <= version {
		c.Replace = 0
	}
	for key, child := range c.Children {
		if child.prune(version) {
			delete(c.Children, key)
		}
	}
	return c.Replace == 0 && len(c.Children) == 0
}

// SyncChangeSnapshot 是一次受 Entity 锁保护的捕获；调用方可只读地遍历。
type SyncChangeSnapshot struct {
	Version uint64
	Root    *SyncChange
}

type syncChanges struct {
	mu           sync.Mutex
	version      uint64
	acknowledged uint64
	root         *SyncChange
	nodes        int
}

// 变化元数据有界；超过节点预算时收敛为 DAO 全量，不丢同步事实。
const maxSyncChangeNodes = 4096

func (s *syncChanges) mark(path SyncPath) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version++
	if s.root == nil {
		s.root = &SyncChange{}
		s.nodes = 1
	}
	if s.nodes+len(path) > maxSyncChangeNodes {
		s.root = &SyncChange{Replace: s.version}
		s.nodes = 1
		return
	}
	node := s.root
	for _, step := range path {
		if node.Children == nil {
			node.Children = make(map[SyncStep]*SyncChange)
		}
		child := node.Children[step]
		if child == nil {
			child = &SyncChange{}
			node.Children[step] = child
			s.nodes++
		}
		node = child
	}
	// 新的整体赋值覆盖先前子操作；旧版本在途时仍可按版本正确结算。
	s.nodes -= countSyncChanges(node) - 1
	node.Replace = s.version
	node.Children = nil
}
func (s *syncChanges) snapshot() SyncChangeSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SyncChangeSnapshot{s.version, s.root.clone()}
}
func (s *syncChanges) ack(version uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acknowledged = max(s.acknowledged, version)
	if s.root != nil && s.root.prune(s.acknowledged) {
		s.root = nil
	}
	s.nodes = countSyncChanges(s.root)
}
func (s *syncChanges) restore(snapshot SyncChangeSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version = max(s.version, snapshot.Version) // 不复用已发出的捕获序号。
	s.root = snapshot.Root.clone()
	// 并发交付可能已确认事务开始前的记录，回滚不能把它们重新复活。
	if s.root != nil && s.root.prune(s.acknowledged) {
		s.root = nil
	}
	s.nodes = countSyncChanges(s.root)
}
func countSyncChanges(node *SyncChange) int {
	if node == nil {
		return 0
	}
	n := 1
	for _, child := range node.Children {
		n += countSyncChanges(child)
	}
	return n
}

// MarkSyncPath 与已有字段脏位并存，只服务客户端增量，不改变服务间同步。
func (t *Tracker) MarkSyncPath(mask uint64, path SyncPath) { t.MarkSync(mask); t.changes.mark(path) }
func (t *Tracker) SyncChanges() SyncChangeSnapshot         { return t.changes.snapshot() }
func (t *Tracker) AcknowledgeSyncChanges(version uint64)   { t.changes.ack(version) }

func (t *Tracker) SyncChangeVersion() uint64 {
	t.changes.mu.Lock()
	defer t.changes.mu.Unlock()
	return t.changes.version
}

// ReadSyncChanges 供生成编码器在锁内读取路径，避免为编码复制整棵变化树。
// fn 必须只读，不能保留节点、修改 DAO 或再次访问 Tracker 的变化记录。
// 业务值仍由调用方持有的 Entity 锁保护；此锁仅串行化路径读取与交付确认。
func (t *Tracker) ReadSyncChanges(fn func(*SyncChange) []byte) []byte {
	t.changes.mu.Lock()
	defer t.changes.mu.Unlock()
	return fn(t.changes.root)
}
