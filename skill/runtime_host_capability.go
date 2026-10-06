package skill

import "fmt"

// Runtime 侧的 Host 能力核对（B3 ③）：Program 带着编译期按环境能力表核对过的需求
// （hostRequirements），第一次在这个 Runtime 上启动 / 注册 / 入队被动 / 从 checkpoint 恢复时，
// 核对它们都在 Host 声明的表里（HostCapabilityProvider）。以前缺的能力要到施法中途才暴露：
// 召唤物在扣费之后才发现 Host 没有 OwnedEntityRuntimeHost（ErrHostContractViolation），属性 /
// 资源读到表外的 key 被 Host 静默当成 0。
//
// 通过的 Program 记在 hostAdmitted 里，之后同一个 Program 不再重复核对（Host 的表在生命周期内
// 不变）；没实现 HostCapabilityProvider 的 Host 不做核对。
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
	provider, ok := host.(HostCapabilityProvider)
	if !ok || program == nil {
		return nil
	}
	if missing := provider.HostCapabilities().Missing(program.hostRequirements); len(missing) > 0 {
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
