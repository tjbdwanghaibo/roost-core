//go:build protocoldef

package protocoldef

// SceneReadyRequest says the client has installed its state decoder and can
// take frames. The server holds a session that has just entered the game and
// starts replicating to it only on this message: without it the first
// snapshot races the client's decoder and may land before anyone listens.
type SceneReadyRequest struct{}

type SceneReadyResponse struct {
	Code   int32  `pb:"1"`
	Reason string `pb:"2"`
}

//roost:protocol group=game handler=player
type SceneReadyProtocol interface {
	//roost:msg id=10024
	SceneReady(SceneReadyRequest) SceneReadyResponse
}
