import sys
p=sys.argv[1]; s=open(p).read()
anchor='func registerCalls(actions *action.Registry, protocols *protocol.Registry) {\n'
assert anchor in s
add=anchor+'''	// 压测专用（未入库）：客户端视野请求。
	action.MustRegisterCall[pb.SceneViewRequest, pb.SceneViewResponse](actions, protocols, "scene_view", msgid.MsgSceneView,
		action.OnResp(func(_ *robot.Context, resp *pb.SceneViewResponse) error {
			return coded(resp.Code, resp.Reason)
		}))
'''
s=s.replace(anchor,add); open(p,'w').write(s)
s=open(p).read()
a1='''	switch message := value.(type) {
	case *pb.EnterGameRequest:
		return pb.MarshalEnterGameRequest(message)
'''
assert a1 in s
s=s.replace(a1,'''	switch message := value.(type) {
	case *pb.SceneViewRequest:
		return pb.MarshalSceneViewRequest(message)
	case *pb.EnterGameRequest:
		return pb.MarshalEnterGameRequest(message)
''')
a2='''	switch message := target.(type) {
	case *pb.EnterGameResponse:
'''
assert a2 in s
s=s.replace(a2,'''	switch message := target.(type) {
	case *pb.SceneViewResponse:
		decoded, err := pb.UnmarshalSceneViewResponse(data)
		if err != nil {
			return err
		}
		*message = *decoded
		return nil
	case *pb.EnterGameResponse:
''')
open(p,'w').write(s)
