package account

import (
	"context"
	"fmt"
	"slices"
)

// 区服清单：account 能回答“现在有哪些区服”。
//
// 区服记录按服号一条一条存（键值存储，没有扫描），所以另有一条**索引记录**记着登记过的服号。索引记录放在
// Servers 存储的保留键 0 上（GameServer.Listed）：服号是正数，0 号永远不是一个服，GetServer / UpsertServer /
// 合服都拒绝非正的服号，这条记录不会被当成区服交出去。放在同一个存储里而不是另开一个，是因为这样内存存储
// 与 Redis 存储走的是同一段代码，也不存在“接线时忘了把索引存储交进来、索引悄悄写进进程内存”的装配错误。
//
// 一致性——索引与记录是两次写，不是原子的，顺序是**先索引、后记录**：
//
//   - 停在两次写之间：索引里多出一个没有记录的服号。ListServers 读记录时发现没有就跳过，对外看不出来；
//     下一次 UpsertServer 同一个服号把记录补上。反过来（先记录后索引）停下的话，记录在而清单里没有它，
//     只有运维再登记一次才看得见——宁可索引多，不让清单少。
//   - 索引只增不减：没有删除区服的接口（关服是状态 closed，合服是别名，记录都留着）。
//   - 写索引是一次版本比较（versionstore.Update）：两个服号同时第一次登记，各自重读再写，都进得去；
//     服号已经在索引里时不写，所以改名、改状态不碰索引、互不竞争。
//   - 这一版之前登记的区服没有索引：ListServers 列不出它们，直到对它再执行一次 UpsertServer（accountctl
//     upsert-server）或它参与一次 MergeServer——两者都会补登记。没有上线过，不做自动迁移。
//
// MaxServers 是索引的容量。到了上限，登记**新的**服号被拒绝（ErrRangeInvalid），已登记的照常更新；所以
// ListServers 给出的永远是全部，不会悄悄截断。上限不是配置：清单是一次读全部（1 次索引 + 每个服 1 次记录），
// 区服比这多的部署该换成分页的目录，而不是把这个数调大。
const MaxServers = 256

// serverIndexKey 是索引记录在 Servers 存储里的键。
const serverIndexKey int32 = 0

// ListServers 返回登记过的全部区服记录（含合服字段），按服号升序，至多 MaxServers 条。只读。
//
// 区服记录是登录前就公开的列表信息，与 GetServer 一样不需要调用方身份。它在总线上：登录入口、
// 清单服务（manifest）用它知道“有哪些服”，再配上各自的连接地址与推荐规则。
//
// 索引里有而记录不存在的服号跳过（见文件头）；索引记录本身被改坏时（重复、乱序、非正的服号）
// 按“去重、排序、丢掉非正数”读，不因此失败。存储出错时返回错误，不返回半份清单。
func (s *Service) ListServers(ctx context.Context) ([]GameServer, error) {
	index, _, err := s.cfg.Servers.Get(ctx, serverIndexKey)
	if err != nil {
		return nil, err
	}
	ids := listedServerIDs(index.Value.Listed)
	servers := make([]GameServer, 0, len(ids))
	for _, id := range ids {
		record, found, err := s.cfg.Servers.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if !found || record.Value.ID != id {
			continue
		}
		servers = append(servers, record.Value.clone())
	}
	return servers, nil
}

// listedServerIDs 把索引记录里的服号整理成升序、不重复、全为正数的列表，至多 MaxServers 个。
func listedServerIDs(listed []int32) []int32 {
	ids := make([]int32, 0, len(listed))
	for _, id := range listed {
		if id > 0 {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) > MaxServers {
		ids = ids[:MaxServers]
	}
	return ids
}

// ensureServerListed 保证 id 在索引里；已经在就什么也不写。索引满了而 id 是新的，答 ErrRangeInvalid。
// 调用方在写区服记录之前调它（先索引、后记录）。
func (s *Service) ensureServerListed(ctx context.Context, id int32) error {
	_, _, err := s.cfg.Servers.Update(ctx, serverIndexKey, func(current GameServer, _ bool) (GameServer, bool, error) {
		ids := listedServerIDs(current.Listed)
		if _, listed := slices.BinarySearch(ids, id); listed {
			return current, false, nil
		}
		if len(ids) >= MaxServers {
			return current, false, fmt.Errorf("%w: %d game servers are registered, which is the limit; server %d was not added", ErrRangeInvalid, MaxServers, id)
		}
		// 索引记录只有 Listed 一个字段有意义；其余字段保持零值。
		return GameServer{Listed: slices.Insert(ids, insertionPoint(ids, id), id)}, true, nil
	})
	return err
}

func insertionPoint(sorted []int32, id int32) int {
	at, _ := slices.BinarySearch(sorted, id)
	return at
}
