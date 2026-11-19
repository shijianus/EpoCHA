package m4baseline

import (
	"runtime"
	"runtime/metrics"
)

// HostInfo 是参考主机的完整规格。M4 的数字只对这台机器成立；换机器必须重跑，
// 不能把这里的 CPU/RSS 当成目标机（1 vCPU / 1GB）的稳态数字。
type HostInfo struct {
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	NumCPU        int    `json:"num_cpu"`
	GOMAXPROCS    int    `json:"gomaxprocs"`
	CPUModel      string `json:"cpu_model,omitempty"`
	TotalRAMBytes uint64 `json:"total_ram_bytes,omitempty"`
	GoVersion     string `json:"go_version"`
}

func currentHost() HostInfo {
	return HostInfo{
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		NumCPU:        runtime.NumCPU(),
		GOMAXPROCS:    runtime.GOMAXPROCS(0),
		CPUModel:      hostCPUModel(),
		TotalRAMBytes: hostTotalRAM(),
		GoVersion:     runtime.Version(),
	}
}

// Runtime estimates are retained for comparison only; Go does not define them as OS process CPU.
func runtimeUserEstimateSeconds() float64 {
	s := []metrics.Sample{{Name: "/cpu/classes/user:cpu-seconds"}}
	metrics.Read(s)
	if s[0].Value.Kind() == metrics.KindFloat64 {
		return s[0].Value.Float64()
	}
	return 0
}
