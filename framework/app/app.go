package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lifecycle"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	fctx "github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	flog "github.com/tjbdwanghaibo/roost-core/infra/observe/log"
	"github.com/tjbdwanghaibo/roost-core/internal/configfile"
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
	// settings 是 App 自己的配置（appConfig），启动检查（checkConfig）读出后供日志、单实例锁、业务时间与停机使用。
	settings appConfig
	// configLayers 是配置由哪些文件叠成（入口文件与 config.includes），--check-config 与启动日志写出它。
	configLayers configfile.Layers

	// signalSource exists to make shutdown sequencing testable on platforms
	// where a process cannot deliver os.Interrupt to itself (notably Windows).
	// Production leaves it nil and uses the process signal notifier below.
	signalSource func() (<-chan os.Signal, func())

	singletonOpener SingletonOpener
	singletonClock  singletonClock
	singleton       *singletonLock
	// logFrame 是文件 / 控制台日志在 Nest 请求之外取的帧号，Nest Mod 经 ModLogFrame 接上自己的 Ticker。
	logFrame LogFrame
	// businessTimeInterval 是运行中推进业务时间高水位的间隔；0 为 businessTimeAdvanceInterval（测试缩短它）。
	businessTimeInterval time.Duration
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
			// 走到这里说明命令行已解析成功，之后的失败是运行期错误：交给调用方（生成的 main 用结构化日志记录），
			// 不再由 cobra 追加 Usage 帮助和第二份 "Error:" 文本。命令行写错仍在此之前失败，照常打印 Usage。
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			if print, _ := cmd.Flags().GetBool("print-config"); print {
				return a.printConfig(serverType, cmd.OutOrStdout())
			}
			if print, _ := cmd.Flags().GetBool("print-config-schema"); print {
				return a.printConfigSchema(serverType, cmd.OutOrStdout())
			}
			if check, _ := cmd.Flags().GetBool("check-config"); check {
				if _, err := a.loadServiceConfig(serverType, true); err != nil {
					return err
				}
				if a.configLayers.Layered() {
					_, err := fmt.Fprintf(cmd.OutOrStdout(), "config ok: %s (includes: %s)\n", a.cfg.ConfigFileUsed(), strings.Join(a.configLayers.Includes, ", "))
					return err
				}
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "config ok: %s\n", a.cfg.ConfigFileUsed())
				return err
			}
			return a.run(serverType)
		},
	}
	// 配置按本服务全部 Mod 的声明检查（维护者决定）。--check-config 只加载并检查配置（与启动前的检查相同），
	// 不启动任何 Mod，用来在发布前检查真实的生产配置；--print-config 打印本服务全部声明（含业务 Mod）生成的配置段。
	cmd.Flags().Bool("check-config", false, "load and check the config against every mod's declaration, then exit")
	cmd.Flags().Bool("print-config", false, "print every config key this service's mods declare, with defaults and help, then exit")
	cmd.Flags().Bool("print-config-schema", false, "print this service's config declarations (app, mods and the service itself) as JSON, then exit; roost project doctor reads it")
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

// loadServiceConfig 读配置文件并按 App 与本服务全部 Mod 的声明检查（任何 Mod Init 之前），返回配置文件路径。
// requireFile 用于 --check-config：检查真实文件，不能把开发启动的缺省回退报告成文件检查成功。
func (a *App) loadServiceConfig(serverType ServiceName, requireFile bool) (string, error) {
	cfgPath, _ := a.rootCmd.Flags().GetString("config")
	explicitConfig := a.rootCmd.PersistentFlags().Changed("config")
	if cfgPath == "" {
		cfgPath = defaultServiceConfigPath(serverType)
	}
	sid, _ := a.rootCmd.Flags().GetInt("sid")

	a.cfg.SetDefault("sid", sid)
	a.cfg.SetDefault("server_type", serverType)

	// 入口文件与它的 config.includes 叠加层（规则只在 internal/configfile 一处，工具与 doctor 共用）。入口文件不存在时
	// 错误不包装，开发启动照旧退回缺省值；include 缺失或读不出总是错误。
	layers, err := configfile.Read(a.cfg, cfgPath)
	if err != nil {
		if requireFile || explicitConfig || !isMissingConfig(err) {
			return "", fmt.Errorf("read config %q: %w", cfgPath, err)
		}
		slog.Warn("default config file not found, using development defaults", "path", cfgPath, "err", err)
	}
	a.configLayers = layers
	a.cfg.Set("server_type", serverType)
	if a.rootCmd.Flags().Changed("sid") {
		a.cfg.Set("sid", sid)
	}
	settings, err := checkConfig(a.cfg, a.serviceDeclarations(serverType), a.serviceMods(serverType))
	if err != nil {
		// 叠加时错误补上键来自哪个文件。
		return "", layers.Annotate(err)
	}
	a.settings = settings
	return cfgPath, nil
}

func (a *App) printConfig(serverType ServiceName, out io.Writer) error {
	schema, err := a.ServiceConfigSchema(serverType)
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, schema.ReferenceYAML())
	return err
}

// printConfigSchema 把本服务的全部声明（含业务服务与业务 Mod 的）打印成 JSON：键表的每一项是 configschema.Key。
// 业务声明只有编译后的进程知道，`roost project doctor` 在工程里编译并运行它、读这份输出，
// 才能按业务声明检查工程里的配置文件。
func (a *App) printConfigSchema(serverType ServiceName, out io.Writer) error {
	schema, err := a.ServiceConfigSchema(serverType)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(schema.Keys)
}

// appRun 是一次 App.run 在启动、服务与停机各步骤之间共享的运行状态，只承载跨步骤的所有权与收尾判定。
//
//   - failureReturned：RuntimeFailure 是否已经进了返回值（启动检查点、或等待停止时的 Done 分支）。
//     没进的由 run 的 reportLateRuntimeFailure 在返回时并入。
//   - singletonReleasable / singletonReleaseDeadline：单实例锁 finish 的释放判定。singletonReleasable
//     只在“全部 Mod 都停完”的路径上置位（逐个返回点置位，漏掉的路径默认不释放，键在 TTL 内自然过期，
//     方向是安全的）；singletonReleaseDeadline 是停机路径 shutdownCtx 的截止时间。
//   - providedSharedMods / providedServiceMods：调过 Provide 的 Mod（含 Provide 失败的那个：它可能已部分登记），
//     启动失败与启动被打断时逆序停止它们。
//   - startedSharedMods / startedServiceMods：Start 成功返回的 Mod，停机第一步关它们的输入，正常停机逆序停止它们。
//   - initializedService：Init 已返回（不论成败）的 Service，收尾时调它的 Shutdown；nil 表示 Init 还没返回过。
//
// 启动阶段的 runStartupStep 返回 finished=false（超时或被信号打断）时阶段仍可能在跑：各步骤把它交给
// stopInterruptedStartup，按“先关输入、收到信号时等这一步、再停已开始的部分”收尾。
type appRun struct {
	serverType     ServiceName
	sigChan        <-chan os.Signal
	startupCtx     context.Context
	runtimeFailure *RuntimeFailure

	failureReturned bool

	singleton                *singletonLock
	singletonReleasable      bool
	singletonReleaseDeadline time.Time

	providedSharedMods  []Mod
	providedServiceMods []Mod
	startedSharedMods   []Mod
	startedServiceMods  []Mod
	initializedService  Service
}

func (a *App) run(serverType ServiceName) (runErr error) {
	// --- Load and check config: before any Mod Init ---
	cfgPath, err := a.loadServiceConfig(serverType, false)
	if err != nil {
		return err
	}
	// 进程级业务时钟（fctx.Now、框架库的缺省）在这里、启动时设一次，运行期不改；
	// 生产环境非 0 已被启动检查拒绝。Registry 的业务时钟读同一个键。
	clock.SetOffset(a.settings.Time.LogicOffset)
	// 进程级业务日历（clock.BusinessDay 等全局函数读它）同样只在这里设一次；非法时区已被启动检查拒绝。
	clock.SetCalendar(a.settings.Time.calendar)
	fctx.SetRuntimeConfig(a.cfg)
	if err := flog.Init(flog.Options{
		LevelText:        a.settings.Log.Level,
		JSON:             a.settings.Log.JSON,
		Stdout:           a.settings.Log.Stdout,
		File:             a.settings.Log.File,
		Dir:              a.settings.Log.Dir,
		Service:          string(serverType),
		Sid:              int(a.settings.Sid),
		Caller:           a.settings.Log.Caller,
		RotateInterval:   a.settings.Log.RotateInterval,
		RotateTimeFormat: a.settings.Log.RotateTimeFormat,
		FrameFunc:        a.logFrame.Current,
	}); err != nil {
		return fmt.Errorf("init log: %w", err)
	}
	// This App run owns the configured sink. Close it before returning so a
	// subsequent run can safely rotate or remove the same file on Windows.
	//
	// 关闭之前先把最终错误写进日志：生成的 main 在 run 返回、文件日志已关之后才打印
	// "server exit"，那一行只到 stderr；启动阶段失败（Mod Init / Provide / Start、单实例锁、业务时间检查）
	// 直接返回，文件里没有退出原因。这个 defer 最先登记、最后执行，runErr 已含停机期间并入的 RuntimeFailure。
	defer func() {
		if runErr != nil {
			slog.Error("app run failed", "err", runErr)
		}
		_ = flog.Close()
	}()

	slog.Info("starting server",
		"name", a.name,
		"version", a.version,
		"type", serverType,
		"sid", a.settings.Sid,
		"config", cfgPath,
		"config_includes", a.configLayers.Includes,
		"business_timezone", a.settings.Time.calendar.Location().String(),
		"business_day_start", a.settings.Time.BusinessDayStart,
	)

	// 从启动开始接收终止信号，后续 Serve 沿用同一个来源，不留信号处理空窗。
	sigChan, stopSignals := a.exitSignals()
	defer stopSignals()
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), a.settings.Startup.Timeout)
	defer cancelStartup()
	run := &appRun{serverType: serverType, sigChan: sigChan, startupCtx: startupCtx}

	// --- Init registry ---
	if err := a.initRegistry(run, cfgPath); err != nil {
		return err
	}
	run.runtimeFailure, _ = Lookup[*RuntimeFailure](a.registry, ModRuntimeFailure)
	// 这个 defer 登记在单实例锁的 finish 之前，因而在它之后执行，收尾期间的失败也看得到。
	defer run.reportLateRuntimeFailure(&runErr)

	// --- Singleton lock: before any Mod Init ---
	// 同一服务类型 + sid 只让一个进程跑 Mod（见 singleton.go）。
	// 收尾统一在 defer 里：先停续期，再按 singletonReleasable 决定是否 Release，最后关闭 store。
	singleton, err := a.openSingleton(serverType)
	if err != nil {
		return err
	}
	if singleton != nil {
		run.singleton = singleton
		defer run.finishSingleton()
		slog.Info("singleton: acquiring", "key", singleton.key)
		if err := singleton.acquire(sigChan); err != nil {
			if errors.Is(err, errSingletonWaitInterrupted) {
				return nil
			}
			return err
		}
		slog.Info("singleton: acquired", "key", singleton.key, "value", string(singleton.value))
		singleton.startRenewal()
	}

	// --- Business time only moves forward: before any Mod Init ---
	// 业务时间高水位（见 business_time.go）：按新偏移算出的业务时间早于
	// 这套部署到过的时刻就拒绝启动。它可能与单实例锁共用存储，所以 stop 登记在锁的 finish 之后、先于它执行。
	businessTime, err := a.startBusinessTimeGuard(serverType, singleton)
	if err != nil {
		run.singletonReleasable = true
		return err
	}
	defer businessTime.stop()

	// --- Mods lifecycle: Init → Provide → Start ---
	sharedMods, err := sortMods(a.mods, nil)
	if err != nil {
		run.singletonReleasable = true
		return err
	}
	if err := a.startSharedMods(run, sharedMods); err != nil {
		return err
	}
	entry, serviceMods, err := a.resolveServiceEntry(run, sharedMods)
	if err != nil {
		return err
	}
	if err := a.startServiceMods(run, serviceMods); err != nil {
		return err
	}
	if err := a.confirmModsStarted(run); err != nil {
		return err
	}

	// --- Service lifecycle: Init → Serve ---
	svc := entry.svc
	if err := a.initService(run, svc); err != nil {
		return err
	}
	cancelStartup()
	return a.serveAndShutdown(run, svc)
}

// initRegistry 建立本次运行的 Registry 并发布 app.init。
func (a *App) initRegistry(run *appRun, cfgPath string) error {
	a.registry = NewRegistry(a.cfg)
	a.registry.store[ModLogFrame] = &a.logFrame
	finished, err, pending := runStartupStep(run.startupCtx, run.sigChan, "app.init", func(ctx context.Context) error {
		return a.emitLifecycle(ctx, lifecycle.Event{
			Phase:   lifecycle.PhaseAppInit,
			Service: string(run.serverType),
			Name:    a.name,
			Data: map[string]any{
				"sid":    int(a.settings.Sid),
				"config": cfgPath,
			},
		})
	})
	if !finished {
		return a.stopInterruptedStartup(run, err, pending, nil)
	}
	return err
}

// reportLateRuntimeFailure 在 run 返回时并入没进返回值的 RuntimeFailure——停机开始之后才失败、
// 启动失败收尾期间才失败：返回值是进程退出码与部署脚本看得到的唯一信号，fail-stop 不能以 0 退出。
// 围栏、失锁不 Release 这些安全动作由 RuntimeFailure / 单实例锁自己完成，这里只补报告。
func (run *appRun) reportLateRuntimeFailure(runErr *error) {
	if run.failureReturned {
		return
	}
	if err := run.runtimeFailure.Err(); err != nil {
		*runErr = errors.Join(*runErr, fmt.Errorf("app: runtime failure after shutdown began: %w", err))
	}
}

// startupFailure 在启动各阶段之间检查：启动期间（例如 DataEngine 重放很长时）发生的 fail-stop
// （失锁、DataEngine / Remote fatal）不等进入 Serve 的 select，按启动失败路径停掉已启动的 Mod。
func (run *appRun) startupFailure() error {
	if err := run.runtimeFailure.Err(); err != nil {
		run.failureReturned = true
		return fmt.Errorf("app: runtime failure during startup: %w", err)
	}
	return nil
}

// finishSingleton 停续期，按 singletonReleasable 决定是否 Release，最后关闭 store。
func (run *appRun) finishSingleton() {
	run.singleton.finish(run.singletonReleasable, run.singletonReleaseDeadline)
}

// cleanupAfterStartupFailure 是启动失败（某一步返回错误、启动检查点发现 fail-stop）的收尾，与停机同一顺序：
// 先关已启动部分的输入并排空（closeIngress），再调 Service.Shutdown（Init 已返回过时），最后逆序停已 Provide 的
// 服务专属 Mod、共享 Mod，全部停完才允许释放单实例锁。任何一步没在时限内结束都保留后面的依赖与锁。
// 返回收尾中的错误（并入启动错误）。
func (a *App) cleanupAfterStartupFailure(run *appRun) error {
	ctx, cancel := context.WithTimeout(context.Background(), a.settings.Shutdown.TotalTimeout)
	defer cancel()
	drained, err := run.closeIngress(ctx, a.registry, run.initializedService)
	if !drained {
		return err
	}
	if svc := run.initializedService; svc != nil {
		stopped, cleanupErr := shutdownAfterStartupFailure(svc)
		err = errors.Join(err, cleanupErr)
		if !stopped {
			// Service 的组件可能还在用 Mod：不停 Mod、不释放锁（singletonReleasable 保持 false），
			// 进程退出后键在 TTL 内过期。
			return err
		}
	}
	run.singletonReleasable = run.stopProvidedMods()
	return err
}

// stopProvidedMods 逆序停已 Provide 的服务专属 Mod、再停共享 Mod（无总截止，每个 Mod 用自己的默认时长），
// 返回是否全部停完（决定能否释放单实例锁）。
func (run *appRun) stopProvidedMods() bool {
	return allStopped(
		stopModsReverse(run.providedServiceMods, "mod stop (service-specific)"),
		stopModsReverse(run.providedSharedMods, "mod stop"))
}

// stopInterruptedStartup 是启动阶段没在等待中结束（pending 仍在跑）时的收尾，与停机同一套顺序，只停已经开始的部分
// （App 生命周期方案 §3.5）：
//
//  1. 停机预算（shutdown.total_timeout）开始计时，关已启动部分的输入并排空：在途请求答完，之后的答“没执行”。
//  2. startup.timeout 到期（不是信号）：那一步已经超过自己的预算、多半卡住，不再等它，保留依赖与锁返回（待决定 D2）。
//  3. 收到信号：在停机预算内等那一步返回，onDone 按它是什么补登记（Provide / Start 返回了算已 Provide / 已 Start，
//     Service.Init 返回了算 Service 已初始化）；再关一次输入（这一步期间新启动、新登记的）。
//  4. Service 已初始化 → Service.Shutdown；然后按停机预算逆序停已 Provide 的服务专属 Mod、共享 Mod，全部停完才释放锁。
//
// 任何一步没在预算内结束都保留依赖与锁返回（与正常停机相同）。返回值是打断原因（带 context.Canceled / DeadlineExceeded）
// 并入收尾中的错误。不发布 service.stopping / service.stopped（service.started 没有发布过）。
func (a *App) stopInterruptedStartup(run *appRun, cause error, pending *startupStep, onDone func(stepErr error)) error {
	total := a.settings.Shutdown.TotalTimeout
	ctx, cancel := context.WithTimeout(context.Background(), total)
	defer cancel()
	result := cause
	join := func(err error) {
		if err != nil {
			result = errors.Join(result, err)
		}
	}
	step, bySignal := "", false
	if pending != nil {
		step, bySignal = pending.name, pending.bySignal
	}
	slog.Warn("startup interrupted; closing the inputs of what has started", "step", step, "signal", bySignal, "budget", total)
	drained, err := run.closeIngress(ctx, a.registry, run.initializedService)
	join(err)
	if !drained || !bySignal {
		return result
	}
	select {
	case stepErr := <-pending.result:
		if onDone != nil {
			onDone(stepErr)
		}
		if stepErr != nil {
			join(fmt.Errorf("app: startup %s: %w", pending.name, stepErr))
		}
	case <-ctx.Done():
		join(fmt.Errorf("app: startup %s did not return within shutdown.total_timeout after the interruption; keeping its dependencies: %w", pending.name, ctx.Err()))
		return result
	}
	drained, err = run.closeIngress(ctx, a.registry, run.initializedService)
	join(err)
	if !drained {
		return result
	}
	if svc := run.initializedService; svc != nil {
		serviceCtx, serviceCancel := context.WithCancel(ctx)
		stopped, err := shutdownServiceWithin(serviceCtx, svc)
		serviceCancel()
		join(err)
		if !stopped {
			return result
		}
	}
	modStopCtx, modStopCancel := run.modStopContext(ctx, total)
	defer modStopCancel()
	if err := stopModsReverseBefore(modStopCtx, run.providedServiceMods, run.providedSharedMods, "mod stop (service-specific)"); err != nil {
		join(err)
		if stopIncomplete(err) {
			return result
		}
	}
	if err := stopModsReverseWithContext(modStopCtx, run.providedSharedMods, "mod stop"); err != nil {
		join(err)
		run.singletonReleasable = !stopIncomplete(err)
	} else {
		run.singletonReleasable = true
	}
	slog.Info("startup interrupted; stopped what had started", "type", run.serverType)
	return result
}

// startSharedMods 对共享 Mod 依次执行 Init → Provide → Start。Provide / Start 失败时按启动失败收尾（逆序停止
// 已 Provide 的共享 Mod，Provide 失败的那个也停，它可能已部分登记）。
func (a *App) startSharedMods(run *appRun, sharedMods []Mod) error {
	for _, mod := range sharedMods {
		slog.Info("mod init", "mod", mod.Name())
		finished, err, pending := runStartupStep(run.startupCtx, run.sigChan, "mod init "+string(mod.Name()), func(context.Context) error { return mod.Init(a.cfg) })
		if !finished {
			return a.stopInterruptedStartup(run, err, pending, nil)
		}
		if err != nil {
			run.singletonReleasable = true
			return fmt.Errorf("mod %s init: %w", mod.Name(), err)
		}
	}
	for _, mod := range sharedMods {
		finished, err, pending := runStartupStep(run.startupCtx, run.sigChan, "mod provide "+string(mod.Name()), func(context.Context) error { return mod.Provide(a.registry) })
		if !finished {
			return a.stopInterruptedStartup(run, err, pending, func(error) { run.providedSharedMods = append(run.providedSharedMods, mod) })
		}
		run.providedSharedMods = append(run.providedSharedMods, mod)
		if err != nil {
			return errors.Join(fmt.Errorf("mod %s provide: %w", mod.Name(), err), a.cleanupAfterStartupFailure(run))
		}
	}
	for _, mod := range sharedMods {
		if err := run.startupFailure(); err != nil {
			return errors.Join(err, a.cleanupAfterStartupFailure(run))
		}
		slog.Info("mod start", "mod", mod.Name())
		finished, err, pending := runStartupStep(run.startupCtx, run.sigChan, "mod start "+string(mod.Name()), func(context.Context) error { return mod.Start() })
		if !finished {
			return a.stopInterruptedStartup(run, err, pending, func(stepErr error) {
				if stepErr == nil {
					run.startedSharedMods = append(run.startedSharedMods, mod)
				}
			})
		}
		if err != nil {
			return errors.Join(fmt.Errorf("mod %s start: %w", mod.Name(), err), a.cleanupAfterStartupFailure(run))
		}
		run.startedSharedMods = append(run.startedSharedMods, mod)
	}
	return nil
}

// resolveServiceEntry 找到本服务类型的入口，并对它的专属 Mod 排序（共享 Mod 视为已满足的依赖）。
// 失败时共享 Mod 已全部启动，按启动失败收尾。
func (a *App) resolveServiceEntry(run *appRun, sharedMods []Mod) (*serviceEntry, []Mod, error) {
	entry, ok := a.services[run.serverType]
	if !ok {
		return nil, nil, errors.Join(fmt.Errorf("unknown server type: %s", run.serverType), a.cleanupAfterStartupFailure(run))
	}
	sharedNames := make(map[ModName]struct{}, len(sharedMods))
	for _, mod := range sharedMods {
		sharedNames[mod.Name()] = struct{}{}
	}
	serviceMods, err := sortMods(entry.mods, sharedNames)
	if err != nil {
		return nil, nil, errors.Join(err, a.cleanupAfterStartupFailure(run))
	}
	return entry, serviceMods, nil
}

// startServiceMods 对服务专属 Mod 依次执行 Init → Provide → Start。失败时按启动失败收尾：先关已启动部分的输入，
// 再逆序停已 Provide 的服务专属 Mod（含 Provide 失败的那个），再停共享 Mod。
func (a *App) startServiceMods(run *appRun, serviceMods []Mod) error {
	for _, mod := range serviceMods {
		slog.Info("mod init (service-specific)", "mod", mod.Name())
		finished, err, pending := runStartupStep(run.startupCtx, run.sigChan, "mod init "+string(mod.Name()), func(context.Context) error { return mod.Init(a.cfg) })
		if !finished {
			return a.stopInterruptedStartup(run, err, pending, nil)
		}
		if err != nil {
			return errors.Join(fmt.Errorf("mod %s init: %w", mod.Name(), err), a.cleanupAfterStartupFailure(run))
		}
	}
	for _, mod := range serviceMods {
		finished, err, pending := runStartupStep(run.startupCtx, run.sigChan, "mod provide "+string(mod.Name()), func(context.Context) error { return mod.Provide(a.registry) })
		if !finished {
			return a.stopInterruptedStartup(run, err, pending, func(error) { run.providedServiceMods = append(run.providedServiceMods, mod) })
		}
		run.providedServiceMods = append(run.providedServiceMods, mod)
		if err != nil {
			return errors.Join(fmt.Errorf("mod %s provide: %w", mod.Name(), err), a.cleanupAfterStartupFailure(run))
		}
	}
	for _, mod := range serviceMods {
		if err := run.startupFailure(); err != nil {
			return errors.Join(err, a.cleanupAfterStartupFailure(run))
		}
		slog.Info("mod start (service-specific)", "mod", mod.Name())
		finished, err, pending := runStartupStep(run.startupCtx, run.sigChan, "mod start "+string(mod.Name()), func(context.Context) error { return mod.Start() })
		if !finished {
			return a.stopInterruptedStartup(run, err, pending, func(stepErr error) {
				if stepErr == nil {
					run.startedServiceMods = append(run.startedServiceMods, mod)
				}
			})
		}
		if err != nil {
			return errors.Join(fmt.Errorf("mod %s start: %w", mod.Name(), err), a.cleanupAfterStartupFailure(run))
		}
		run.startedServiceMods = append(run.startedServiceMods, mod)
	}
	return nil
}

// confirmModsStarted 在全部 Mod 启动后再查一次 RuntimeFailure，然后发布 mods.started。
func (a *App) confirmModsStarted(run *appRun) error {
	if err := run.startupFailure(); err != nil {
		return errors.Join(err, a.cleanupAfterStartupFailure(run))
	}
	finished, err, pending := runStartupStep(run.startupCtx, run.sigChan, "mods.started", func(ctx context.Context) error {
		return a.emitLifecycle(ctx, lifecycle.Event{
			Phase:   lifecycle.PhaseModsStarted,
			Service: string(run.serverType),
			Name:    a.name,
		})
	})
	if !finished {
		return a.stopInterruptedStartup(run, err, pending, nil)
	}
	if err != nil {
		return errors.Join(err, a.cleanupAfterStartupFailure(run))
	}
	return nil
}

// initService 执行 Service.Init、再查一次 RuntimeFailure，然后发布 service.started。
// Init 失败与 Init 之后的启动失败走同一条收尾（cleanupAfterStartupFailure）：先关输入，再收回 Service 已启动的部分
// （Init 返回错误时同样调 Shutdown：Init 可能已经起了后台循环、订阅），再停 Mod、释放锁；Service 在时限内停不下来
// 就保留 Mod 与锁，与正常停机相同。
func (a *App) initService(run *appRun, svc Service) error {
	slog.Info("service init", "service", svc.Name())
	finished, startErr, pending := runStartupStep(run.startupCtx, run.sigChan, "service init "+string(svc.Name()), func(context.Context) error { return svc.Init(a.registry) })
	if !finished {
		return a.stopInterruptedStartup(run, startErr, pending, func(error) { run.initializedService = svc })
	}
	run.initializedService = svc
	if startErr != nil {
		startErr = fmt.Errorf("service %s init: %w", svc.Name(), startErr)
	} else {
		startErr = run.startupFailure()
	}
	if startErr == nil {
		finished, startErr, pending = runStartupStep(run.startupCtx, run.sigChan, "service.started", func(ctx context.Context) error {
			return a.emitLifecycle(ctx, lifecycle.Event{
				Phase:   lifecycle.PhaseServiceStarted,
				Service: string(run.serverType),
				Name:    string(svc.Name()),
			})
		})
		if !finished {
			return a.stopInterruptedStartup(run, startErr, pending, nil)
		}
	}
	if startErr != nil {
		return errors.Join(startErr, a.cleanupAfterStartupFailure(run))
	}
	return nil
}

// serveAndShutdown 在后台运行 Serve，等到终止信号、RuntimeFailure 或 Serve 自行退出，然后按停机预算
// 收尾。返回值合并 Serve 自身的错误与停机过程的错误。
func (a *App) serveAndShutdown(run *appRun, svc Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- svc.Serve(ctx)
	}()
	serviceErr, serveDone := run.waitForStop(serveErr)
	cancel()
	return a.shutdown(run, svc, serveErr, serveDone, serviceErr)
}

// waitForStop 等终止信号、fail-stop 基础设施失败或 Serve 退出，返回 Serve 的错误与 Serve 是否已返回。
func (run *appRun) waitForStop(serveErr <-chan error) (serviceErr error, serveDone bool) {
	select {
	case sig := <-run.sigChan:
		slog.Info("received signal, shutting down", "signal", sig)
	case err := <-run.runtimeFailure.Done():
		slog.Error("runtime infrastructure failure, shutting down", "err", err)
		serviceErr = errors.Join(serviceErr, err)
		run.failureReturned = true
	case err := <-serveErr:
		serveDone = true
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("service exited with error", "err", err)
			serviceErr = err
		}
	}
	return serviceErr, serveDone
}

// shutdown 是优雅停机：发布 service.stopping → 关全部外部输入并排空（closeIngress）→ 等 Serve 与
// Service.Shutdown 返回 → 逆序停服务专属 Mod、再停共享 Mod（决定单实例锁能否释放）→ 发布 service.stopped。
// 任何一步在预算内没结束都直接返回，保留仍可能被访问的依赖与锁。
func (a *App) shutdown(run *appRun, svc Service, serveErr <-chan error, serveDone bool, serviceErr error) error {
	shutdownTimeout := a.settings.Shutdown.TotalTimeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()
	modStopCtx, modStopCancel := run.modStopContext(shutdownCtx, shutdownTimeout)
	defer modStopCancel()

	slog.Info("service shutdown", "service", svc.Name())
	var shutdownErr error
	stoppingFinished, err := a.emitLifecycleWithin(shutdownCtx, lifecycle.Event{
		Phase:   lifecycle.PhaseServiceStopping,
		Service: string(run.serverType),
		Name:    string(svc.Name()),
	})
	if err != nil {
		shutdownErr = errors.Join(shutdownErr, err)
	}
	if !stoppingFinished {
		// service.stopping 的 hook 在停机预算内没返回，可能还在用 Service 与 Mod：和 Service.Shutdown
		// 不完整相同，不调 Shutdown、不停 Mod、不释放单实例锁。
		return errors.Join(serviceErr, shutdownErr)
	}
	// 停机第一步：关全部外部输入并排空（ingress.go）。之后才停业务与 Mod；排空没完成就保留依赖与锁。
	drained, err := run.closeIngress(shutdownCtx, a.registry, svc)
	if err != nil {
		shutdownErr = errors.Join(shutdownErr, err)
	}
	if !drained {
		return errors.Join(serviceErr, shutdownErr)
	}
	serviceErr, shutdownErr, complete := awaitServeAndShutdown(shutdownCtx, svc, serveErr, serveDone, serviceErr, shutdownErr)
	if !complete {
		// Dependencies must remain alive while Serve or Shutdown can still
		// access them. Returning without stopping mods is safer than racing
		// an uncooperative service during process termination.
		// 单实例锁同理不释放（singletonReleasable 保持 false）。
		return errors.Join(serviceErr, shutdownErr)
	}

	// Stop service-specific mods in reverse order
	// 共享 Mod 在服务专属 Mod 之后停止，但共用同一个总时限：规划时把它们声明的预算算进去。
	if err := stopModsReverseBefore(modStopCtx, run.startedServiceMods, run.startedSharedMods, "mod stop (service-specific)"); err != nil {
		shutdownErr = errors.Join(shutdownErr, err)
		if stopIncomplete(err) {
			// A service-specific Mod may still use shared capabilities. Preserve
			// those dependencies until the process terminates.
			return errors.Join(serviceErr, shutdownErr)
		}
	}

	// Stop shared mods in reverse order
	if err := stopModsReverseWithContext(modStopCtx, run.startedSharedMods, "mod stop"); err != nil {
		shutdownErr = errors.Join(shutdownErr, err)
		run.singletonReleasable = !stopIncomplete(err)
	} else {
		run.singletonReleasable = true
	}

	slog.Info("server stopped", "type", run.serverType)
	// Mod 已经停完，singletonReleasable 已按 Mod 的结果定好；这里卡住的 hook 只让 run 按预算返回
	// （剩余时间不够时 Release 跳过，键在 ttl 内过期），不改变释放规则。
	if _, err := a.emitLifecycleWithin(shutdownCtx, lifecycle.Event{
		Phase:   lifecycle.PhaseServiceStopped,
		Service: string(run.serverType),
		Name:    string(svc.Name()),
	}); err != nil {
		shutdownErr = errors.Join(shutdownErr, err)
	}
	return errors.Join(serviceErr, shutdownErr)
}

// modStopContext 返回 Mod 停机用的 ctx。启用单实例锁时，截止时间比 shutdownCtx 提前 releaseReserve，
// 留给全部 Mod 停完之后的 Release：stopModsReverseBefore 会把截止前的剩余时间全部分给 Mod。预留不超过
// 总时长的一半，避免很小的 total_timeout 被预留吃光（codegen 会把 Release 的 3s 计入 total_timeout）。
// 此刻已 Lost（失锁触发的 fail-stop）就不会 Release，不预留，整段时间留给 Mod 停机；停机期间才失锁的，
// 预留照旧，只是用不上。未启用单实例锁时直接用 shutdownCtx。
func (run *appRun) modStopContext(shutdownCtx context.Context, shutdownTimeout time.Duration) (context.Context, context.CancelFunc) {
	if run.singleton == nil {
		return shutdownCtx, func() {}
	}
	deadline, _ := shutdownCtx.Deadline()
	run.singletonReleaseDeadline = deadline
	releaseReserve := min(singletonReleaseBudget, shutdownTimeout/2)
	if run.singleton.snapshot().state == singletonLost {
		releaseReserve = 0
	}
	return context.WithDeadline(shutdownCtx, deadline.Add(-releaseReserve))
}

// awaitServeAndShutdown 调用 Service.Shutdown，并等 Serve 与 Shutdown 都返回；两者的错误分别并入
// serviceErr 与 shutdownErr。complete 为 false 表示停机预算内没等到：Serve 或 Shutdown 仍可能访问依赖，
// 调用方不停 Mod、不释放单实例锁。
func awaitServeAndShutdown(shutdownCtx context.Context, svc Service, serveErr <-chan error, serveDone bool, serviceErr, shutdownErr error) (error, error, bool) {
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
			return serviceErr, shutdownErr, false
		}
	}
	return serviceErr, shutdownErr, true
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

// startupCleanupTimeout 是启动失败时留给 Service.Shutdown 收回已启动部分的时长。
const startupCleanupTimeout = 5 * time.Second

// shutdownAfterStartupFailure 在启动失败时调用 Service.Shutdown，收回 Init 已经启动的部分
// （Init 返回错误时同样调用：Init 可能已经起了后台循环、订阅，App 不知道是哪些）。Shutdown 必须
// 容忍部分初始化。stopped 为 false 表示 Shutdown 没在 startupCleanupTimeout 内结束（包括不配合
// ctx、按 ctx 超时返回、panic）：调用方保留 Mod 与单实例锁，和正常停机“Shutdown 不完整就保留
// 依赖”的规则一致；Shutdown 返回其他错误视为已结束，错误并入启动错误。
func shutdownAfterStartupFailure(svc Service) (stopped bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), startupCleanupTimeout)
	defer cancel()
	return shutdownServiceWithin(ctx, svc)
}

// shutdownServiceWithin 在 ctx 内调用 Service.Shutdown 并等它返回（启动收尾用，规则见 shutdownAfterStartupFailure）。
func shutdownServiceWithin(ctx context.Context, svc Service) (stopped bool, err error) {
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

// stopModsReverse 是启动失败路径的逆序停止（无总截止时间，每个 Mod 用自己的默认时长）。
// 返回是否全部停完：某个 Mod 停机超时会中断后续停止、保留它们的依赖，此时返回 false，
// 调用方据此不释放单实例锁。Mod 返回普通错误仍算停完。
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
// 只把它们声明的预算计入规划：先停的一段必须给后面声明了预算的 Mod 留出时间。
// later 中未声明预算的 Mod 不参与这一段的均分。
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

// undeclaredModStopFloor 是有总截止时间时每个未声明预算的 Mod 的固定保底：
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
// 这一段的规划）。每停一个 Mod 重新规划一次，前面 Mod 提前结束省下的时间自然留给后面。
//
// 规则（维护者批准）：
//   - 保底之和 = 本段未声明 Mod 数 x undeclaredModStopFloor；声明预算的上限 = remaining − 保底之和。
//   - 声明值之和不超过上限：声明的 Mod 拿到声明值，未声明的 Mod 均分 remaining − 声明值之和。
//   - 超过上限：声明值按“上限 / 声明值之和”缩放，未声明的 Mod 各拿保底；保底不参与缩放。
//   - remaining 不足以给参与规划的每个 Mod 一份保底：全部均分 remaining（声明的 Mod 也一样）。
//
// 没有任何 Mod 声明预算时三种情形都退化为“remaining / 本段剩余 Mod 数”。
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

// emitLifecycleWithin 是停机阶段（service.stopping / service.stopped）的 hook 派发：在 ctx（停机总预算）
// 内等全部 hook 返回。hook 不配合 ctx 时 App 杀不掉它——到期返回 finished=false 与 ctx 错误，hook 留在
// 自己的 goroutine 里继续跑，调用方按“停机不完整”保留依赖；与 Service.Shutdown、Mod 停机的处理相同
// （roost-coding 三步停机②）。到期的错误点出卡住的 hook（最后一个开始、还没返回的，
// 它之后的 hook 没有运行），run 的 “app run failed” 日志带上它。hook 自己返回的错误
// （包括配合 ctx 返回的 ctx 错误）算已返回，finished=true。启动阶段的 hook 没有停机预算，仍同步派发。
func (a *App) emitLifecycleWithin(ctx context.Context, event lifecycle.Event) (finished bool, err error) {
	var running atomic.Pointer[string]
	result := make(chan error, 1)
	go func() {
		result <- a.emitStopLifecycle(ctx, event, func(hook string) { running.Store(&hook) })
	}()
	select {
	case err := <-result:
		return true, err
	case <-ctx.Done():
		select {
		case err := <-result: // 与到期同时返回的，按已返回算
			return true, err
		default:
		}
		if hook := running.Load(); hook != nil {
			return false, fmt.Errorf("app: lifecycle %s hook %q did not return within the shutdown budget: %w", event.Phase, *hook, ctx.Err())
		}
		return false, fmt.Errorf("app: lifecycle %s hooks did not return within the shutdown budget: %w", event.Phase, ctx.Err())
	}
}

func (a *App) emitLifecycle(ctx context.Context, event lifecycle.Event) error {
	if event.Phase == lifecycle.PhaseServiceStopping || event.Phase == lifecycle.PhaseServiceStopped {
		return a.emitStopLifecycle(ctx, event, nil)
	}
	reg, err := a.lifecycleRegistry()
	if err != nil {
		return err
	}
	return reg.Emit(ctx, event)
}

// emitStopLifecycle 派发停机阶段的 hook：一个失败不挡住后面的（EmitAll），starting 记下正在运行的 hook。
func (a *App) emitStopLifecycle(ctx context.Context, event lifecycle.Event, starting func(hook string)) error {
	reg, err := a.lifecycleRegistry()
	if err != nil {
		return err
	}
	return reg.EmitAllWatched(ctx, event, starting)
}

func (a *App) lifecycleRegistry() (*lifecycle.Registry, error) {
	if a == nil || a.registry == nil {
		return nil, fmt.Errorf("app: lifecycle registry unavailable")
	}
	reg, ok := Lookup[*lifecycle.Registry](a.registry, ModLifecycle)
	if !ok || reg == nil {
		return nil, fmt.Errorf("app: capability %q not found or wrong type", ModLifecycle)
	}
	return reg, nil
}

func sortMods(mods []Mod, external map[ModName]struct{}) ([]Mod, error) {
	// 单 Mod 也必须校验名字、依赖和环，只有空集合可直接返回。
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
		// 服务专属 Mod 可以依赖已启动的共享 Mod，但不能以同名覆盖它的生命周期身份。
		if _, exists := external[name]; exists {
			return nil, fmt.Errorf("duplicate mod %q: already registered as a shared mod", name)
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
