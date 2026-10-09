package mail

import "context"

// Server 运行本地领域任务；配置和 Registry 查找由 Wiring 负责。
// 只有拥有本地能力的进程启动周期任务，远端客户端不拥有运行循环。
type Server struct{ service Mail }

func NewServer(service Mail) *Server              { return &Server{service: service} }
func (s *Server) Service() Mail                   { return s.service }
func (s *Server) Serve(ctx context.Context) error { return s.run(ctx) }
func (s *Server) Shutdown(context.Context) error  { return nil }
