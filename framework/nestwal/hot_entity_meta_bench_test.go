package nestwal_test

import corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"

// hotEntityAsyncCompletionMeta 单独成文件：要在还没有 HandlerMeta.AsyncCompletion 的提交上做对照时，只把这个文件换成返回
// 只声明 pipelined 的版本（与 pipelined 变体相同）即可编译。2026-11-15 的前后对照只换了 nestwal（两侧都有这个声明）。
func hotEntityAsyncCompletionMeta() corenest.HandlerMeta {
	return corenest.HandlerMeta{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityPipelined, AsyncCompletion: true}
}
