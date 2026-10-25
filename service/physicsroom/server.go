package physicsroom

import "context"

// Server 的生命周期由 app.Service 管理；Shutdown 排空所有房间。
type Server struct{ service Rooms }

func NewServer(service Rooms) *Server             { return &Server{service: service} }
func (s *Server) Service() Rooms                  { return s.service }
func (s *Server) Serve(ctx context.Context) error { <-ctx.Done(); return nil }
func (s *Server) Shutdown(ctx context.Context) error {
	local, ok := s.service.(*Service)
	if !ok {
		return ErrRequestInvalid
	}
	return local.Shutdown(ctx)
}
