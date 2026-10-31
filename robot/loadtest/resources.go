package loadtest

import (
	"runtime"
	"runtime/debug"
	runtimemetrics "runtime/metrics"
	"time"
)

// Resources 描述压测进程自身，不是服务端指标。采样堆峰值与进程生命期 RSS 高水位分开命名。
// CPUPercent 按一个核为 100% 计算；同进程跑其他任务时也会计入，容量测试应独占 Robot 进程。
type Resources struct {
	ElapsedSeconds        float64 `json:"elapsed_seconds"`
	CPUAvailable          bool    `json:"cpu_available"`
	CPUSeconds            float64 `json:"cpu_seconds"`
	CPUPercent            float64 `json:"cpu_percent"`
	ProcessPeakRSSBytes   uint64  `json:"process_peak_rss_bytes"`
	SampledPeakHeapBytes  uint64  `json:"sampled_peak_heap_bytes"`
	SampledPeakGoroutines int     `json:"sampled_peak_goroutines"`
	Samples               int     `json:"samples"`
}

type Environment struct {
	GoVersion       string `json:"go_version"`
	OS              string `json:"os"`
	Arch            string `json:"arch"`
	CPUs            int    `json:"cpus"`
	GOMAXPROCS      int    `json:"gomaxprocs"`
	MainModule      string `json:"main_module"`
	MainVersion     string `json:"main_version"`
	CoreVersion     string `json:"core_version"`
	CoreReplacement string `json:"core_replacement,omitempty"`
	VCSRevision     string `json:"vcs_revision,omitempty"`
	VCSModified     string `json:"vcs_modified,omitempty"`
}

func environment() Environment {
	result := Environment{GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(), GOMAXPROCS: runtime.GOMAXPROCS(0)}
	if info, ok := debug.ReadBuildInfo(); ok {
		result.MainModule, result.MainVersion = info.Main.Path, info.Main.Version
		for _, dep := range info.Deps {
			if dep.Path == "github.com/tjbdwanghaibo/roost-core" {
				result.CoreVersion = dep.Version
				if dep.Replace != nil {
					result.CoreReplacement = dep.Replace.Path + "@" + dep.Replace.Version
				}
			}
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				result.VCSRevision = setting.Value
			case "vcs.modified":
				result.VCSModified = setting.Value
			}
		}
	}
	return result
}

func monitorResources() func() Resources {
	start := time.Now()
	initialCPU, _, available := processUsage()
	stop := make(chan struct{})
	result := make(chan Resources, 1)
	go func() {
		stats := Resources{CPUAvailable: available}
		samples := []runtimemetrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
		sample := func() {
			runtimemetrics.Read(samples)
			stats.SampledPeakHeapBytes = max(stats.SampledPeakHeapBytes, samples[0].Value.Uint64())
			stats.SampledPeakGoroutines = max(stats.SampledPeakGoroutines, runtime.NumGoroutine())
			stats.Samples++
		}
		sample()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sample()
			case <-stop:
				sample()
				cpu, rss, ok := processUsage()
				stats.ElapsedSeconds = time.Since(start).Seconds()
				stats.ProcessPeakRSSBytes = rss
				stats.CPUAvailable = available && ok
				if stats.CPUAvailable {
					stats.CPUSeconds = max(0, cpu-initialCPU)
					stats.CPUPercent = 100 * stats.CPUSeconds / stats.ElapsedSeconds
				}
				result <- stats
				return
			}
		}
	}()
	return func() Resources { close(stop); return <-result }
}
