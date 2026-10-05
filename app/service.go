package app

import "context"

// Service is the main business logic that consumes Mod capabilities.
// Lifecycle: Init → Serve (blocking) → Shutdown.
//
// 启动失败时 App 同样调用 Shutdown 收回已经启动的部分，包括 Init 自己返回错误的情况：Init 可能已经
// 起了后台循环、订阅，App 不知道是哪些。所以 Shutdown 必须容忍部分初始化（未设置的字段跳过）。
// 这次调用限时 5s；没在时限内结束（不配合 ctx、按 ctx 超时返回、panic）时 App 不停 Mod、不释放
// 单实例锁，和正常停机里 Shutdown 不完整的处理相同（RR-20261005-NC-193）。
type Service interface {
	Name() ServiceName
	Init(r *Registry) error             // consume capabilities from registry
	Serve(ctx context.Context) error    // blocking, ctx cancelled on shutdown
	Shutdown(ctx context.Context) error // graceful shutdown with timeout
}
