package entity

// SubjectSyncChangePacker 使用 setter 变化记录生成增量。捕获时只读取当前
// 变化版本，成功内容结算后才确认；后续写入必须保留，Abort 不确认。
// 所有 Pack 调用及版本捕获由 Entity 锁串行化；确认必须只操作自己的线程安全元数据。
type SubjectSyncChangePacker interface {
	SubjectSyncChangeVersion() uint64
	AcknowledgeSubjectSyncChanges(uint64)
}

func (s *SubjectSyncState) captureChangeVersion(item *PreparedSubjectSync, packer SubjectSyncPacker) *PreparedSubjectSync {
	if tracked, ok := packer.(SubjectSyncChangePacker); ok {
		item.changeVersion = tracked.SubjectSyncChangeVersion()
		item.changePacker = tracked
	}
	return item
}
