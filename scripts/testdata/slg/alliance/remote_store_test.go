package alliance

import (
	"context"
	"encoding/json"
	"errors"

	sender "example.com/slg-practice/game/handler/syncsender"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// 教学适配器只实现联盟所需三项操作，不冒充通用 versionstore 后端。
// 先读版本，再通过生成 Sender 做条件写；仅明确 CAS 冲突重算，未知结果直接返回。
type remoteAllianceStore struct {
	owner *allianceOwner
	key   string
}
type allianceRecord struct {
	Version uint64
	Value   state
}

func (s *remoteAllianceStore) Get(ctx context.Context, key string) (versionstore.Versioned[state], bool, error) {
	if key != s.key {
		return versionstore.Versioned[state]{}, false, errRefused
	}
	raw, e := sender.NewAllianceAuthoritySender(s.owner.nest).Sync_AllianceRemoteRead(ctx, s.owner.id)
	if e != nil || raw == "" {
		return versionstore.Versioned[state]{}, false, e
	}
	var r allianceRecord
	if e = json.Unmarshal([]byte(raw), &r); e != nil {
		return versionstore.Versioned[state]{}, false, e
	}
	if r.Version == 0 || r.Value.Key.String() != key {
		return versionstore.Versioned[state]{}, false, errRefused
	}
	return versionstore.Versioned[state]{Version: r.Version, Value: r.Value}, true, nil
}
func (s *remoteAllianceStore) Update(ctx context.Context, key string, mutate versionstore.Mutate[state]) (versionstore.Versioned[state], bool, error) {
	for range 32 {
		cur, found, e := s.Get(ctx, key)
		if e != nil {
			return cur, false, e
		}
		next, save, e := mutate(cur.Value, found)
		if e != nil || !save {
			return cur, false, e
		}
		if next.Key.String() != key {
			return cur, false, errRefused
		}
		record := allianceRecord{cur.Version + 1, next}
		raw, e := json.Marshal(record)
		if e != nil {
			return cur, false, e
		}
		_, e = sender.NewAllianceAuthoritySender(s.owner.nest).Sync_AllianceRemoteCAS(ctx, s.owner.id, cur.Version, string(raw))
		if errors.Is(e, versionstore.ErrVersionMismatch) {
			continue
		}
		if e != nil {
			return cur, false, e
		}
		return versionstore.Versioned[state]{Value: next, Version: record.Version}, true, nil
	}
	return versionstore.Versioned[state]{}, false, versionstore.ErrConflict
}
func (s *remoteAllianceStore) Create(ctx context.Context, key string, v state) (versionstore.Versioned[state], bool, error) {
	out, applied, e := s.Update(ctx, key, func(old state, found bool) (state, bool, error) {
		if found {
			return old, false, nil
		}
		return v, true, nil
	})
	if !applied {
		return versionstore.Versioned[state]{}, false, e
	}
	return out, true, e
}
