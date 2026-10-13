package skill

// 面向 Host 的 RuntimeValue 构造函数。IntRuntimeValue 的量纲参数是未导出类型，外部 Host 实现写不出来；
// 这里按 catalog 数据构造带正确量纲的取值结果。

// ResourceRuntimeValue 构造 Host 对 ResourceRead 返回的值。
func ResourceRuntimeValue(value int64) RuntimeValue {
	return IntRuntimeValue(value, quantityResourceAmount)
}

// AttributeRuntimeValue 构造 Host 对 AttributeRead 返回的值，量纲取 catalog 为该句柄声明的量纲
// （句柄不在 catalog 里时为无量纲）。
func AttributeRuntimeValue(catalog GameplayCatalog, handle AttributeHandle, value int64) RuntimeValue {
	quantity := quantityDimensionless
	for _, entry := range catalog.Attributes.Entries {
		if entry.Handle == handle {
			quantity = entry.Quantity
			break
		}
	}
	return IntRuntimeValue(value, quantity)
}
