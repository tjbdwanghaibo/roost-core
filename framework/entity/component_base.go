package entity

// ComponentBase 是 ComponentInterfaceBase 的默认实现，嵌入具体组件即可获得空操作的默认方法。
type ComponentBase struct{}

func (b *ComponentBase) Name() string { return "" }

func (b *ComponentBase) OnInitFinish(param *EntityCreateParam, isCreate bool) error {
	return nil
}

func (b *ComponentBase) OnDestroy(rea EntityDestroyReason) {}
