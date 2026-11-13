package cellworld

// 扩展消息（线上格式 v7）：别的模块借 cellworld 的进程间通道发自己的消息，不另起连接与主题。
//
// 用法：收件方在 NodeConfig.Ext 里按命名空间登记处理器；发件方调 SendExt。消息走与 Node 自己的消息相同的路：发件
// 排进出站队列、由出站 goroutine 编码发出（所以可以在 Nest 的提交后回调里调用——那里不能 Dispatch，也不该做 I/O），
// 收件在传输回调上、同一个准入门内交给处理器。语义与 cellworld 的其余消息相同：至多一次，可能丢失、重复、乱序；
// 可靠性（版本、重传、补齐）由使用方的协议负责。第一个使用方是可编辑地形的边界块副本（gameplay/terrain 的
// Replicator，命名空间 "terrain"）。

// ExtHandler 处理一条扩展消息：from 是发件进程的 sid，body 是发件方 SendExt 给的字节（只在调用期间有效）。
// 在传输回调上调用，必须快速返回、不能阻塞。
type ExtHandler func(from int32, body []byte)

// SendExt 把 body 作为命名空间 space 的扩展消息发给 sid（发给本进程自己的 sid 也可以，经传输回到本进程的收件）。
// 只做非阻塞入队：出站队列满时丢弃并计数（OutboxDrops），返回 false。body 入队后归 Node，调用方不得再修改。
// space 不合法时返回 false、不发。任何 goroutine 可调用，包括 Nest 的提交后回调。
func (n *Node) SendExt(sid int32, space string, body []byte) bool {
	n.lifecycle.Lock()
	defer n.lifecycle.Unlock()
	stopping := n.lifecycle.stopping
	if stopping || sid == 0 || !validWorld(space) {
		return false
	}
	ok := n.enqueue(job{kind: jobSend, send: outgoing{sid: sid, msg: &wireMsg{Kind: kindExt, Ext: &extMsg{Space: space, Body: body}}}})
	if ok {
		n.stats.extOut.Add(1)
	}
	return ok
}

// onExt 在传输回调上把扩展消息交给命名空间的处理器；没有处理器时计数丢弃。
func (n *Node) onExt(from int32, m *extMsg) {
	handler := n.cfg.Ext[m.Space]
	if handler == nil {
		n.stats.extDrops.Add(1)
		return
	}
	n.stats.extIn.Add(1)
	handler(from, m.Body)
}
