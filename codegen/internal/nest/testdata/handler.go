package testdata

import (
	"github.com/tjbdwanghaibo/cube/game/view"
	"github.com/tjbdwanghaibo/roost-core/entity"
)

var _ view.EntityKind = view.EntityKindPlayer

// IPlayerEntity is a sample entity interface for testing.
type IPlayerEntity interface {
	ID() int64
	Name() string
}

// IAllianceEntity is a sample alliance entity interface.
type IAllianceEntity interface {
	ID() int64
}

type RemoteViewRequest struct {
	TargetPlayerViewRef entity.RemoteViewRef `remote:"view.PlayerViewMapSnapshot,consistency=monotonic,required"`
	LivePlayerViewRef   entity.RemoteViewRef `remote:"view.PlayerViewMapSnapshot,consistency=strong"`
}

// --- Single entity handler ---

//roost:nest
func handlerPlayerLogin(p IPlayerEntity, token string) {
}

// --- Single entity handler with return ---

//roost:nest
func handlerPlayerGetLevel(p IPlayerEntity) (ret int32, err error) {
	return
}

// --- Multi entity handler ---

//roost:nest rollback=undo durability=strict
func handlerTransferItem(from IPlayerEntity, to IPlayerEntity, itemId int64, count int32) {
}

// --- Group entity handler ---

//roost:nest
func handlerBroadcastToGroup(targets []IPlayerEntity, sender IPlayerEntity, msg string) {
}

// --- Group handler with return ---

//roost:nest
func handlerGroupCalc(targets []IPlayerEntity, val int64) (ret int64, err error) {
	return
}

// --- Handler with generated remote access ---

//roost:nest
func handlerRemoteView(p IPlayerEntity, req RemoteViewRequest) {
	_, _ = req.TargetPlayer()
	_ = req.MustTargetPlayer()
	_, _ = req.LivePlayer()
}
