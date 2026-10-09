package chat

import "context"

// Server 运行本地领域任务；配置和 Registry 查找由 Wiring 负责。
// 周期工作只由本地 owner 的进程启动，远端客户端不拥有运行循环。
type Server struct{ service Messaging }

func NewServer(service Messaging) *Server         { return &Server{service: service} }
func (s *Server) Service() Messaging              { return s.service }
func (s *Server) Serve(ctx context.Context) error { return s.run(ctx) }
func (s *Server) Shutdown(context.Context) error  { return nil }
