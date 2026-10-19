package roost

import (
	"fmt"
	"slices"
	"strings"
)

// 端口块（维护者 2026-10-19 决定）：开发配置集的服务在同一台机器上一起运行（deploy/dev/run.sh），每个服务要有自己的
// ops 端口与 etcd.advertise_addr。端口不再按服务名排序现算——那样 add 一个名字排在中间的服务，排在它后面的服务
// 序号全部后移，run.sh 与配置文件对不上、新服务拿走别人的端口。每个服务在 roost.yaml 里有一个块号
// （services.<name>.port_block），由生成器分配、一经分配不再改变：
//
//   - 新建工程：按“业务服务在前、托管框架服务在后，各自按名字排序”依次分配 0、1、2……（与改用块号之前的端口相同）；
//   - 之后新增的服务（add service、模板或命令带进来的托管服务、手改 roost.yaml 后 project sync）：当前最大块号 + 1，
//     几个同时新增时按上面的顺序；
//   - 删除服务不回收：只有删掉的恰好是最大块号时，下一个新服务会拿到同一个号（它的旧配置文件也应一并删掉）。
//
// 按服务区分的端口只从块号派生（portsOf），规则只在这里：
//
//	ops.addr                    9100 + N   （9100–9199）
//	etcd.advertise_addr         9000 + N   （9000–9099）
//	second-game.sh 的 ops       9200 + N   （9200–9299，第二个 game 进程）
//	second-game.sh 的 advertise 8900 + N   （8900–8999）
//
// 块号上限 99，四段端口互不重叠；改用块号之前 second-game 用 +10，第 11 个服务的端口就会撞上。玩家 TCP（7000，第二个
// game 7001）与 web（8080）只有一个服务持有，不按服务派生。生产示例每个服务一个容器，统一 9100 / CHANGE_ME:9000。
const maxPortBlock = 99

// servicePorts 是一个服务在开发机上的按服务端口。
type servicePorts struct {
	Block                      int
	Ops, Advertise             int
	SecondOps, SecondAdvertise int
}

func portsFor(block int) servicePorts {
	return servicePorts{Block: block, Ops: 9100 + block, Advertise: 9000 + block, SecondOps: 9200 + block, SecondAdvertise: 8900 + block}
}

// portsOf 是 service 的端口：块号取 roost.yaml 记录的值；内存里新加、还没写进 roost.yaml 的服务按 assignPortBlocks 的
// 同一规则算（写 roost.yaml 时 Marshal 记下同一个值）。
func portsOf(m Manifest, service string) servicePorts {
	return portsFor(m.portBlocks()[service])
}

// portBlocks 是全部服务的块号：已记录的原样，没记录的按分配规则补上。
func (m Manifest) portBlocks() map[string]int {
	blocks := make(map[string]int, len(m.Services))
	next := 0
	for name, spec := range m.Services {
		if spec.PortBlock != nil {
			blocks[name] = *spec.PortBlock
			next = max(next, *spec.PortBlock+1)
		}
	}
	for _, name := range portBlockOrder(m) {
		if _, recorded := blocks[name]; !recorded {
			blocks[name] = next
			next++
		}
	}
	return blocks
}

// portBlockOrder 是分配顺序：业务服务在前、托管框架服务在后，各自按名字排序。
func portBlockOrder(m Manifest) []string {
	var business, hosted []string
	for _, name := range sortedServiceNames(m) {
		if m.isFrameworkService(name) {
			hosted = append(hosted, name)
		} else {
			business = append(business, name)
		}
	}
	return append(business, hosted...)
}

// withPortBlocks 返回给每个服务记下块号的副本（Services 另拷一份，不改调用方的 map）。生成器写 roost.yaml 前
// （Marshal）与 project sync 读到缺块号的服务时调用。
func (m Manifest) withPortBlocks() Manifest {
	blocks := m.portBlocks()
	services := make(map[string]ServiceSpec, len(m.Services))
	for name, spec := range m.Services {
		if spec.PortBlock == nil {
			block := blocks[name]
			spec.PortBlock = &block
		}
		services[name] = spec
	}
	m.Services = services
	return m
}

// missingPortBlocks 是 roost.yaml 里没写块号的服务（按分配顺序）。
func (m Manifest) missingPortBlocks() []string {
	var missing []string
	for _, name := range portBlockOrder(m) {
		if m.Services[name].PortBlock == nil {
			missing = append(missing, name)
		}
	}
	return missing
}

// validatePortBlocks 检查记录的块号：0～maxPortBlock、互不相同。没记录的不在这里报（内存里新加的服务由 Marshal 补上）；
// 从 roost.yaml 读到缺块号的服务由 requirePortBlocks 报。
func (m Manifest) validatePortBlocks() error {
	owners := map[int]string{}
	var problems []string
	for _, name := range sortedServiceNames(m) {
		block := m.Services[name].PortBlock
		if block == nil {
			continue
		}
		if *block < 0 || *block > maxPortBlock {
			problems = append(problems, fmt.Sprintf("services.%s.port_block %d is outside 0-%d", name, *block, maxPortBlock))
			continue
		}
		if other, taken := owners[*block]; taken {
			problems = append(problems, fmt.Sprintf("services.%s.port_block %d is also services.%s's; each service needs its own block (its development ops port 9100+N and etcd advertise port 9000+N)", name, *block, other))
			continue
		}
		owners[*block] = name
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// requirePortBlocks：roost.yaml 是生成器写的，每个服务都带块号；缺了（手改 roost.yaml 加了服务）就报错并给出改法。
func (m Manifest) requirePortBlocks() error {
	missing := m.missingPortBlocks()
	if len(missing) == 0 {
		return nil
	}
	blocks := m.portBlocks()
	var fixes []string
	for _, name := range missing {
		fixes = append(fixes, fmt.Sprintf("services.%s.port_block: %d", name, blocks[name]))
	}
	return fmt.Errorf("%s has no port_block for service(s) %s (each service's development ports derive from its block, which never changes once assigned): run roost project sync to assign the next free block, or write %s",
		ManifestName, strings.Join(missing, ", "), strings.Join(fixes, ", "))
}

// loadManifestAssigningPortBlocks 读 roost.yaml 并给没写块号的服务分配块号（只在内存里；调用方负责写回）。
// assigned 是新分配的服务。
func loadManifestAssigningPortBlocks(root string) (m Manifest, assigned []string, err error) {
	m, err = decodeManifest(root)
	if err != nil {
		return Manifest{}, nil, err
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, nil, err
	}
	assigned = m.missingPortBlocks()
	m = m.withPortBlocks()
	if err := m.Validate(); err != nil {
		return Manifest{}, nil, err
	}
	return m, assigned, nil
}

// sortedPortBlocks 按块号列出 service → 块号与端口：doctor config-across-services 通过时的说明里给出全部分配，
// 不另设列出分配的命令。
func sortedPortBlocks(m Manifest) []string {
	blocks := m.portBlocks()
	names := portBlockOrder(m)
	slices.SortStableFunc(names, func(a, b string) int { return blocks[a] - blocks[b] })
	var out []string
	for _, name := range names {
		p := portsFor(blocks[name])
		out = append(out, fmt.Sprintf("%s=%d (ops %d, advertise %d)", name, p.Block, p.Ops, p.Advertise))
	}
	return out
}
