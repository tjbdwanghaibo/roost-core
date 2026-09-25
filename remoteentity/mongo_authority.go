package remoteentity

import (
	"context"
	"errors"
	"math"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// WriteAuthority 将所有权、最新写许可与数据版本放在提交使用的同一个持久化域。
// GrantWrite 每次调用必须使用全新的 token；未知结果不能换 token 自动重试。
// ownerSID=0 表示共享锁写入，否则必须是当前独占所有者。
type WriteAuthority interface {
	entity.IRemoteEntityOwnershipStore
	GrantWrite(context.Context, int64, string, int32) (WriteGrant, error)
}

type WriteGrant struct {
	Version   int64
	Fence     uint64
	Ownership entity.RemoteEntityMarkerLease
}

// WriteAuthorityProvider 必须同时保证提交时对最新许可做原子校验。
type WriteAuthorityProvider interface{ WriteAuthority() WriteAuthority }

var ErrRemoteAuthorityInvalid = errors.New("remote_entity: invalid or unsupported ownership metadata")

type mongoAuthority struct {
	ID      int64  `bson:"_id"`
	Version int64  `bson:"_ver"`
	Enabled bool   `bson:"_authority"`
	Owner   int32  `bson:"_owner_sid"`
	Shared  bool   `bson:"_owner_shared"`
	Marker  uint64 `bson:"_owner_epoch"`
	Route   uint64 `bson:"_owner_route"`
	Fence   uint64 `bson:"_grant_fence"`
	Token   string `bson:"_grant_token"`
}

func (d mongoAuthority) lease() entity.RemoteEntityMarkerLease {
	return entity.RemoteEntityMarkerLease{OwnerSid: d.Owner, Shared: d.Shared, MarkerEpoch: d.Marker, RouteEpoch: d.Route}
}

func (s *MongoCommitter) WriteAuthority() WriteAuthority {
	return s
}

func (s *MongoCommitter) readAuthority(ctx context.Context, id int64) (mongoAuthority, error) {
	var d mongoAuthority
	err := s.controlDB().Collection(remoteMetaCollection).FindOne(ctx, bson.M{"_id": id}, &d)
	if err == nil && !d.Enabled {
		err = ErrRemoteAuthorityInvalid
	} else if err == nil && (!validOwnershipLease(d.lease()) || d.Version < 0 || d.Fence > math.MaxInt64 || d.Marker > math.MaxInt64 || d.Route > math.MaxInt64) {
		err = entity.ErrRemoteFenced
	}
	return d, err
}

func (s *MongoCommitter) GetOwnership(ctx context.Context, id int64) (entity.RemoteEntityMarkerLease, bool, error) {
	d, err := s.readAuthority(ctx, id)
	if errors.Is(err, fmongo.ErrNotFound) {
		return entity.RemoteEntityMarkerLease{}, false, nil
	}
	return d.lease(), err == nil, err
}

func (s *MongoCommitter) ClaimOwnership(ctx context.Context, id int64, owner int32) (entity.RemoteEntityMarkerLease, error) {
	if id == 0 || owner == 0 {
		return entity.RemoteEntityMarkerLease{}, entity.ErrRemoteFenced
	}
	d, err := s.readAuthority(ctx, id)
	if errors.Is(err, fmongo.ErrNotFound) {
		d = mongoAuthority{ID: id, Enabled: true, Owner: owner, Marker: 1, Route: 1}
		_, err = s.controlDB().Collection(remoteMetaCollection).InsertOne(ctx, d)
		if err != nil {
			d, err = s.readAuthority(ctx, id)
		}
	}
	if err != nil {
		return entity.RemoteEntityMarkerLease{}, err
	}
	if d.Owner != owner {
		return entity.RemoteEntityMarkerLease{}, entity.ErrRemoteFenced
	}
	return d.lease(), nil
}

func authorityFilter(id int64, lease entity.RemoteEntityMarkerLease) bson.M {
	return bson.M{"_id": id, "_authority": true, "_owner_sid": lease.OwnerSid, "_owner_shared": lease.Shared, "_owner_epoch": lease.MarkerEpoch, "_owner_route": lease.RouteEpoch}
}

func (s *MongoCommitter) GrantWrite(ctx context.Context, id int64, token string, owner int32) (WriteGrant, error) {
	if id == 0 || token == "" {
		return WriteGrant{}, entity.ErrRemoteFenced
	}
	filter := bson.M{"_id": id, "_authority": true, "_grant_fence": bson.M{"$gte": 0, "$lt": int64(math.MaxInt64)}, "_ver": bson.M{"$gte": 0}, "_owner_shared": owner == 0}
	if owner != 0 {
		filter["_owner_sid"] = owner
	}
	// 一次 FindAndModify 同时取得当前版本并递增许可，不先读后写；与提交争用同一文档。
	var next mongoAuthority
	err := s.controlDB().Collection(remoteMetaCollection).FindOneAndUpdate(ctx, filter, bson.M{"$inc": bson.M{"_grant_fence": int64(1)}, "$set": bson.M{"_grant_token": token}}, &next, fmongo.FindOneAndUpdateOption{ReturnAfter: true})
	if err != nil {
		// token 每次调用唯一。丢回复只查询本次结果，不再递增；被替换则拒绝准入。
		observed, readErr := s.readAuthority(ctx, id)
		if readErr != nil || observed.Token != token {
			return WriteGrant{}, errors.Join(entity.ErrRemoteFenced, err, readErr)
		}
		next = observed
	}
	if !validOwnershipLease(next.lease()) || next.Fence == 0 || next.Fence > math.MaxInt64 || next.Marker > math.MaxInt64 || next.Route > math.MaxInt64 || next.Version < 0 {
		return WriteGrant{}, entity.ErrRemoteFenced
	}
	return WriteGrant{Version: next.Version, Fence: next.Fence, Ownership: next.lease()}, nil
}

func (s *MongoCommitter) changeOwnership(ctx context.Context, id int64, old, next entity.RemoteEntityMarkerLease) (entity.RemoteEntityMarkerLease, error) {
	if !validOwnershipLease(old) || next.OwnerSid == 0 || old.MarkerEpoch >= math.MaxInt64 || next.RouteEpoch > math.MaxInt64 {
		return entity.RemoteEntityMarkerLease{}, entity.ErrRemoteFenced
	}
	next.MarkerEpoch = old.MarkerEpoch + 1
	filter := authorityFilter(id, old)
	// 同一文档上的写与数据提交互斥；代际更新立即撤销旧许可。
	var d mongoAuthority
	err := s.controlDB().Collection(remoteMetaCollection).FindOneAndUpdate(ctx, filter, bson.M{"$set": bson.M{"_owner_sid": next.OwnerSid, "_owner_shared": next.Shared, "_owner_epoch": next.MarkerEpoch, "_owner_route": next.RouteEpoch, "_grant_token": ""}}, &d, fmongo.FindOneAndUpdateOption{ReturnAfter: true})
	if err != nil {
		return entity.RemoteEntityMarkerLease{}, errors.Join(entity.ErrRemoteFenced, err)
	}
	return d.lease(), nil
}

func (s *MongoCommitter) EnterSharedExpected(ctx context.Context, id int64, old entity.RemoteEntityMarkerLease) (entity.RemoteEntityMarkerLease, error) {
	if old.Shared {
		return entity.RemoteEntityMarkerLease{}, entity.ErrRemoteFenced
	}
	next := old
	next.Shared = true
	return s.changeOwnership(ctx, id, old, next)
}
func (s *MongoCommitter) LeaveSharedExpected(ctx context.Context, id int64, old entity.RemoteEntityMarkerLease) (entity.RemoteEntityMarkerLease, error) {
	if !old.Shared {
		return entity.RemoteEntityMarkerLease{}, entity.ErrRemoteFenced
	}
	next := old
	next.Shared = false
	return s.changeOwnership(ctx, id, old, next)
}
func (s *MongoCommitter) TransferExpected(ctx context.Context, id int64, old entity.RemoteEntityMarkerLease, owner int32) (entity.RemoteEntityMarkerLease, error) {
	if old.RouteEpoch >= math.MaxInt64 {
		return entity.RemoteEntityMarkerLease{}, entity.ErrRemoteFenced
	}
	next := old
	next.OwnerSid = owner
	next.RouteEpoch++
	return s.changeOwnership(ctx, id, old, next)
}
