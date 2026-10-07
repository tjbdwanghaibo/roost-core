package skill

import "fmt"

// Runtime 侧的 Host 能力核对（B3 ③）：Program 带着编译期按环境能力表核对过的需求
// （hostRequirements），第一次在这个 Runtime 上启动 / 注册 / 入队被动 / 从 checkpoint 恢复时，
// 核对它们都在 Host 声明的表里（Host.HostCapabilities）。以前缺的能力要到施法中途才暴露：
// 召唤物在扣费之后才发现 Host 没有 OwnedEntityRuntimeHost（ErrHostContractViolation），属性 /
// 资源读到表外的 key 被 Host 静默当成 0。
//
// 通过的 Program 至多缓存 1024 个；容量满后仍核对，只是不持有新 Program，避免临时编译
// 程序随热更新无界保留。Host 的表在生命周期内不变。能力表是 Host 接口的一部分，每个 Host 都声明。
// 以前没实现 HostCapabilityProvider
// 的 Host 直接跳过核对，包装型调试 Host 因此绕过准入直接施法（B3 ③ 收尾，
// docs/feature/B3-3-HOST-CAPABILITY-TABLE-2026-10-07.md §12）。
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
