package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/clock"
	"github.com/tjbdwanghaibo/roost-core/lifecycle"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
	flog "github.com/tjbdwanghaibo/roost-core/log"
	"github.com/tjbdwanghaibo/roost-core/nest"
)

// App is the top-level application container.
// It wires Mods and Services together with CLI support.
type App struct {
	name    string
	version string
	rootCmd *cobra.Command

	mods     []Mod
	services map[ServiceName]*serviceEntry

	// runtime
	registry *Registry
	cfg      *viper.Viper

	// signalSource exists to make shutdown sequencing testable on platforms
	// where a process cannot deliver os.Interrupt to itself (notably Windows).
	// Production leaves it nil and uses the process signal notifier below.
	signalSource func() (<-chan os.Signal, func())

	singletonOpener SingletonOpener
	singletonClock  singletonClock
	singleton       *singletonLock
}

// serviceEntry holds a service and its specific mods.
type serviceEntry struct {
	svc  Service
	mods []Mod
}

func New(name, version string) *App {
	a := &App{
		name:     name,
		version:  version,
		services: make(map[ServiceName]*serviceEntry),
		cfg:      viper.New(),
	}
	a.rootCmd = &cobra.Command{
		Use:     name,
		Version: version,
		Short:   fmt.Sprintf("%s game server", name),
	}
	return a
}

// Mods registers infrastructure modules (order matters for init).
func (a *App) Mods(mods ...Mod) *App {
	a.mods = append(a.mods, mods...)
	return a
}

// RegisterServer registers a service as a CLI subcommand.
// Optional mods are service-specific and only start when this service runs.
func (a *App) RegisterServer(serverType ServiceName, svc Service, mods ...Mod) *App {
	a.services[serverType] = &serviceEntry{svc: svc, mods: mods}
	st := string(serverType)
	cmd := &cobra.Command{
		Use:   st,
		Short: fmt.Sprintf("Start %s server", st),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.run(serverType)
		},
	}
	a.rootCmd.AddCommand(cmd)
	return a
}

// Execute parses CLI args and runs the appropriate subcommand.
func (a *App) Execute() error {
	a.rootCmd.PersistentFlags().StringP("config", "c", "", "config file path; empty uses configs/service/config.<service>.yaml")
	a.rootCmd.PersistentFlags().Int("sid", 1000, "server id")
	return a.rootCmd.Execute()
}

// RootCmd returns the root cobra command for further customization.
func (a *App) RootCmd() *cobra.Command {
	return a.rootCmd
}

func (a *App) run(serverType ServiceName) error {
	// --- Load config ---
	cfgPath, _ := a.rootCmd.Flags().GetString("config")
	explicitConfig := a.rootCmd.PersistentFlags().Changed("config")
	if cfgPath == "" {
		cfgPath = defaultServiceConfigPath(serverType)
	}
	sid, _ := a.rootCmd.Flags().GetInt("sid")

	a.cfg.SetConfigFile(cfgPath)
	a.cfg.SetDefault("sid", sid)
	a.cfg.SetDefault("server_type", serverType)
	a.cfg.SetDefault("log.level", "info")
	a.cfg.SetDefault("log.json", false)
	a.cfg.SetDefault("log.stdout", true)
	a.cfg.SetDefault("log.file", true)
	a.cfg.SetDefault("log.dir", "log")
	a.cfg.SetDefault("log.caller", false)
	a.cfg.SetDefault("log.rotate_interval", "24h")
	a.cfg.SetDefault("player_protocol.rate_limit.enabled", false)

	if err := a.cfg.ReadInConfig(); err != nil {
		if explicitConfig || !isMissingConfig(err) {
			return fmt.Errorf("read config %q: %w", cfgPath, err)
		}
		slog.Warn("default config file not found, using development defaults", "path", cfgPath, "err", err)
	}
	a.cfg.Set("server_type", serverType)
	if a.rootCmd.Flags().Changed("sid") {
		a.cfg.Set("sid", sid)
	}
	if err := ValidateServiceConfig(a.cfg); err != nil {
		return err
	}
	clock.SetOffset(a.cfg.GetDuration("time.logic_offset"))
	fctx.SetRuntimeConfig(a.cfg)
	if err := flog.Init(flog.Options{
		LevelText:        a.cfg.GetString("log.level"),
		JSON:             a.cfg.GetBool("log.json"),
		Stdout:           a.cfg.GetBool("log.stdout"),
		File:             a.cfg.GetBool("log.file"),
		Dir:              a.cfg.GetString("log.dir"),
		Service:          string(serverType),
		Sid:              a.cfg.GetInt("sid"),
		Caller:           a.cfg.GetBool("log.caller"),
		RotateInterval:   a.cfg.GetDuration("log.rotate_interval"),
		RotateTimeFormat: a.cfg.GetString("log.rotate_time_format"),
		FrameFunc:        nest.CurTick,
	}); err != nil {
		return fmt.Errorf("init log: %w", err)
	}
	// This App run owns the configured sink. Close it before returning so a
	// subsequent run can safely rotate or remove the same file on Windows.
	defer func() { _ = flog.Close() }()

	slog.Info("starting server",
		"name", a.name,
		"version", a.version,
		"type", serverType,
		"sid", a.cfg.GetInt("sid"),
		"config", cfgPath,
	)

	// --- Init registry ---
	a.registry = NewRegistry(a.cfg)
	if err := a.emitLifecycle(context.Background(), lifecycle.Event{
		Phase:   lifecycle.PhaseAppInit,
		Service: string(serverType),
		Name:    a.name,
		Data: map[string]any{
			"sid":    a.cfg.GetInt("sid"),
			"config": cfgPath,
		},
	}); err != nil {
		return err
	}
	runtimeFailure, _ := Lookup[*RuntimeFailure](a.registry, ModRuntimeFailure)
	// startupFailure 在启动各阶段之间检查：启动期间（例如 DataEngine 重放很长时）发生的 fail-stop
	// （失锁、DataEngine / Remote fatal）不等进入 Serve 的 select，按启动失败路径停掉已启动的 Mod。
	startupFailure := func() error {
		if err := runtimeFailure.Err(); err != nil {
			return fmt.Errorf("app: runtime failure during startup: %w", err)
		}
		return nil
	}

	// --- Singleton lock: before any Mod Init ---
	// 同一服务类型 + sid 只让一个进程跑 Mod（docs/feature/APP-SINGLETON-LOCK-2026-10-05.md）。
	// 收尾统一在 defer 里：先停续期，再按 singletonReleasable 决定是否 Release，最后关闭 store。
	// singletonReleasable 只在“全部 Mod 都停完”的路径上置位（逐个返回点置位，漏掉的路径默认不释放，
	// 键在 TTL 内自然过期，方向是安全的）；singletonReleaseDeadline 是停机路径 shutdownCtx 的截止时间。
	singleton, err := a.openSingleton(serverType)
	if err != nil {
		return err
	}
	var singletonReleasable bool
	var singletonReleaseDeadline time.Time
	if singleton != nil {
		defer func() { singleton.finish(singletonReleasable, singletonReleaseDeadline) }()
		var waitSignals <-chan os.Signal
		if a.signalSource != nil {
			signals, stop := a.signalSource()
			waitSignals = signals
			defer stop()
		}
		slog.Info("singleton: acquiring", "key", singleton.key)
		if err := singleton.acquire(waitSignals); err != nil {
			if errors.Is(err, errSingletonWaitInterrupted) {
				return nil
			}
			return err
		}
		slog.Info("singleton: acquired", "key", singleton.key, "value", string(singleton.value))
		singleton.startRenewal()
	}

	sharedMods, err := sortMods(a.mods, nil)
	if err != nil {
		singletonReleasable = true
		return err
	}
	var startedSharedMods []Mod
	var startedServiceMods []Mod
	var providedSharedMods []Mod
	var providedServiceMods []Mod

	// --- Mods lifecycle: Init → Provide → Start ---
	for _, mod := range sharedMods {
		slog.Info("mod init", "mod", mod.Name())
		if err := mod.Init(a.cfg); err != nil {
			singletonReleasable = true
			return fmt.Errorf("mod %s init: %w", mod.Name(), err)
		}
	}
	for _, mod := range sharedMods {
		if err := mod.Provide(a.registry); err != nil {
			singletonReleasable = stopModsReverse(append(providedSharedMods, mod), "mod stop after provide error")
			return fmt.Errorf("mod %s provide: %w", mod.Name(), err)
		}
		providedSharedMods = append(providedSharedMods, mod)
	}
	for _, mod := range sharedMods {
		if err := startupFailure(); err != nil {
			singletonReleasable = stopModsReverse(providedSharedMods, "mod stop")
			return err
		}
		slog.Info("mod start", "mod", mod.Name())
		if err := mod.Start(); err != nil {
			singletonReleasable = stopModsReverse(providedSharedMods, "mod stop")
			return fmt.Errorf("mod %s start: %w", mod.Name(), err)
		}
		startedSharedMods = append(startedSharedMods, mod)
	}

	// --- Service entry ---
	entry, ok := a.services[serverType]
	if !ok {
		singletonReleasable = stopModsReverse(startedSharedMods, "mod stop")
		return fmt.Errorf("unknown server type: %s", serverType)
	}
	sharedNames := make(map[ModName]struct{}, len(sharedMods))
	for _, mod := range sharedMods {
		sharedNames[mod.Name()] = struct{}{}
	}
	serviceMods, err := sortMods(entry.mods, sharedNames)
	if err != nil {
		singletonReleasable = stopModsReverse(startedSharedMods, "mod stop")
		return err
	}

	// --- Service-specific Mods lifecycle: Init → Provide → Start ---
	for _, mod := range serviceMods {
		slog.Info("mod init (service-specific)", "mod", mod.Name())
		if err := mod.Init(a.cfg); err != nil {
			singletonReleasable = stopModsReverse(startedSharedMods, "mod stop")
			return fmt.Errorf("mod %s init: %w", mod.Name(), err)
		}
	}
	for _, mod := range serviceMods {
		if err := mod.Provide(a.registry); err != nil {
			singletonReleasable = allStopped(
				stopModsReverse(append(providedServiceMods, mod), "mod stop after provide error (service-specific)"),
				stopModsReverse(startedSharedMods, "mod stop"))
			return fmt.Errorf("mod %s provide: %w", mod.Name(), err)
		}
		providedServiceMods = append(providedServiceMods, mod)
	}
	for _, mod := range serviceMods {
		if err := startupFailure(); err != nil {
			singletonReleasable = allStopped(
				stopModsReverse(providedServiceMods, "mod stop (service-specific)"),
				stopModsReverse(startedSharedMods, "mod stop"))
			return err
		}
		slog.Info("mod start (service-specific)", "mod", mod.Name())
		if err := mod.Start(); err != nil {
			singletonReleasable = allStopped(
				stopModsReverse(providedServiceMods, "mod stop (service-specific)"),
				stopModsReverse(startedSharedMods, "mod stop"))
			return fmt.Errorf("mod %s start: %w", mod.Name(), err)
		}
		startedServiceMods = append(startedServiceMods, mod)
	}
	if err := startupFailure(); err != nil {
		singletonReleasable = allStopped(
			stopModsReverse(startedServiceMods, "mod stop (service-specific)"),
			stopModsReverse(startedSharedMods, "mod stop"))
		return err
	}
	if err := a.emitLifecycle(context.Background(), lifecycle.Event{
		Phase:   lifecycle.PhaseModsStarted,
		Service: string(serverType),
		Name:    a.name,
	}); err != nil {
		singletonReleasable = allStopped(
			stopModsReverse(startedServiceMods, "mod stop (service-specific)"),
			stopModsReverse(startedSharedMods, "mod stop"))
		return err
	}

	// --- Service lifecycle: Init → Serve ---
	svc := entry.svc
	slog.Info("service init", "service", svc.Name())
	// Init 失败与 Init 之后的启动失败走同一条收尾：先收回 Service 已启动的部分，再停 Mod、释放锁；
	// Service 在预算内停不下来就保留 Mod 与锁，与正常停机相同（RR-20261005-NC-193）。
	startErr := svc.Init(a.registry)
	if startErr != nil {
		startErr = fmt.Errorf("service %s init: %w", svc.Name(), startErr)
	} else {
		startErr = startupFailure()
	}
	if startErr == nil {
		startErr = a.emitLifecycle(context.Background(), lifecycle.Event{
			Phase:   lifecycle.PhaseServiceStarted,
			Service: string(serverType),
			Name:    string(svc.Name()),
		})
	}
	if startErr != nil {
		stopped, cleanupErr := shutdownAfterStartupFailure(svc)
		if cleanupErr != nil {
			startErr = errors.Join(startErr, cleanupErr)
		}
		if !stopped {
			// Service 的组件可能还在用 Mod：不停 Mod、不释放锁（singletonReleasable 保持 false），
			// 进程退出后键在 TTL 内过期。
			return startErr
		}
		singletonReleasable = allStopped(
			stopModsReverse(startedServiceMods, "mod stop (service-specific)"),
			stopModsReverse(startedSharedMods, "mod stop"))
		return startErr
	}

	// Register signal handling before Serve can expose readiness and receive
	// external termination.
	sigChan, stopSignals := a.exitSignals()
	defer stopSignals()

	// Serve in background, wait for signal.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- svc.Serve(ctx)
	}()

	// Wait for signal, fail-stop infrastructure, or service exit.
	var serviceErr error
	serveDone := false
	select {
	case sig := <-sigChan:
		slog.Info("received signal, shutting down", "signal", sig)
	case err := <-runtimeFailure.Done():
		slog.Error("runtime infrastructure failure, shutting down", "err", err)
		serviceErr = errors.Join(serviceErr, err)
	case err := <-serveErr:
		serveDone = true
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("service exited with error", "err", err)
			serviceErr = err
		}
	}
	cancel()

	// --- Graceful shutdown ---
	shutdownTimeout := a.cfg.GetDuration("shutdown.total_timeout")
	if shutdownTimeout <= 0 {
		shutdownTimeout = 30 * time.Second
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()
	// 启用单实例锁时，Mod 停机用的截止时间提前 releaseReserve，留给全部 Mod 停完之后的 Release：
	// stopModsReverseBefore 会把截止前的剩余时间全部分给 Mod。预留不超过总时长的一半，避免很小的
	// total_timeout 被预留吃光（codegen 会把 Release 的 3s 计入 total_timeout）。此刻已 Lost
	// （失锁触发的 fail-stop）就不会 Release，不预留，整段时间留给 Mod 停机；停机期间才失锁的，
	// 预留照旧，只是用不上。
	modStopCtx := shutdownCtx
	if singleton != nil {
		deadline, _ := shutdownCtx.Deadline()
		singletonReleaseDeadline = deadline
		releaseReserve := min(singletonReleaseBudget, shutdownTimeout/2)
		if singleton.snapshot().state == singletonLost {
			releaseReserve = 0
		}
		var modStopCancel context.CancelFunc
		modStopCtx, modStopCancel = context.WithDeadline(shutdownCtx, deadline.Add(-releaseReserve))
		defer modStopCancel()
	}

	slog.Info("service shutdown", "service", svc.Name())
	var shutdownErr error
	if err := a.emitLifecycle(shutdownCtx, lifecycle.Event{
		Phase:   lifecycle.PhaseServiceStopping,
		Service: string(serverType),
		Name:    string(svc.Name()),
	}); err != nil {
		shutdownErr = errors.Join(shutdownErr, err)
	}
	shutdownResult := make(chan error, 1)
	go func() { shutdownResult <- svc.Shutdown(shutdownCtx) }()
	shutdownDone := false
	for !serveDone || !shutdownDone {
		select {
		case err := <-serveErr:
			serveDone = true
			if err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("service exited with error during shutdown", "err", err)
				serviceErr = errors.Join(serviceErr, err)
			}
		case err := <-shutdownResult:
			shutdownDone = true
			if err != nil {
				slog.Error("service shutdown error", "err", err)
				shutdownErr = errors.Join(shutdownErr, fmt.Errorf("service %s shutdown: %w", svc.Name(), err))
			}
		case <-shutdownCtx.Done():
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("service %s shutdown incomplete: %w", svc.Name(), shutdownCtx.Err()))
			// Dependencies must remain alive while Serve or Shutdown can still
			// access them. Returning without stopping mods is safer than racing
			// an uncooperative service during process termination.
			// 单实例锁同理不释放（singletonReleasable 保持 false）。
			return errors.Join(serviceErr, shutdownErr)
		}
	}

	// Stop service-specific mods in reverse order
	// 共享 Mod 在服务专属 Mod 之后停止，但共用同一个总时限：规划时把它们声明的预算算进去。
	if err := stopModsReverseBefore(modStopCtx, startedServiceMods, startedSharedMods, "mod stop (service-specific)"); err != nil {
		shutdownErr = errors.Join(shutdownErr, err)
		if stopIncomplete(err) {
			// A service-specific Mod may still use shared capabilities. Preserve
			// those dependencies until the process terminates.
			return errors.Join(serviceErr, shutdownErr)
		}
	}

	// Stop shared mods in reverse order
	if err := stopModsReverseWithContext(modStopCtx, startedSharedMods, "mod stop"); err != nil {
		shutdownErr = errors.Join(shutdownErr, err)
		singletonReleasable = !stopIncomplete(err)
	} else {
		singletonReleasable = true
	}

	slog.Info("server stopped", "type", serverType)
	if err := a.emitLifecycle(shutdownCtx, lifecycle.Event{
		Phase:   lifecycle.PhaseServiceStopped,
		Service: string(serverType),
		Name:    string(svc.Name()),
	}); err != nil {
		shutdownErr = errors.Join(shutdownErr, err)
	}
	return errors.Join(serviceErr, shutdownErr)
}

func (a *App) exitSignals() (<-chan os.Signal, func()) {
	if a.signalSource != nil {
		return a.signalSource()
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	return ch, func() { signal.Stop(ch) }
}

func defaultServiceConfigPath(serverType ServiceName) string {
	return fmt.Sprintf("configs/service/config.%s.yaml", serverType)
}

func isMissingConfig(err error) bool {
	if err == nil {
		return false
	}
	if os.IsNotExist(err) {
		return true
	}
	var notFound viper.ConfigFileNotFoundError
	return errors.As(err, &notFound)
}

// stopModsReverse 是启动失败路径的逆序停止（无总截止时间，每个 Mod 用自己的默认时长）。
// 返回是否全部停完：某个 Mod 停机超时会中断后续停止、保留它们的依赖，此时返回 false，
// 调用方据此不释放单实例锁。Mod 返回普通错误仍算停完。
// startupCleanupTimeout 是启动失败时留给 Service.Shutdown 收回已启动部分的时长。
const startupCleanupTimeout = 5 * time.Second

// shutdownAfterStartupFailure 在启动失败时调用 Service.Shutdown，收回 Init 已经启动的部分
// （Init 返回错误时同样调用：Init 可能已经起了后台循环、订阅，App 不知道是哪些）。Shutdown 必须
// 容忍部分初始化。stopped 为 false 表示 Shutdown 没在 startupCleanupTimeout 内结束（包括不配合
// ctx、按 ctx 超时返回、panic）：调用方保留 Mod 与单实例锁，和正常停机“Shutdown 不完整就保留
// 依赖”的规则一致；Shutdown 返回其他错误视为已结束，错误并入启动错误（RR-20261005-NC-193）。
func shutdownAfterStartupFailure(svc Service) (stopped bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), startupCleanupTimeout)
	defer cancel()
	type outcome struct {
		err      error
		panicked bool
	}
	result := make(chan outcome, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				result <- outcome{err: fmt.Errorf("panic: %v", recovered), panicked: true}
			}
		}()
		result <- outcome{err: svc.Shutdown(ctx)}
	}()
	select {
	case out := <-result:
		switch {
		case out.panicked:
			return false, fmt.Errorf("service %s cleanup after startup failure incomplete: %w", svc.Name(), out.err)
		case out.err == nil:
			return true, nil
		case stopIncomplete(out.err):
			return false, fmt.Errorf("service %s cleanup after startup failure incomplete: %w", svc.Name(), out.err)
		default:
			return true, fmt.Errorf("service %s cleanup after startup failure: %w", svc.Name(), out.err)
		}
	case <-ctx.Done():
		return false, fmt.Errorf("service %s cleanup after startup failure incomplete: %w", svc.Name(), ctx.Err())
	}
}

func stopModsReverse(mods []Mod, msg string) bool {
	err := stopModsReverseWithContext(context.Background(), mods, msg)
	if err != nil {
		slog.Error("mod stop failed", "err", err)
	}
	return !stopIncomplete(err)
}

// allStopped 合并几段 stopModsReverse 的结果；参数在调用前已全部求值，每段都会执行。
func allStopped(stopped ...bool) bool {
	for _, ok := range stopped {
		if !ok {
			return false
		}
	}
	return true
}

func stopModsReverseWithContext(ctx context.Context, mods []Mod, msg string) error {
	return stopModsReverseBefore(ctx, mods, nil, msg)
}

// stopModsReverseBefore 逆序停止 mods。later 是同一次停机里、这些 mods 停完之后才会停止
// 的 Mod（服务专属 Mod 先停、共享 Mod 后停，共用一个 shutdown.total_timeout），不在这里停止，
// 只把它们声明的预算计入规划：先停的一段必须给后面声明了预算的 Mod 留出时间
// （RR-20260926-42）。later 中未声明预算的 Mod 不参与这一段的均分，与原先相同。
func stopModsReverseBefore(ctx context.Context, mods []Mod, later []Mod, msg string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var joined error
	for i := len(mods) - 1; i >= 0; i-- {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.Join(joined, ctxErr)
		}
		mod := mods[i]
		if mod == nil {
			joined = errors.Join(joined, errors.New("app: nil mod during stop"))
			continue
		}
		modCtx, cancel, budget := modStopContext(ctx, mod, mods[:i], later)
		started := time.Now()
		slog.Info(msg, "mod", mod.Name(), "budget", budget)
		err := stopModSafely(modCtx, mod)
		cancel()
		duration := time.Since(started)
		if err != nil {
			slog.Error("mod stop failed", "mod", mod.Name(), "duration", duration, "err", err)
			joined = errors.Join(joined, fmt.Errorf("mod %s stop: %w", mod.Name(), err))
			if stopIncomplete(err) {
				break
			}
			continue
		}
		slog.Info("mod stopped", "mod", mod.Name(), "duration", duration)
	}
	return joined
}

func stopIncomplete(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// defaultModStopTimeout 是没有总截止时间（兼容 Stop 路径）时未声明预算的 Mod 的停机时长。
const defaultModStopTimeout = 5 * time.Second

// undeclaredModStopFloor 是有总截止时间时每个未声明预算的 Mod 的固定保底（RR-20260926-51）：
// 规划先为它们留出保底，声明预算只能用剩下的部分；保底不随声明预算一起缩放，先停的 Mod 因此
// 不会被一个很大的声明值压到接近零——那会让它立即超时并中断整条关闭链。取 3s，与生成配置的
// nest.request_timeout 同值（Nest 排空已准入请求的上限）。
const undeclaredModStopFloor = 3 * time.Second

// declaredStopBudget 返回 Mod 通过 ModStopBudgetProvider 声明的预算，未声明为 0。
func declaredStopBudget(mod Mod) time.Duration {
	provider, ok := mod.(ModStopBudgetProvider)
	if !ok {
		return 0
	}
	return max(provider.StopBudget(), 0)
}

// stopBudgetPlan 是某一时刻“剩余总时长”在尚未停止的 Mod 之间的分配结果。
type stopBudgetPlan int

const (
	// stopBudgetDeclaredGranted：声明的 Mod 拿到声明值，未声明的均分其余（每个不低于保底）。
	stopBudgetDeclaredGranted stopBudgetPlan = iota
	// stopBudgetDeclaredScaled：声明值之和超过“剩余 − 未声明保底之和”，声明值按比例缩放到这个上限，
	// 未声明的 Mod 恰好拿保底。
	stopBudgetDeclaredScaled
	// stopBudgetEvenSplit：剩余连每个 Mod 一份保底都给不起，所有 Mod（含声明的）均分剩余。
	stopBudgetEvenSplit
)

// modStopBudget 在剩余总时长 remaining 内为 mod 分配停机截止时长。pending 是同一段里在它
// 之后停止的 Mod；later 是之后另一段才停止的 Mod，只贡献声明预算（later 中未声明的 Mod 不参与
// 这一段的规划，与 RR-42 之前相同）。每停一个 Mod 重新规划一次，前面 Mod 提前结束省下的时间
// 自然留给后面。
//
// 规则（RR-20260926-51，维护者批准）：
//   - 保底之和 = 本段未声明 Mod 数 x undeclaredModStopFloor；声明预算的上限 = remaining − 保底之和。
//   - 声明值之和不超过上限：声明的 Mod 拿到声明值，未声明的 Mod 均分 remaining − 声明值之和。
//   - 超过上限：声明值按“上限 / 声明值之和”缩放，未声明的 Mod 各拿保底；保底不参与缩放。
//   - remaining 不足以给参与规划的每个 Mod 一份保底：全部均分 remaining（声明的 Mod 也一样）。
//
// 没有任何 Mod 声明预算时三种情形都退化为“remaining / 本段剩余 Mod 数”，与原先相同。
// 声明预算之和因此永远不超过剩余总时长。
func modStopBudget(remaining time.Duration, mod Mod, pending, later []Mod) (time.Duration, stopBudgetPlan) {
	own := declaredStopBudget(mod)
	declared, declaring, undeclared := own, 0, 0
	if own > 0 {
		declaring = 1
	} else {
		undeclared = 1
	}
	for _, next := range pending {
		if next == nil {
			continue
		}
		if b := declaredStopBudget(next); b > 0 {
			declared += b
			declaring++
		} else {
			undeclared++
		}
	}
	for _, next := range later {
		if next == nil {
			continue
		}
		if b := declaredStopBudget(next); b > 0 {
			declared += b
			declaring++
		}
	}
	participants := time.Duration(declaring + undeclared)
	if remaining < participants*undeclaredModStopFloor {
		return remaining / participants, stopBudgetEvenSplit
	}
	floors := time.Duration(undeclared) * undeclaredModStopFloor
	capacity := remaining - floors
	if declared <= capacity {
		if own > 0 {
			return own, stopBudgetDeclaredGranted
		}
		return (remaining - declared) / time.Duration(undeclared), stopBudgetDeclaredGranted
	}
	if own > 0 {
		return time.Duration(float64(own) * float64(capacity) / float64(declared)), stopBudgetDeclaredScaled
	}
	return undeclaredModStopFloor, stopBudgetDeclaredScaled
}

// modStopContext 给一个 Mod 的 StopWithContext 建立截止时间，并返回所给的时长（日志用）。
// 没有总截止时间（兼容 Stop 路径）时，声明的 Mod 用声明值，其余用 defaultModStopTimeout。
func modStopContext(parent context.Context, mod Mod, pending, later []Mod) (context.Context, context.CancelFunc, time.Duration) {
	if parent == nil {
		parent = context.Background()
	}
	deadline, ok := parent.Deadline()
	if !ok {
		budget := declaredStopBudget(mod)
		if budget == 0 {
			budget = defaultModStopTimeout
		}
		ctx, cancel := context.WithTimeout(parent, budget)
		return ctx, cancel, budget
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		ctx, cancel := context.WithCancel(parent)
		return ctx, cancel, 0
	}
	budget, plan := modStopBudget(remaining, mod, pending, later)
	declared := declaredStopBudget(mod)
	switch {
	case plan == stopBudgetEvenSplit:
		slog.Warn("mod stop budget: shutdown.total_timeout cannot cover the per-mod floor; splitting the remaining time evenly",
			"mod", mod.Name(), "declared", declared, "granted", budget, "remaining", remaining,
			"floor", undeclaredModStopFloor)
	case plan == stopBudgetDeclaredScaled && declared > 0:
		slog.Warn("mod stop budget scaled down: shutdown.total_timeout minus the other mods' floors cannot cover every declared budget",
			"mod", mod.Name(), "declared", declared, "granted", budget, "remaining", remaining,
			"floor", undeclaredModStopFloor)
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	return ctx, cancel, budget
}

func stopModSafely(ctx context.Context, mod Mod) error {
	result := make(chan error, 1)
	go func() {
		var err error
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("panic: %v", recovered)
			}
			result <- err
		}()
		if stopper, ok := mod.(ModStopperWithContext); ok {
			err = stopper.StopWithContext(ctx)
			return
		}
		mod.Stop()
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *App) emitLifecycle(ctx context.Context, event lifecycle.Event) error {
	if a == nil || a.registry == nil {
		return fmt.Errorf("app: lifecycle registry unavailable")
	}
	reg, ok := Lookup[*lifecycle.Registry](a.registry, ModLifecycle)
	if !ok || reg == nil {
		return fmt.Errorf("app: capability %q not found or wrong type", ModLifecycle)
	}
	if event.Phase == lifecycle.PhaseServiceStopping || event.Phase == lifecycle.PhaseServiceStopped {
		return reg.EmitAll(ctx, event)
	}
	return reg.Emit(ctx, event)
}

func sortMods(mods []Mod, external map[ModName]struct{}) ([]Mod, error) {
	// RR-20261003-NC-01：单 Mod 也必须校验名字、依赖和环，只有空集合可直接返回。
	if len(mods) == 0 {
		return append([]Mod(nil), mods...), nil
	}

	byName := make(map[ModName]Mod, len(mods))
	order := make(map[ModName]int, len(mods))
	for i, mod := range mods {
		if mod == nil {
			return nil, fmt.Errorf("mod entry %d is nil", i)
		}
		name := mod.Name()
		if name == "" {
			return nil, fmt.Errorf("mod entry %d has empty name", i)
		}
		if _, exists := byName[name]; exists {
			return nil, fmt.Errorf("duplicate mod %q", name)
		}
		byName[name] = mod
		order[name] = i
	}

	visiting := make(map[ModName]bool, len(mods))
	visited := make(map[ModName]bool, len(mods))
	out := make([]Mod, 0, len(mods))

	var visit func(ModName) error
	visit = func(name ModName) error {
		if visited[name] {
			return nil
		}
		if visiting[name] {
			return fmt.Errorf("mod dependency cycle at %q", name)
		}
		mod, ok := byName[name]
		if !ok {
			if _, ok := external[name]; ok {
				return nil
			}
			return fmt.Errorf("unknown mod dependency %q", name)
		}
		visiting[name] = true
		if depProvider, ok := mod.(ModDependencyProvider); ok {
			deps := append([]ModName(nil), depProvider.DependsOn()...)
			sort.SliceStable(deps, func(i, j int) bool {
				return order[deps[i]] < order[deps[j]]
			})
			for _, dep := range deps {
				if dep == "" {
					continue
				}
				if err := visit(dep); err != nil {
					return fmt.Errorf("mod %s depends on %s: %w", name, dep, err)
				}
			}
		}
		if depProvider, ok := mod.(ModOptionalDependencyProvider); ok {
			deps := append([]ModName(nil), depProvider.OptionalDependsOn()...)
			sort.SliceStable(deps, func(i, j int) bool {
				return order[deps[i]] < order[deps[j]]
			})
			for _, dep := range deps {
				if dep == "" {
					continue
				}
				if _, present := byName[dep]; !present {
					// A shared Mod is already initialized before service Mods;
					// an entirely absent optional Mod intentionally has no edge.
					continue
				}
				if err := visit(dep); err != nil {
					return fmt.Errorf("mod %s optionally depends on %s: %w", name, dep, err)
				}
			}
		}
		visiting[name] = false
		visited[name] = true
		out = append(out, mod)
		return nil
	}

	names := make([]ModName, 0, len(mods))
	for name := range byName {
		names = append(names, name)
	}
	sort.SliceStable(names, func(i, j int) bool {
		return order[names[i]] < order[names[j]]
	})
	for _, name := range names {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return out, nil
}
