package skill

import "fmt"

// admitHostCapabilitiesLocked 是 Runtime 侧的 Host 能力核对：Program 带着编译期按环境能力表核对过的需求
// （hostRequirements），第一次在这个 Runtime 上启动 / 注册 / 入队被动 / 从 checkpoint 恢复时，核对它们都在 Host
// 声明的表里（Host.HostCapabilities）。缺的能力在准入处暴露，而不是到施法中途：召唤物不会在扣费之后才发现 Host
// 没有 OwnedEntityRuntimeHost，属性 / 资源也不会读到表外的 key 被 Host 静默当成 0。
//
// 通过的 Program 至多缓存 1024 个；容量满后仍核对，只是不持有新 Program，避免临时编译程序随热更新无界保留。
// Host 的表在生命周期内不变。能力表是 Host 接口的一部分，每个 Host（包括包装型调试 Host）都声明，没有可以跳过
// 核对的 Host。
func (runtime *Runtime) admitHostCapabilitiesLocked(program *Program) error {
	if runtime.host == nil {
		return ErrProgramInvariant
	}
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
	if len(runtime.hostAdmitted) < 1024 {
		runtime.hostAdmitted[program] = struct{}{}
	}
	return nil
}

func hostCoversProgram(host Host, program *Program) error {
	if host == nil {
		return ErrProgramInvariant
	}
	if program == nil || len(program.hostRequirements) == 0 {
		return nil
	}
	if missing := host.HostCapabilities().Missing(program.hostRequirements); len(missing) > 0 {
		return fmt.Errorf("%w: program %s needs %s", ErrHostCapabilityMissing, program.id, joinHostCapabilities(missing))
	}
	return nil
}
