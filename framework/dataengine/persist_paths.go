package dataengine

// PersistPaths 是单个事务内的持久化变化树，复用 setter 的 SyncPath 路径词汇。
// 它不使用 Sync 的确认版本；父节点替换一旦登记，本事务后续子修改只需读取父节点最终值。
// 调用由持有 Entity 锁的事务串行化；节点不持有业务值，也不会写入 WAL。
type PersistPaths struct {
	root  *SyncChange
	nodes int
}

func (p *PersistPaths) Root() *SyncChange  { return p.root }
func (p PersistPaths) Clone() PersistPaths { return PersistPaths{root: p.root.clone(), nodes: p.nodes} }
func (p *PersistPaths) Record(path SyncPath) {
	if p.root == nil {
		p.root = &SyncChange{}
		p.nodes = 1
	}
	if p.root.Replace != 0 {
		return
	}
	if p.nodes+len(path) > maxSyncChangeNodes {
		p.root = &SyncChange{Replace: 1}
		p.nodes = 1
		return
	}
	node := p.root
	for _, step := range path {
		if node.Replace != 0 {
			return
		}
		if node.Children == nil {
			node.Children = make(map[SyncStep]*SyncChange)
		}
		next := node.Children[step]
		if next == nil {
			next = &SyncChange{}
			node.Children[step] = next
			p.nodes++
		}
		node = next
	}
	p.nodes -= countSyncChanges(node) - 1
	node.Replace = 1
	node.Children = nil
}

// ReplacementCount 只计算整个元素被替换/删除的数量；多个元素各改一个
// 子字段不等于整集合重写，不能因为集合只有一个大对象就退回全量。
func (c *SyncChange) ReplacementCount() int {
	count := 0
	if c != nil {
		for _, child := range c.Children {
			if child.Full() {
				count++
			}
		}
	}
	return count
}
