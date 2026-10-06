package skill

import "fmt"

// Runtime 侧的 Host 能力核对（B3 ③）：Program 带着编译期按环境能力表核对过的需求
// （hostRequirements），第一次在这个 Runtime 上启动 / 注册 / 入队被动 / 从 checkpoint 恢复时，
// 核对它们都在 Host 声明的表里（Host.HostCapabilities）。以前缺的能力要到施法中途才暴露：
// 召唤物在扣费之后才发现 Host 没有 OwnedEntityRuntimeHost（ErrHostContractViolation），属性 /
// 资源读到表外的 key 被 Host 静默当成 0。
//
// 通过的 Program 记在 hostAdmitted 里，之后同一个 Program 不再重复核对（Host 的表在生命周期内
// 不变）。能力表是 Host 接口的一部分，每个 Host 都声明（没有 Host 的 Runtime 在 Start /
// RegisterAbility / RestoreRuntime 入口先被拒，到不了这里）。以前没实现 HostCapabilityProvider
// 的 Host 直接跳过核对，包装型调试 Host 因此绕过准入直接施法（B3 ③ 收尾，
// docs/feature/B3-3-HOST-CAPABILITY-TABLE-2026-10-07.md §12）。
func (runtime *Runtime) admitHostCapabilitiesLocked(program *Program) error {
	if program == nil || len(program.hostRequirements) == 0 {
		return nil
	}
	if _, admitted := runtime.hostAdmitted[program]; admitted {
		return nil
	}
	if err := hostCoversProgram(runtime.host, program); err != nil {
		return err
	}
	if runtime.hostAdmitted == nil {
		runtime.hostAdmitted = make(map[*Program]struct{})
	}
	runtime.hostAdmitted[program] = struct{}{}
	return nil
}

func hostCoversProgram(host Host, program *Program) error {
	if program == nil || len(program.hostRequirements) == 0 {
		return nil
	}
	if missing := host.HostCapabilities().Missing(program.hostRequirements); len(missing) > 0 {
		return fmt.Errorf("%w: program %s needs %s", ErrHostCapabilityMissing, program.id, joinHostCapabilities(missing))
	}
	return nil
}

// hostCheckedResolver 让 checkpoint 恢复时解析出的每个 Program 也经过同一项核对。
type hostCheckedResolver struct {
	resolver ProgramResolver
	host     Host
}

func (resolver hostCheckedResolver) ResolveProgram(id, gameplayDigest string) (*Program, error) {
	program, err := resolver.resolver.ResolveProgram(id, gameplayDigest)
	if err != nil || program == nil {
		return program, err
	}
	if err := hostCoversProgram(resolver.host, program); err != nil {
		return nil, err
	}
	return program, nil
}
